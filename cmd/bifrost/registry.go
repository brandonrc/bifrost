package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/brandonrc/bifrost/internal/core"
)

// loadRegistry reads, resolves, and validates a cluster registry file for
// the job gateway (T13/gateway.go).
//
// Format decision (T15, reconciled against .github/workflows/contract.yml):
// JSON, not mobula's TOML. core.ClusterRegistry/ClusterEndpoint (T4,
// registry.go) already carry `json` struct tags and no `toml` ones, no
// TOML library is vendored anywhere in go.mod, and T14 wired
// contract.yml's fixture as `clusters.json` shaped exactly like
// ClusterEndpoint's JSON — so JSON is not a new choice here, it is the
// only one consistent with what's already landed. The registry's own
// wire shape (the contract) is unaffected either way: this file never
// crosses the API.
//
// Steps mirror mobula-cli's load_registry: decode, log per-entry token
// provenance (#57 — never a value), warn on a plaintext-token file that
// is group/other readable (#4, unix only), resolve `auth_token_env`
// indirections, then run the registry's own security validation
// (duplicate ids/hostnames, URL scheme/SSRF posture, cleartext-token
// refusal) — mobula-api never called ClusterRegistry::validate itself
// (it's a load-time gate), and neither does any internal/api caller in
// this Go port, so the CLI is the one place it must run.
func loadRegistry(path string, allowInsecureTransport bool) (*core.ClusterRegistry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg core.ClusterRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("invalid registry %s: %w", path, err)
	}

	for _, note := range reg.TokenSourceNotes() {
		switch note.Kind {
		case core.TokenSourceNotePlaintext:
			slog.Warn("plaintext token in registry file; prefer auth_token_env — issue #57", "cluster", note.Id)
		case core.TokenSourceNoteEnv:
			slog.Info("token source: env", "cluster", note.Id, "env_var", note.Var)
		}
	}
	if warning := registryPermissionWarning(path, &reg); warning != "" {
		slog.Warn(warning)
	}

	if err := reg.ResolveAuthTokens(); err != nil {
		return nil, fmt.Errorf("invalid registry %s: %w", path, err)
	}
	if err := reg.Validate(allowInsecureTransport); err != nil {
		return nil, fmt.Errorf("invalid registry %s: %w", path, err)
	}
	return &reg, nil
}

// registryPermissionWarning mirrors mobula-cli's registry_permission_warning:
// a registry file carrying a plaintext auth_token is a bearer-equivalent
// secret (#4) — warn (never fail) when it is readable by group/other.
// Entries using auth_token_env hold no secret in the file and need no
// warning. Windows has no POSIX mode bits to check.
func registryPermissionWarning(path string, reg *core.ClusterRegistry) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	hasPlaintext := false
	for _, c := range reg.Clusters {
		if c.AuthToken != nil && c.AuthTokenEnv == nil {
			hasPlaintext = true
			break
		}
	}
	if !hasPlaintext {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Sprintf(
			"registry %s contains auth_tokens but is mode %04o — group/other can "+
				"read cluster bearer tokens; run: chmod 600 %s", path, mode, path)
	}
	return ""
}
