package core

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Ported from mobula-core/src/registry.rs #[cfg(test)] mod tests.

func testRegistry() ClusterRegistry {
	return ClusterRegistry{
		Clusters: []ClusterEndpoint{
			{
				Id:         ClusterId("demo"),
				Hostname:   "demo.ray.example.com",
				ApiBaseUrl: "http://demo-head-svc:8265",
				AuthToken:  strPtr("secret"),
			},
		},
	}
}

func testEndpoint(id string) ClusterEndpoint {
	return ClusterEndpoint{
		Id:         ClusterId(id),
		Hostname:   id + ".ray.example.com",
		ApiBaseUrl: "https://demo-head-svc:8265",
	}
}

func TestHostnameLookupIgnoresPortAndCase(t *testing.T) {
	r := testRegistry()
	if r.ByHostname("demo.ray.example.com") == nil {
		t.Fatal("expected match")
	}
	if r.ByHostname("DEMO.ray.Example.com:8484") == nil {
		t.Fatal("expected case/port-insensitive match")
	}
	if r.ByHostname("other.example.com") != nil {
		t.Fatal("expected no match")
	}
}

func TestLookupById(t *testing.T) {
	r := testRegistry()
	if r.ByID(ClusterId("demo")) == nil {
		t.Fatal("expected match")
	}
	if r.ByID(ClusterId("nope")) != nil {
		t.Fatal("expected no match")
	}
}

func TestIpv6HostsAreNotMangledByPortStripping(t *testing.T) {
	// An unbracketed IPv6 literal's last segment is not a port.
	r := ClusterRegistry{
		Clusters: []ClusterEndpoint{
			{
				Id:         ClusterId("v6"),
				Hostname:   "fe80::1",
				ApiBaseUrl: "http://[fe80::1]:8265",
			},
		},
	}
	if r.ByHostname("fe80::1") == nil {
		t.Fatal("expected match")
	}
	if r.ByHostname("[fe80::1]:8484") == nil {
		t.Fatal("expected bracketed match")
	}
	if r.ByHostname("fe80::2") != nil {
		t.Fatal("expected no match")
	}
}

func TestDebugRedactsAuthToken(t *testing.T) {
	printed := testRegistry().String()
	if strings.Contains(printed, "secret") {
		t.Fatalf("debug output leaked token: %s", printed)
	}
	if !strings.Contains(printed, "[REDACTED]") {
		t.Fatalf("debug output missing redaction marker: %s", printed)
	}
}

func TestValidateAcceptsGoodRegistryAndRejectsCleartextToken(t *testing.T) {
	r := testRegistry() // http:// + token
	err, ok := r.Validate(false).(RegistryError)
	if !ok || err.Kind != RegistryErrCleartextToken {
		t.Fatalf("expected CleartextToken error, got %v", r.Validate(false))
	}
	if err := r.Validate(true); err != nil {
		t.Fatalf("dev override should permit http+token: %v", err)
	}

	https := testRegistry()
	https.Clusters[0].ApiBaseUrl = "https://demo-head-svc:8265"
	if err := https.Validate(false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsDuplicatesAndBadUrls(t *testing.T) {
	dup := testRegistry()
	dup.Clusters = append(dup.Clusters, ClusterEndpoint{
		Id:         ClusterId("other"),
		Hostname:   "DEMO.ray.example.com", // case-insensitive dup
		ApiBaseUrl: "https://x:1",
	})
	if err, ok := dup.Validate(true).(RegistryError); !ok || err.Kind != RegistryErrDuplicateHostname {
		t.Fatalf("expected DuplicateHostname error, got %v", dup.Validate(true))
	}

	dupId := testRegistry()
	dupId.Clusters = append(dupId.Clusters, ClusterEndpoint{
		Id:         ClusterId("demo"),
		Hostname:   "other.example.com",
		ApiBaseUrl: "https://x:1",
	})
	if err, ok := dupId.Validate(true).(RegistryError); !ok || err.Kind != RegistryErrDuplicateId {
		t.Fatalf("expected DuplicateId error, got %v", dupId.Validate(true))
	}

	for _, url := range []string{
		"ftp://host:1",
		"https://user:pw@host:1",
		"https://host/x#frag",
		"https://",
		"not-a-url",
	} {
		bad := testRegistry()
		bad.Clusters[0].ApiBaseUrl = url
		if err, ok := bad.Validate(true).(RegistryError); !ok || err.Kind != RegistryErrInvalidUrl {
			t.Fatalf("%s should be rejected as InvalidUrl, got %v", url, bad.Validate(true))
		}
	}

	badHost := testRegistry()
	badHost.Clusters[0].Hostname = "demo host"
	if err, ok := badHost.Validate(true).(RegistryError); !ok || err.Kind != RegistryErrInvalidHostname {
		t.Fatalf("expected InvalidHostname error, got %v", badHost.Validate(true))
	}
}

func TestValidateRejectsLinkLocalAndCgnatLiteralIps(t *testing.T) {
	// #2: cloud metadata endpoints and overlay meshes must never be
	// registered as cluster heads.
	for _, url := range []string{
		"http://169.254.169.254:8265",
		"https://169.254.0.1",
		"http://100.64.0.1:8265",
		"http://100.127.255.254",
		"http://[fe80::1]:8265",
		"http://[febf::ffff]:8265",
	} {
		bad := testRegistry()
		bad.Clusters[0].ApiBaseUrl = url
		bad.Clusters[0].AuthToken = nil
		if err, ok := bad.Validate(true).(RegistryError); !ok || err.Kind != RegistryErrInvalidUrl {
			t.Fatalf("%s should be rejected, got %v", url, bad.Validate(true))
		}
	}
	// Ordinary private/loopback IPs (in-cluster heads, dev setups) and
	// DNS names (residual risk, documented on Validate) still pass.
	for _, url := range []string{
		"http://10.0.0.5:8265",
		"http://127.0.0.1:8265",
		"http://100.63.255.255:8265",
		"https://[fd00::1]:8265",
		"http://demo-head-svc:8265",
	} {
		ok := testRegistry()
		ok.Clusters[0].ApiBaseUrl = url
		ok.Clusters[0].AuthToken = nil
		if err := ok.Validate(false); err != nil {
			t.Fatalf("%s should pass, got %v", url, err)
		}
	}
}

func TestStripPortEdgeCases(t *testing.T) {
	cases := map[string]string{
		"example.com:8080": "example.com",
		"example.com":      "example.com",
		"example.com:":     "example.com:",
		"example.com:8a":   "example.com:8a",
		"[::1]:9000":       "::1",
		"[::1]":            "::1",
		"fe80::1":          "fe80::1",
		"127.0.0.1:8484":   "127.0.0.1",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Fatalf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthTokenNeverSerializes(t *testing.T) {
	b, err := json.Marshal(testRegistry())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "secret") || strings.Contains(s, "auth_token") {
		t.Fatalf("registry JSON leaked the token: %s", s)
	}
}

func TestAuthTokenEnvSerializesButTokenNeverDoes(t *testing.T) {
	// #57: the env var NAME is not a secret and may serialize; the token
	// itself must not, even when env-sourced.
	e := testEndpoint("demo")
	e.AuthTokenEnv = strPtr("DEMO_RAY_TOKEN")
	r := ClusterRegistry{Clusters: []ClusterEndpoint{e}}

	os.Setenv("DEMO_RAY_TOKEN", "env-secret")
	if err := r.ResolveAuthTokens(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	os.Unsetenv("DEMO_RAY_TOKEN")

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "auth_token_env") {
		t.Fatalf("expected auth_token_env in JSON: %s", s)
	}
	if !strings.Contains(s, "DEMO_RAY_TOKEN") {
		t.Fatalf("expected env var name in JSON: %s", s)
	}
	if strings.Contains(s, "env-secret") {
		t.Fatalf("token leaked into JSON: %s", s)
	}
}

func TestResolveAuthTokensReadsEnvIntoAuthToken(t *testing.T) {
	e := testEndpoint("demo")
	e.AuthTokenEnv = strPtr("MOBULA_CORE_TEST_TOKEN_OK")
	r := ClusterRegistry{Clusters: []ClusterEndpoint{e}}

	os.Setenv("MOBULA_CORE_TEST_TOKEN_OK", "resolved-secret")
	if err := r.ResolveAuthTokens(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	os.Unsetenv("MOBULA_CORE_TEST_TOKEN_OK")

	// In-memory shape is unchanged: the gateway reads the resolved token
	// from AuthToken; the env name stays as provenance.
	if r.Clusters[0].AuthToken == nil || *r.Clusters[0].AuthToken != "resolved-secret" {
		t.Fatalf("AuthToken = %v, want resolved-secret", r.Clusters[0].AuthToken)
	}
	if r.Clusters[0].AuthTokenEnv == nil || *r.Clusters[0].AuthTokenEnv != "MOBULA_CORE_TEST_TOKEN_OK" {
		t.Fatalf("AuthTokenEnv = %v, want MOBULA_CORE_TEST_TOKEN_OK", r.Clusters[0].AuthTokenEnv)
	}
}

func TestResolveAuthTokensFailsFastOnMissingOrEmptyEnv(t *testing.T) {
	for _, value := range []*string{nil, strPtr("")} {
		e := testEndpoint("demo")
		e.AuthTokenEnv = strPtr("MOBULA_CORE_TEST_TOKEN_MISSING")
		r := ClusterRegistry{Clusters: []ClusterEndpoint{e}}

		if value != nil {
			os.Setenv("MOBULA_CORE_TEST_TOKEN_MISSING", *value)
		} else {
			os.Unsetenv("MOBULA_CORE_TEST_TOKEN_MISSING")
		}
		err := r.ResolveAuthTokens()
		os.Unsetenv("MOBULA_CORE_TEST_TOKEN_MISSING")

		got, ok := err.(RegistryError)
		want := RegistryError{Kind: RegistryErrMissingTokenEnv, Id: "demo", Var: "MOBULA_CORE_TEST_TOKEN_MISSING"}
		if !ok || got != want {
			t.Fatalf("got %v, want %v", err, want)
		}
		msg := err.Error()
		if !strings.Contains(msg, "demo") || !strings.Contains(msg, "MOBULA_CORE_TEST_TOKEN_MISSING") {
			t.Fatalf("error message missing context: %s", msg)
		}
	}
}

func TestResolveAuthTokensRejectsBothTokenSources(t *testing.T) {
	r := testRegistry() // plaintext token
	r.Clusters[0].AuthTokenEnv = strPtr("SOME_VAR")
	got, ok := r.ResolveAuthTokens().(RegistryError)
	want := RegistryError{Kind: RegistryErrConflictingTokenSource, Id: "demo"}
	if !ok || got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTokenSourceNotesFlagPlaintextAndAcknowledgeEnv(t *testing.T) {
	r := testRegistry() // plaintext token
	want := []TokenSourceNote{{Kind: TokenSourceNotePlaintext, Id: "demo"}}
	if got := r.TokenSourceNotes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	envEntry := testEndpoint("envdemo")
	envEntry.AuthTokenEnv = strPtr("ENVDEMO_RAY_TOKEN")
	r2 := ClusterRegistry{Clusters: []ClusterEndpoint{envEntry}}
	want2 := []TokenSourceNote{{Kind: TokenSourceNoteEnv, Id: "envdemo", Var: "ENVDEMO_RAY_TOKEN"}}
	if got := r2.TokenSourceNotes(); !reflect.DeepEqual(got, want2) {
		t.Fatalf("got %v, want %v", got, want2)
	}

	// Tokenless entries produce no note.
	r3 := ClusterRegistry{Clusters: []ClusterEndpoint{testEndpoint("bare")}}
	if got := r3.TokenSourceNotes(); len(got) != 0 {
		t.Fatalf("expected no notes, got %v", got)
	}
}
