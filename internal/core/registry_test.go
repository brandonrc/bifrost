package core

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Ported from mobula-core/src/registry.rs #[cfg(test)] mod tests.

func testRegistry() *ClusterRegistry {
	return &ClusterRegistry{
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
	if _, ok := r.ByHostname("demo.ray.example.com"); !ok {
		t.Fatal("expected match")
	}
	if _, ok := r.ByHostname("DEMO.ray.Example.com:8484"); !ok {
		t.Fatal("expected case/port-insensitive match")
	}
	if _, ok := r.ByHostname("other.example.com"); ok {
		t.Fatal("expected no match")
	}
}

func TestLookupById(t *testing.T) {
	r := testRegistry()
	if _, ok := r.ByID(ClusterId("demo")); !ok {
		t.Fatal("expected match")
	}
	if _, ok := r.ByID(ClusterId("nope")); ok {
		t.Fatal("expected no match")
	}
}

// L6/C6: ByHostname/ByID must return a copy, not a pointer into the live
// slice — mutating the returned value must not affect the registry's
// stored entry (in particular, must not be a way to corrupt or leak-write
// AuthToken across lookups).
func TestByHostnameAndByIDReturnCopiesNotAliases(t *testing.T) {
	r := testRegistry()

	byHost, ok := r.ByHostname("demo.ray.example.com")
	if !ok {
		t.Fatal("expected match")
	}
	byHost.Hostname = "mutated"
	if r.Clusters[0].Hostname == "mutated" {
		t.Fatal("mutating ByHostname's result mutated the registry's stored entry")
	}

	byID, ok := r.ByID(ClusterId("demo"))
	if !ok {
		t.Fatal("expected match")
	}
	byID.Hostname = "mutated"
	if r.Clusters[0].Hostname == "mutated" {
		t.Fatal("mutating ByID's result mutated the registry's stored entry")
	}
}

func TestIpv6HostsAreNotMangledByPortStripping(t *testing.T) {
	// An unbracketed IPv6 literal's last segment is not a port.
	r := &ClusterRegistry{
		Clusters: []ClusterEndpoint{
			{
				Id:         ClusterId("v6"),
				Hostname:   "fe80::1",
				ApiBaseUrl: "http://[fe80::1]:8265",
			},
		},
	}
	if _, ok := r.ByHostname("fe80::1"); !ok {
		t.Fatal("expected match")
	}
	if _, ok := r.ByHostname("[fe80::1]:8484"); !ok {
		t.Fatal("expected bracketed match")
	}
	if _, ok := r.ByHostname("fe80::2"); ok {
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
	r := &ClusterRegistry{Clusters: []ClusterEndpoint{e}}

	t.Setenv("DEMO_RAY_TOKEN", "env-secret")
	if err := r.ResolveAuthTokens(); err != nil {
		t.Fatalf("resolve: %v", err)
	}

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
	e.AuthTokenEnv = strPtr("BIFROST_CORE_TEST_TOKEN_OK")
	r := &ClusterRegistry{Clusters: []ClusterEndpoint{e}}

	t.Setenv("BIFROST_CORE_TEST_TOKEN_OK", "resolved-secret")
	if err := r.ResolveAuthTokens(); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// In-memory shape is unchanged: the gateway reads the resolved token
	// from AuthToken; the env name stays as provenance.
	if r.Clusters[0].AuthToken == nil || *r.Clusters[0].AuthToken != "resolved-secret" {
		t.Fatalf("AuthToken = %v, want resolved-secret", r.Clusters[0].AuthToken)
	}
	if r.Clusters[0].AuthTokenEnv == nil || *r.Clusters[0].AuthTokenEnv != "BIFROST_CORE_TEST_TOKEN_OK" {
		t.Fatalf("AuthTokenEnv = %v, want BIFROST_CORE_TEST_TOKEN_OK", r.Clusters[0].AuthTokenEnv)
	}
}

func TestResolveAuthTokensFailsFastOnMissingOrEmptyEnv(t *testing.T) {
	for _, value := range []*string{nil, strPtr("")} {
		e := testEndpoint("demo")
		e.AuthTokenEnv = strPtr("BIFROST_CORE_TEST_TOKEN_MISSING")
		r := &ClusterRegistry{Clusters: []ClusterEndpoint{e}}

		if value != nil {
			t.Setenv("BIFROST_CORE_TEST_TOKEN_MISSING", *value)
		}
		err := r.ResolveAuthTokens()

		got, ok := err.(RegistryError)
		want := RegistryError{Kind: RegistryErrMissingTokenEnv, Id: "demo", Var: "BIFROST_CORE_TEST_TOKEN_MISSING"}
		if !ok || got != want {
			t.Fatalf("got %v, want %v", err, want)
		}
		msg := err.Error()
		if !strings.Contains(msg, "demo") || !strings.Contains(msg, "BIFROST_CORE_TEST_TOKEN_MISSING") {
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
	r2 := &ClusterRegistry{Clusters: []ClusterEndpoint{envEntry}}
	want2 := []TokenSourceNote{{Kind: TokenSourceNoteEnv, Id: "envdemo", Var: "ENVDEMO_RAY_TOKEN"}}
	if got := r2.TokenSourceNotes(); !reflect.DeepEqual(got, want2) {
		t.Fatalf("got %v, want %v", got, want2)
	}

	// Tokenless entries produce no note.
	r3 := &ClusterRegistry{Clusters: []ClusterEndpoint{testEndpoint("bare")}}
	if got := r3.TokenSourceNotes(); len(got) != 0 {
		t.Fatalf("expected no notes, got %v", got)
	}
}

// Added (not ported from Rust): fix round 1 (review finding M3). A
// zero-value ClusterRegistry (nil Clusters) must still marshal as `[]`,
// not the Go zero value `null`, matching Rust's Vec::default() serde
// behavior.
func TestClusterRegistryMarshalsNilClustersAsEmpty(t *testing.T) {
	var r ClusterRegistry
	b, err := json.Marshal(&r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if clusters, ok := v["clusters"].([]any); !ok || len(clusters) != 0 {
		t.Fatalf("clusters = %v, want []", v["clusters"])
	}
}

// --- Dynamic entries (#5: the lifecycle controller registers ephemeral
// clusters at run time; the static file stays the operator's override) ---

func dynamicEndpoint(id string) ClusterEndpoint {
	return ClusterEndpoint{
		Id:         ClusterId(id),
		Hostname:   id + ".ray.kind.invalid",
		ApiBaseUrl: "http://" + id + "-head-svc:8265",
		Project:    "team-a",
	}
}

func TestUpsertRegistersDynamicEntryVisibleToLookups(t *testing.T) {
	r := testRegistry()
	if err := r.Upsert(dynamicEndpoint("job-1")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, ok := r.ByHostname("JOB-1.ray.kind.invalid:8484")
	if !ok || got.Id != "job-1" || got.Source != RegistrySourceDynamic || got.Target != RegistryTargetJobs || got.Project != "team-a" {
		t.Fatalf("ByHostname = %+v %v", got, ok)
	}
	if got, ok := r.ByID("job-1"); !ok || got.Source != RegistrySourceDynamic {
		t.Fatalf("ByID = %+v %v", got, ok)
	}
	if st, ok := r.ByID("demo"); !ok || st.Source != RegistrySourceStatic || st.Target != RegistryTargetJobs {
		t.Fatalf("static entry must be stamped static/jobs: %+v %v", st, ok)
	}
	if len(r.Clusters) != 1 {
		t.Fatalf("Upsert must not touch the static slice: %d", len(r.Clusters))
	}
}

func TestUpsertRefusesToShadowStaticEntries(t *testing.T) {
	r := testRegistry()
	shadow := dynamicEndpoint("job-1")
	shadow.Hostname = "DEMO.ray.example.com"
	var re RegistryError
	if err := r.Upsert(shadow); !errors.As(err, &re) || re.Kind != RegistryErrDuplicateHostname {
		t.Fatalf("shadowing a static hostname: %v", err)
	}
	sameID := dynamicEndpoint("demo")
	if err := r.Upsert(sameID); !errors.As(err, &re) || re.Kind != RegistryErrDuplicateId {
		t.Fatalf("reusing a static id: %v", err)
	}
	bad := dynamicEndpoint("job-2")
	bad.Hostname = "job 2"
	if err := r.Upsert(bad); !errors.As(err, &re) || re.Kind != RegistryErrInvalidHostname {
		t.Fatalf("invalid hostname: %v", err)
	}
	if _, ok := r.ByID("job-1"); ok {
		t.Fatal("refused entries must not be registered")
	}
}

func TestUpsertReplacesByIdButRefusesDynamicHostnameCollision(t *testing.T) {
	r := &ClusterRegistry{}
	if err := r.Upsert(dynamicEndpoint("a")); err != nil {
		t.Fatal(err)
	}
	moved := dynamicEndpoint("a")
	moved.ApiBaseUrl = "http://a-head-svc:9999"
	if err := r.Upsert(moved); err != nil {
		t.Fatalf("re-upsert by id: %v", err)
	}
	if got, _ := r.ByID("a"); got.ApiBaseUrl != "http://a-head-svc:9999" {
		t.Fatalf("re-upsert did not replace: %+v", got)
	}
	collide := dynamicEndpoint("b")
	collide.Hostname = "A.ray.kind.invalid"
	var re RegistryError
	if err := r.Upsert(collide); !errors.As(err, &re) || re.Kind != RegistryErrDuplicateHostname {
		t.Fatalf("two dynamic entries on one hostname: %v", err)
	}
}

func TestRemoveAndSnapshotOrder(t *testing.T) {
	r := testRegistry()
	for _, id := range []string{"zeta", "alpha"} {
		if err := r.Upsert(dynamicEndpoint(id)); err != nil {
			t.Fatal(err)
		}
	}
	snap := r.Snapshot()
	ids := make([]string, len(snap))
	for i, c := range snap {
		ids[i] = string(c.Id) + ":" + c.Source
	}
	want := []string{"demo:static", "alpha:dynamic", "zeta:dynamic"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("snapshot = %v, want %v", ids, want)
	}
	snap[0].Hostname = "mutated"
	if r.Clusters[0].Hostname == "mutated" {
		t.Fatal("Snapshot must return copies")
	}
	if !r.Remove("zeta") || r.Remove("zeta") || r.Remove("demo") {
		t.Fatal("Remove: first true, repeat false, static never removable")
	}
	if _, ok := r.ByID("zeta"); ok {
		t.Fatal("removed entry still resolves")
	}
	if _, ok := r.ByID("demo"); !ok {
		t.Fatal("static entry must survive a Remove attempt")
	}
	if !strings.Contains(r.String(), "alpha") || strings.Contains(r.String(), "secret") {
		t.Fatalf("String must list dynamic entries and redact tokens: %s", r.String())
	}
}

func TestDynamicEntriesAreNotPartOfTheFileFormat(t *testing.T) {
	r := testRegistry()
	_ = r.Upsert(dynamicEndpoint("job-1"))
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "job-1") {
		t.Fatalf("dynamic entry leaked into the registry file format: %s", b)
	}
	var back ClusterRegistry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Clusters) != 1 || back.Clusters[0].Id != "demo" {
		t.Fatalf("round trip: %+v", back.Clusters)
	}
}
