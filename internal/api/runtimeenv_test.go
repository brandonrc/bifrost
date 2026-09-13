package api

import (
	"strings"
	"testing"
)

// TestRuntimeEnvValidate covers the #53 rule set: the default governed
// policy refuses every supply-chain/egress shape the epic identified,
// each denial has its policy escape, and a legal governed document passes
// verbatim.
func TestRuntimeEnvValidate(t *testing.T) {
	cases := []struct {
		name   string
		policy RuntimeEnvPolicy
		yaml   string
		deny   string // substring of the refusal; "" = admitted
	}{
		// --- empty / shape ---
		{name: "empty document", yaml: ""},
		{name: "whitespace only", yaml: "   \n  "},
		{name: "null document", yaml: "null"},
		{name: "empty mapping", yaml: "{}"},
		{name: "malformed yaml", yaml: "pip: [torch==2.1.0", deny: "not a valid YAML"},
		{name: "top-level list", yaml: "- torch==2.1.0", deny: "not a valid YAML"},
		{name: "top-level scalar", yaml: "just a string", deny: "not a valid YAML"},

		// --- field allowlist ---
		{name: "py_executable denied", yaml: "py_executable: /bin/sh", deny: `field "py_executable" is denied`},
		{name: "py_executable enabled", policy: RuntimeEnvPolicy{AllowPyExecutable: true}, yaml: "py_executable: /usr/bin/python3.11"},
		{name: "image_uri denied", yaml: "image_uri: evil/worker:latest", deny: `field "image_uri" is denied`},
		{name: "image_uri enabled", policy: RuntimeEnvPolicy{AllowImageURI: true}, yaml: "image_uri: registry.example.com/ml/worker:1.0"},
		{name: "conda denied", yaml: "conda:\n  dependencies: [numpy]", deny: `field "conda" is denied`},
		{name: "conda enabled", policy: RuntimeEnvPolicy{AllowConda: true}, yaml: "conda:\n  dependencies: [numpy]"},
		{name: "container field has no escape", policy: RuntimeEnvPolicy{AllowImageURI: true, AllowPyExecutable: true},
			yaml: "container:\n  image: evil/x", deny: `field "container" is not in the governance allowlist`},
		{name: "uv channel closed", yaml: "uv: [torch==2.1.0]", deny: `field "uv" is not in the governance allowlist`},
		{name: "unknown field", yaml: "wat: 1", deny: `field "wat" is not in the governance allowlist`},

		// --- pip list form: pinning ---
		{name: "pinned packages", yaml: "pip:\n  - torch==2.1.0\n  - ray[data]==2.56.0\n  - numpy==1.26.4+1"},
		{name: "unpinned bare name", yaml: "pip: [torch]", deny: `pip entry "torch" is not pinned`},
		{name: "range specifier", yaml: "pip: [torch>=2]", deny: "is not pinned"},
		{name: "wildcard version", yaml: "pip: [torch==2.1.*]", deny: "is not pinned"},
		{name: "direct reference", yaml: "pip: [" + `"pkg @ https://evil.example/pkg.whl"]`, deny: "not a package requirement"},
		{name: "marker suffix", yaml: `pip: ['torch==2.1.0; python_version<"3.12"']`, deny: "is not pinned"},
		{name: "unpinned allowed by policy", policy: RuntimeEnvPolicy{AllowUnpinnedPackages: true}, yaml: "pip:\n  - torch\n  - numpy>=1.0"},
		{name: "non-string pip entry", yaml: "pip: [42]", deny: "pip entries must be strings"},
		{name: "empty pip entry", yaml: `pip: [""]`, deny: "not a package requirement"},
		{name: "bare url entry", yaml: "pip: [https://evil.example/x.whl]", deny: "not a package requirement"},

		// --- package denylist ---
		{name: "denylisted pinned package", policy: RuntimeEnvPolicy{DeniedPackages: []string{"pwned"}},
			yaml: "pip: [pwned==1.0]", deny: `pip package "pwned" is on the platform denylist`},
		{name: "denylist normalized", policy: RuntimeEnvPolicy{DeniedPackages: []string{"Back-Stab.Der"}},
			yaml: "pip: [back_stab-der==1.0]", deny: "platform denylist"},
		{name: "denylist applies to unpinned too", policy: RuntimeEnvPolicy{DeniedPackages: []string{"pwned"}, AllowUnpinnedPackages: true},
			yaml: "pip: [PWNED]", deny: "platform denylist"},
		{name: "denylist with extras", policy: RuntimeEnvPolicy{DeniedPackages: []string{"pwned"}},
			yaml: `pip: ["pwned[cli]==1.0"]`, deny: "platform denylist"},

		// --- pip object form ---
		{name: "pip object with pinned packages", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_check: true\n  pip_version: ==24.0"},
		{name: "pip object unpinned", yaml: "pip:\n  packages: [torch]", deny: "is not pinned"},
		{name: "pip object unknown key", yaml: "pip:\n  packages: [torch==2.1.0]\n  extra_key: 1", deny: `pip key "extra_key" is not governed`},
		{name: "pip object packages not a list", yaml: "pip:\n  packages: torch==2.1.0", deny: "pip.packages must be a list"},
		{name: "pip scalar form", yaml: "pip: torch==2.1.0", deny: "pip must be a list of pinned packages"},

		// --- pip_install_options ---
		{name: "index-url split token", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--index-url, https://evil.example/simple]",
			deny: `"--index-url" redirects where packages come from`},
		{name: "index-url equals form", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--index-url=https://evil.example/simple]",
			deny: `"--index-url" redirects`},
		{name: "short -i", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [-i, https://evil.example/simple]",
			deny: `"-i" redirects`},
		{name: "extra-index-url", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--extra-index-url, https://evil.example/simple]",
			deny: `"--extra-index-url" redirects`},
		{name: "find-links", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--find-links, /srv/wheels]",
			deny: `"--find-links" redirects`},
		{name: "pre widens what an index serves", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--pre]",
			deny: `"--pre" redirects`},
		{name: "harmless options pass", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--no-cache-dir, --no-compile]"},
		{name: "allowed index host", policy: RuntimeEnvPolicy{AllowedIndexHosts: []string{"pypi.corp.example"}},
			yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--index-url, https://pypi.corp.example/simple, --pre]"},
		{name: "allowed index host case-insensitive with port", policy: RuntimeEnvPolicy{AllowedIndexHosts: []string{"pypi.corp.example"}},
			yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--index-url=https://PYPI.corp.example:8443/simple]"},
		{name: "index host not in allowlist", policy: RuntimeEnvPolicy{AllowedIndexHosts: []string{"pypi.corp.example"}},
			yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--extra-index-url, https://evil.example/simple]",
			deny: "not one of the administrator's allowed index hosts"},
		{name: "index option value missing", policy: RuntimeEnvPolicy{AllowedIndexHosts: []string{"pypi.corp.example"}},
			yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [--index-url]",
			deny: "needs a value"},
		{name: "non-string option", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: [42]",
			deny: "pip_install_options entries must be strings"},
		{name: "options not a list", yaml: "pip:\n  packages: [torch==2.1.0]\n  pip_install_options: --pre",
			deny: "pip_install_options must be a list"},

		// --- working_dir / py_modules ---
		{name: "local working_dir", yaml: "working_dir: ./src"},
		{name: "absolute working_dir", yaml: "working_dir: /home/user/project"},
		{name: "https working_dir", yaml: "working_dir: https://github.com/x/repo/archive/main.zip", deny: "remote URI"},
		{name: "s3 working_dir", yaml: "working_dir: s3://bucket/code.zip", deny: "remote URI"},
		{name: "gs working_dir", yaml: "working_dir: gs://bucket/code.zip", deny: "remote URI"},
		{name: "abfs working_dir", yaml: "working_dir: abfss://c@acct.dfs.core.windows.net/code.zip", deny: "remote URI"},
		{name: "remote working_dir allowed by scheme+host", policy: RuntimeEnvPolicy{AllowedRemoteSchemes: []string{"s3"}, AllowedRemoteHosts: []string{"artifacts.corp.example"}},
			yaml: "working_dir: s3://artifacts.corp.example/code.zip"},
		{name: "remote scheme allowlisted but host not", policy: RuntimeEnvPolicy{AllowedRemoteSchemes: []string{"s3"}, AllowedRemoteHosts: []string{"artifacts.corp.example"}},
			yaml: "working_dir: s3://attacker-bucket/code.zip", deny: "remote URI"},
		{name: "remote host allowlisted but scheme not", policy: RuntimeEnvPolicy{AllowedRemoteSchemes: []string{"s3"}, AllowedRemoteHosts: []string{"artifacts.corp.example"}},
			yaml: "working_dir: https://artifacts.corp.example/code.zip", deny: "remote URI"},
		{name: "working_dir not a string", yaml: "working_dir: [src]", deny: "working_dir must be a path or URI"},
		{name: "py_modules local", yaml: "py_modules: [./lib, /opt/shared/mod]"},
		{name: "py_modules remote entry", yaml: "py_modules: [s3://bucket/mod.zip]", deny: "remote URI"},
		{name: "py_modules not a list", yaml: "py_modules: ./lib", deny: "py_modules must be a list"},

		// --- config ---
		{name: "setup_timeout within cap", yaml: "config:\n  setup_timeout_seconds: 300"},
		{name: "setup_timeout at cap", yaml: "config:\n  setup_timeout_seconds: 600"},
		{name: "setup_timeout over cap", yaml: "config:\n  setup_timeout_seconds: 601", deny: "the policy cap is 600"},
		{name: "custom cap", policy: RuntimeEnvPolicy{MaxSetupTimeoutSeconds: 60},
			yaml: "config:\n  setup_timeout_seconds: 120", deny: "the policy cap is 60"},
		{name: "custom cap admits", policy: RuntimeEnvPolicy{MaxSetupTimeoutSeconds: 3600},
			yaml: "config:\n  setup_timeout_seconds: 1200"},
		{name: "setup_timeout zero", yaml: "config:\n  setup_timeout_seconds: 0", deny: "positive integer"},
		{name: "setup_timeout negative", yaml: "config:\n  setup_timeout_seconds: -5", deny: "positive integer"},
		{name: "setup_timeout fractional", yaml: "config:\n  setup_timeout_seconds: 1.5", deny: "positive integer"},
		{name: "setup_timeout string", yaml: `config: {setup_timeout_seconds: "300"}`, deny: "positive integer"},
		{name: "eager_install bool", yaml: "config:\n  eager_install: true"},
		{name: "eager_install not bool", yaml: "config:\n  eager_install: yes-i-really-do", deny: "eager_install must be a boolean"},
		{name: "unknown config key", yaml: "config:\n  unknown: 1", deny: `config key "unknown" is not governed`},
		{name: "config not a mapping", yaml: "config: 600", deny: "config must be a mapping"},

		// --- env_vars ---
		{name: "env_vars strings", yaml: "env_vars:\n  MY_FLAG: on\n  EMPTY: ''"},
		{name: "env_vars non-string value", yaml: "env_vars:\n  PORT: 8080", deny: `env_vars["PORT"] must be a string`},
		{name: "env_vars not a mapping", yaml: "env_vars: [A=B]", deny: "env_vars must be a mapping"},

		// --- a full legal governed document passes verbatim-shaped ---
		{name: "legal governed document", yaml: `
pip:
  - torch==2.1.0
  - pandas==2.2.3
env_vars:
  OMP_NUM_THREADS: "4"
config:
  setup_timeout_seconds: 300
  eager_install: false
working_dir: ./project
py_modules:
  - ./shared_lib
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate(tc.yaml)
			if tc.deny == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want admitted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate admitted, want refusal containing %q", tc.deny)
			}
			if !strings.Contains(err.Error(), tc.deny) {
				t.Fatalf("Validate = %q, want it to contain %q", err.Error(), tc.deny)
			}
		})
	}
}

// The document-size cap is enforced before parsing, so a huge submission
// never reaches the YAML decoder.
func TestRuntimeEnvValidateSizeCap(t *testing.T) {
	big := "pip:\n  - " + strings.Repeat("a", DefaultMaxDocumentBytes)
	if err := (RuntimeEnvPolicy{}).Validate(big); err == nil || !strings.Contains(err.Error(), "governance limit") {
		t.Fatalf("Validate(%d bytes) = %v, want a size refusal", len(big), err)
	}
	if err := (RuntimeEnvPolicy{MaxDocumentBytes: 16}).Validate("pip: [torch==2.1.0]"); err == nil {
		t.Fatal("a small custom cap must refuse a larger document")
	}
	just := (RuntimeEnvPolicy{MaxDocumentBytes: 64}).Validate("pip: [torch==2.1.0]")
	if just != nil {
		t.Fatalf("document within a custom cap refused: %v", just)
	}
}

// A policy with no caps set behaves like the documented defaults — a bare
// RuntimeEnvPolicy{} (what the server applies) is the governed policy.
func TestRuntimeEnvValidateZeroValueIsTheGovernedDefault(t *testing.T) {
	if err := (RuntimeEnvPolicy{}).Validate("config:\n  setup_timeout_seconds: 601"); err == nil {
		t.Fatal("zero-value policy must cap setup_timeout at the default 600")
	}
	if err := (RuntimeEnvPolicy{}).Validate("working_dir: s3://bucket/code.zip"); err == nil {
		t.Fatal("zero-value policy must refuse remote working_dir")
	}
}
