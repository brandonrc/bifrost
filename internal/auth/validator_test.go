package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Port of the Validator-level cases from the predecessor's api crate, tests/auth.rs (a mock
// OIDC provider backed by a real RSA key, locally signed JWTs) — the
// end-to-end HTTP/RBAC matrix (viewer_reads_but_cannot_submit and friends)
// belongs to the API layer (Task 10+) and isn't ported here; the RBAC
// wiring itself is covered by TestValidateResolvesRolesFromGroups below.

// testIdp is a mock OIDC issuer serving discovery + JWKS for a fresh RSA
// key, mirroring the predecessor's api crate, tests/auth.rs's spawn_idp().
type testIdp struct {
	server *httptest.Server
	issuer string
	priv   *rsa.PrivateKey
	kid    string
	hits   atomic.Int32 // JWKS fetch count, for the cooldown test
}

func newTestIdp(t *testing.T) *testIdp {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &testIdp{priv: priv, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   idp.issuer,
			"jwks_uri": idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		idp.hits.Add(1)
		pub := idp.priv.PublicKey
		jwk := map[string]string{
			"kty": "RSA",
			"kid": idp.kid,
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
	})

	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

// signRaw signs an arbitrary claim set under the IdP's key and the test
// kid, giving tests full control over which claims are present.
func (idp *testIdp) signRaw(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = idp.kid
	s, err := tok.SignedString(idp.priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// signRawKid is signRaw with an overridden kid header (for unknown-kid
// tests).
func (idp *testIdp) signRawKid(t *testing.T, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(idp.priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// token is the common case: sub=user-123, configurable groups/aud/exp
// offset, mirroring Idp::token in the predecessor's api crate, tests/auth.rs.
func (idp *testIdp) token(t *testing.T, groups []string, aud string, expOffset time.Duration) string {
	t.Helper()
	now := time.Now()
	return idp.signRaw(t, jwt.MapClaims{
		"sub":    "user-123",
		"email":  "user@example.com",
		"iss":    idp.issuer,
		"aud":    aud,
		"exp":    now.Add(expOffset).Unix(),
		"iat":    now.Unix(),
		"groups": groups,
	})
}

func discoverT(t *testing.T, idp *testIdp, roles RoleMappings) *Validator {
	t.Helper()
	cfg := AuthConfig{
		Issuer:      idp.issuer,
		Audience:    "bifrost",
		GroupsClaim: "groups",
		Roles:       roles,
	}
	v, err := Discover(context.Background(), cfg, IdpClient(), true)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return v
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func authErrKind(t *testing.T, err error) AuthErrorKind {
	t.Helper()
	var ae AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AuthError, got %T: %v", err, err)
	}
	return ae.Kind
}

// Port of http_issuer_is_refused_without_override (the predecessor's api crate, tests/auth.rs:194-212).
func TestHTTPIssuerIsRefusedWithoutOverride(t *testing.T) {
	idp := newTestIdp(t) // http://127.0.0.1:port
	cfg := AuthConfig{Issuer: idp.issuer, Audience: "bifrost", GroupsClaim: "groups"}

	_, err := Discover(context.Background(), cfg, IdpClient(), false)
	if err == nil {
		t.Fatal("http issuer should be refused without override")
	}
	if authErrKind(t, err) != AuthErrInsecureIssuer {
		t.Fatalf("expected AuthErrInsecureIssuer, got %v", err)
	}
	if !strings.Contains(err.Error(), "not https") {
		t.Fatalf("expected 'not https' in message, got %q", err.Error())
	}

	// With the override it proceeds to real discovery.
	if _, err := Discover(context.Background(), cfg, IdpClient(), true); err != nil {
		t.Fatalf("expected success with allowInsecure=true: %v", err)
	}
}

// Port of token_without_subject_is_rejected (the predecessor's api crate, tests/auth.rs:214-235).
func TestTokenWithoutSubjectIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})

	tok := idp.signRaw(t, jwt.MapClaims{
		"sub":    "",
		"iss":    idp.issuer,
		"aud":    "bifrost",
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"groups": []string{"/ml-eng"},
	})
	_, err := v.Validate(context.Background(), tok)
	if err == nil {
		t.Fatal("expected error for empty subject")
	}
	if authErrKind(t, err) != AuthErrMissingSubject {
		t.Fatalf("expected AuthErrMissingSubject, got %v", err)
	}
	if !strings.Contains(err.Error(), "no subject") {
		t.Fatalf("expected 'no subject' in message, got %q", err.Error())
	}
}

// Port of tokens_missing_iss_aud_or_with_future_nbf_are_rejected
// (the predecessor's api crate, tests/auth.rs:237-270).
func TestTokensMissingIssAudOrWithFutureNbfAreRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})
	now := time.Now()

	t.Run("missing aud", func(t *testing.T) {
		tok := idp.signRaw(t, jwt.MapClaims{
			"sub": "u", "iss": idp.issuer, "exp": now.Add(5 * time.Minute).Unix(),
			"groups": []string{"/ml-eng"},
		})
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected error for missing aud")
		}
	})

	t.Run("missing iss", func(t *testing.T) {
		tok := idp.signRaw(t, jwt.MapClaims{
			"sub": "u", "aud": "bifrost", "exp": now.Add(5 * time.Minute).Unix(),
			"groups": []string{"/ml-eng"},
		})
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected error for missing iss")
		}
	})

	t.Run("future nbf", func(t *testing.T) {
		tok := idp.signRaw(t, jwt.MapClaims{
			"sub": "u", "iss": idp.issuer, "aud": "bifrost",
			"exp": now.Add(10 * time.Minute).Unix(), "nbf": now.Add(5 * time.Minute).Unix(),
			"groups": []string{"/ml-eng"},
		})
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected error for future nbf")
		}
	})
}

// Port of garbage_and_expired_tokens_are_401 (the predecessor's api crate, tests/auth.rs:288-314),
// adapted to the Validator level (the 401 mapping is an API-layer concern).
func TestGarbageAndExpiredTokensAreRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})

	t.Run("not a jwt", func(t *testing.T) {
		if _, err := v.Validate(context.Background(), "not-a-jwt"); err == nil {
			t.Fatal("expected error for garbage token")
		} else if authErrKind(t, err) != AuthErrInvalidToken {
			t.Fatalf("expected AuthErrInvalidToken, got %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		tok := idp.token(t, []string{"/ml-eng"}, "bifrost", -5*time.Minute)
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected error for expired token")
		}
	})

	t.Run("wrong audience", func(t *testing.T) {
		tok := idp.token(t, []string{"/ml-eng"}, "not-bifrost", 5*time.Minute)
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected error for wrong audience")
		}
	})
}

// Issuer cross-check (#16): a provider whose discovery document advertises
// a different issuer than configured is rejected — Rust's hostile-provider
// guard. Not directly exercised by the predecessor's api crate, tests/auth.rs (which relies
// on the mock IdP always advertising its own address); ported fresh from
// the Discover logic in the predecessor's auth crate, src/lib.rs:494-502.
func TestDiscoverIssuerCrossCheckRejectsMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   "https://evil.example.com",
			"jwks_uri": "http://unused.invalid/jwks",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := AuthConfig{Issuer: server.URL, Audience: "bifrost", GroupsClaim: "groups"}
	_, err := Discover(context.Background(), cfg, IdpClient(), true)
	if err == nil {
		t.Fatal("expected issuer mismatch to be rejected")
	}
	if authErrKind(t, err) != AuthErrDiscovery {
		t.Fatalf("expected AuthErrDiscovery, got %v", err)
	}
	if !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("expected 'issuer mismatch' in message, got %q", err.Error())
	}
}

// The cross-check is trailing-slash-insensitive: an advertised issuer that
// differs only by a trailing slash must NOT be treated as a mismatch.
func TestDiscoverIssuerCrossCheckIsTrailingSlashInsensitive(t *testing.T) {
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   issuer + "/", // trailing slash the configured issuer lacks
			"jwks_uri": issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	issuer = server.URL

	cfg := AuthConfig{Issuer: issuer, Audience: "bifrost"}
	_, err := Discover(context.Background(), cfg, IdpClient(), true)
	// Must fail for the EMPTY-JWKS reason, not an issuer mismatch — proves
	// the cross-check tolerated the trailing slash.
	if err == nil {
		t.Fatal("expected failure due to empty JWKS")
	}
	if authErrKind(t, err) != AuthErrJwks {
		t.Fatalf("expected AuthErrJwks (cross-check should have passed), got %v", err)
	}
}

// JWKS refresh cooldown: an unknown kid triggers at most one refresh per
// cooldown window, and a new attempt is allowed once the window elapses.
// Not directly present in the Rust test suite (no fake-clock harness in
// the Rust predecessor's tests); ported fresh from the REFRESH_COOLDOWN contract
// documented at the predecessor's auth crate, src/lib.rs:456-458, 550-561. The cooldown is
// shrunk via the unexported field (same-package test) so the test doesn't
// need to wait out the real 30s default.
func TestJWKSRefreshIsCooldownGated(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})
	if got := idp.hits.Load(); got != 1 {
		t.Fatalf("expected exactly 1 JWKS fetch from discovery, got %d", got)
	}
	v.refreshCooldown = 100 * time.Millisecond
	// The initial Discover() fetch just claimed the cooldown window at
	// the real 30s duration; backdate it so the shrunk window is already
	// elapsed and the first unknown-kid call below actually refreshes.
	v.lastRefresh = time.Now().Add(-v.refreshCooldown)

	unknownKidTok := idp.signRawKid(t, "does-not-exist", jwt.MapClaims{
		"sub": "u", "iss": idp.issuer, "aud": "bifrost",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "groups": []string{"/ml-eng"},
	})

	// Two calls back to back, well within the cooldown window: only one
	// extra refresh should occur.
	for i := 0; i < 2; i++ {
		_, err := v.Validate(context.Background(), unknownKidTok)
		if authErrKind(t, err) != AuthErrUnknownKeyID {
			t.Fatalf("call %d: expected AuthErrUnknownKeyID, got %v", i, err)
		}
	}
	if got := idp.hits.Load(); got != 2 {
		t.Fatalf("expected exactly 1 refresh across 2 rapid unknown-kid calls (2 total fetches), got %d", got)
	}

	// After the cooldown elapses, a new unknown-kid lookup is allowed to
	// refresh again.
	time.Sleep(150 * time.Millisecond)
	_, err := v.Validate(context.Background(), unknownKidTok)
	if authErrKind(t, err) != AuthErrUnknownKeyID {
		t.Fatalf("expected AuthErrUnknownKeyID, got %v", err)
	}
	if got := idp.hits.Load(); got != 3 {
		t.Fatalf("expected a 3rd fetch after the cooldown elapsed, got %d", got)
	}
}

// Fix round 1, #1 (reproduced DoS): a caller whose request context is
// already canceled by the time Validate reaches the JWKS refresh must not
// abort the (shared, cooldown-gated) fetch. Before the fix, the canceled
// context aborted the HTTP round trip AFTER the cooldown slot had already
// been claimed, so the refresh never completed and no other caller — not
// even an honest one — could retry for a full cooldown window. One
// canceled request every 30s would starve key rotation forever.
func TestJWKSRefreshSurvivesCanceledCallerContext(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})
	if got := idp.hits.Load(); got != 1 {
		t.Fatalf("expected 1 fetch from discovery, got %d", got)
	}
	v.refreshCooldown = 100 * time.Millisecond
	v.lastRefresh = time.Now().Add(-v.refreshCooldown)

	unknownKidTok := idp.signRawKid(t, "does-not-exist", jwt.MapClaims{
		"sub": "u", "iss": idp.issuer, "aud": "bifrost",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "groups": []string{"/ml-eng"},
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Validate(canceled, unknownKidTok); authErrKind(t, err) != AuthErrUnknownKeyID {
		t.Fatalf("expected AuthErrUnknownKeyID despite a canceled ctx (the refresh must still complete), got %v", err)
	}
	if got := idp.hits.Load(); got != 2 {
		t.Fatalf("expected the refresh to complete despite the canceled context, got %d fetches", got)
	}

	// An honest call in the SAME cooldown window must not trigger another
	// fetch — the canceled call's refresh already completed successfully.
	if _, err := v.Validate(context.Background(), unknownKidTok); authErrKind(t, err) != AuthErrUnknownKeyID {
		t.Fatalf("expected AuthErrUnknownKeyID, got %v", err)
	}
	if got := idp.hits.Load(); got != 2 {
		t.Fatalf("expected the cooldown to hold (no 3rd fetch), got %d fetches", got)
	}
}

// Fix round 1, #2 (reproduced privilege escalation): Validator.RoleMappings()
// must return a defensive copy. Before the fix, the returned RoleMappings'
// slices aliased the live authz config's backing arrays; a caller (or a
// handler building a response) mutating an element of the "copy" silently
// promoted an unmapped caller (e.g. overwriting index 0 of a Viewer mapping
// with "*").
func TestValidatorRoleMappingsIsDefensivelyCopied(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"/observers"}})

	got := v.RoleMappings()
	got.Viewer[0] = "*"                   // would corrupt the shared backing array if aliased
	got.Viewer = append(got.Viewer, "/x") // and this would too, within capacity

	// A fresh accessor call must be unaffected by the mutation above.
	live := v.RoleMappings()
	if len(live.Viewer) != 1 || live.Viewer[0] != "/observers" {
		t.Fatalf("mutating a returned RoleMappings corrupted the live config: %v", live.Viewer)
	}

	// End-to-end: an unrelated, unmapped group must still be denied — if
	// the "*" mutation above had reached the live config, this caller
	// would now be promoted to Viewer.
	stranger := idp.token(t, []string{"/unrelated-group"}, "bifrost", 5*time.Minute)
	id, err := v.Validate(context.Background(), stranger)
	if err != nil {
		t.Fatalf("validate stranger token: %v", err)
	}
	if id.IsAuthorized() {
		t.Fatal("mutating the returned RoleMappings promoted an unmapped caller")
	}
}

// Fix round 1, #4 — clock skew leeway (RULING: port fidelity with the
// jsonwebtoken reference's default 60s leeway, applied via authLeeway).
func TestClockSkewLeeway(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Developer: []string{"/ml-eng"}})

	t.Run("expired 30s ago is within leeway", func(t *testing.T) {
		tok := idp.token(t, []string{"/ml-eng"}, "bifrost", -30*time.Second)
		if _, err := v.Validate(context.Background(), tok); err != nil {
			t.Fatalf("expected acceptance within the 60s leeway, got %v", err)
		}
	})

	t.Run("expired 90s ago exceeds leeway", func(t *testing.T) {
		tok := idp.token(t, []string{"/ml-eng"}, "bifrost", -90*time.Second)
		if _, err := v.Validate(context.Background(), tok); err == nil {
			t.Fatal("expected rejection beyond the 60s leeway")
		}
	})
}

// Port of wildcard-warning boot logging (#35), the predecessor's auth crate, src/lib.rs:504-512.
func TestDiscoverLogsWildcardWarning(t *testing.T) {
	idp := newTestIdp(t)
	buf := captureLogs(t)
	cfg := AuthConfig{Issuer: idp.issuer, Audience: "bifrost", Roles: RoleMappings{Viewer: []string{"*"}}}
	if _, err := Discover(context.Background(), cfg, IdpClient(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "wildcard") {
		t.Fatalf("expected wildcard warning in logs, got %q", buf.String())
	}
}

func TestDiscoverWithoutWildcardLogsNoWarning(t *testing.T) {
	idp := newTestIdp(t)
	buf := captureLogs(t)
	cfg := AuthConfig{Issuer: idp.issuer, Audience: "bifrost", Roles: RoleMappings{Developer: []string{"/ml-eng"}}}
	if _, err := Discover(context.Background(), cfg, IdpClient(), true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "wildcard") {
		t.Fatalf("unexpected wildcard warning: %q", buf.String())
	}
}

// Port of project_roles boot-time logging (#103), the predecessor's auth crate, src/lib.rs:514-529.
func TestDiscoverLogsProjectRolesBoot(t *testing.T) {
	idp := newTestIdp(t)
	buf := captureLogs(t)
	cfg := AuthConfig{
		Issuer: idp.issuer, Audience: "bifrost",
		ProjectRoles: ProjectRoleMappings{Operator: []string{"*"}},
	}
	if _, err := Discover(context.Background(), cfg, IdpClient(), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "project_roles") {
		t.Fatalf("expected project_roles boot log, got %q", buf.String())
	}
}

func TestDiscoverWithoutProjectRolesLogsNothing(t *testing.T) {
	idp := newTestIdp(t)
	buf := captureLogs(t)
	cfg := AuthConfig{Issuer: idp.issuer, Audience: "bifrost"}
	if _, err := Discover(context.Background(), cfg, IdpClient(), true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "project_roles") {
		t.Fatalf("unexpected project_roles log: %q", buf.String())
	}
}

// End-to-end: Validate resolves group claims into Roles/ProjectRoles via
// the configured mappings, and unmapped groups deny by default — the same
// matrix the predecessor's api crate, tests/auth.rs exercises through the full HTTP app
// (developer_submits_and_unmapped_groups_are_denied,
// operator_reads_but_cannot_submit_jobs), checked here directly against
// the Identity the Validator produces.
func TestValidateResolvesRolesFromGroups(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{
		Operator:  []string{"/sre"},
		Developer: []string{"/ml-eng"},
	})

	dev := idp.token(t, []string{"/ml-eng"}, "bifrost", 5*time.Minute)
	id, err := v.Validate(context.Background(), dev)
	if err != nil {
		t.Fatalf("validate developer token: %v", err)
	}
	if !id.Permits(Write, TargetJob) {
		t.Fatal("expected developer to write jobs")
	}
	if id.Permits(Write, TargetCluster) {
		t.Fatal("expected developer to NOT write clusters")
	}
	if id.Username != nil {
		t.Fatalf("expected no preferred_username claim, got %v", *id.Username)
	}
	if id.Owner() != "user-123" {
		t.Fatalf("expected owner to fall back to subject, got %q", id.Owner())
	}

	operator := idp.token(t, []string{"/sre"}, "bifrost", 5*time.Minute)
	id2, err := v.Validate(context.Background(), operator)
	if err != nil {
		t.Fatalf("validate operator token: %v", err)
	}
	if !id2.Permits(Write, TargetCluster) {
		t.Fatal("expected operator to write clusters")
	}
	if id2.Permits(Write, TargetJob) {
		t.Fatal("expected operator to NOT write jobs")
	}

	stranger := idp.token(t, []string{"/unrelated-team"}, "bifrost", 5*time.Minute)
	id3, err := v.Validate(context.Background(), stranger)
	if err != nil {
		t.Fatalf("validate stranger token: %v", err)
	}
	if id3.IsAuthorized() {
		t.Fatal("expected deny by default for an unmapped group")
	}
}
