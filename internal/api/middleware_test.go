package api

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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/golang-jwt/jwt/v5"
)

// captureLogs redirects log/slog's default logger to a buffer for the
// duration of the test, restoring the previous default on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Port of auth_layer.rs's public_allowlist_is_narrow test: the exact
// exemption list, nothing broader.
func TestIsPublicAllowlistIsNarrow(t *testing.T) {
	for _, p := range []string{
		"/healthz",
		"/api/v1/version",
		SpecPath,
		"/docs",
		"/docs/x",
		"/api/v1/auth/login",
		"/api/v1/auth/providers",
	} {
		if !isPublic(p) {
			t.Errorf("isPublic(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"/api/jobs/",
		"/api/v1/authz/check",
		"/docsx",
		"/",
		"/api/v1/auth/tokens",
		"/api/v1/auth/logout",
		"/api/v1/auth/login/evil",
	} {
		if isPublic(p) {
			t.Errorf("isPublic(%q) = true, want false", p)
		}
	}
}

// Port of auth_layer.rs's jwt_and_pat_shapes_are_unambiguous test.
func TestIsJWTShaped(t *testing.T) {
	if !isJWTShaped("aaa.bbb.ccc") {
		t.Error("aaa.bbb.ccc should be JWT-shaped")
	}
	if isJWTShaped("mob_abcd1234_0123456789abcdef0123456789abcdef") {
		t.Error("a mob_ PAT should not be JWT-shaped")
	}
	if isJWTShaped("garbage") {
		t.Error("garbage should not be JWT-shaped")
	}
	if isJWTShaped("too.many.dots.here") {
		t.Error("too.many.dots.here should not be JWT-shaped")
	}
}

func TestRequireAuth_DevModePassesEverythingThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := IdentityFromContext(r.Context()); ok {
			t.Error("dev mode should attach no identity")
		}
		w.WriteHeader(http.StatusOK)
	})
	h := RequireAuth(AuthState{})(next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireAuth_PublicPathBypassesEvenWhenConfigured(t *testing.T) {
	local := auth.NewLocalAuthenticator(newFakeUserStore(), 3600, 30)
	h := RequireAuth(AuthState{Local: local})(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireAuth_MissingBearerIsDenied(t *testing.T) {
	buf := captureLogs(t)
	local := auth.NewLocalAuthenticator(newFakeUserStore(), 3600, 30)
	h := RequireAuth(AuthState{Local: local})(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Error("expected WWW-Authenticate: Bearer")
	}
	assertErrorBody(t, rec, "missing_token")

	logged := buf.String()
	for _, want := range []string{"decision=deny", "reason=missing_token", "method=GET", "path=/api/v1/clusters"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q, got: %s", want, logged)
		}
	}
}

func TestRequireAuth_InvalidBearerIsDenied(t *testing.T) {
	buf := captureLogs(t)
	local := auth.NewLocalAuthenticator(newFakeUserStore(), 3600, 30)
	h := RequireAuth(AuthState{Local: local})(okHandler())

	const badToken = "mob_totallybogus_0000000000000000000000000000"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+badToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	assertErrorBody(t, rec, "invalid_token")

	logged := buf.String()
	for _, want := range []string{"decision=deny", "reason=invalid_token", "method=GET", "path=/api/v1/clusters"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q, got: %s", want, logged)
		}
	}
	if strings.Contains(logged, badToken) {
		t.Errorf("log output must never contain token contents, got: %s", logged)
	}
}

func TestRequireAuth_ValidLocalPATIsAccepted(t *testing.T) {
	store := newFakeUserStore()
	store.users["alice"] = &core.LocalUserRecord{Username: "alice", Role: core.LocalRoleAdmin}
	local := auth.NewLocalAuthenticator(store, 3600, 30)
	minted, _, err := local.IssueToken(context.Background(), "alice", "test", 1)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	var gotSubject string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFromContext(r.Context())
		if !ok {
			t.Error("expected an identity in context")
		} else {
			gotSubject = id.Subject
		}
		w.WriteHeader(http.StatusOK)
	})
	h := RequireAuth(AuthState{Local: local})(next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotSubject != "alice" {
		t.Errorf("identity subject = %q, want alice", gotSubject)
	}
}

func TestRequireAuth_ValidatorPath(t *testing.T) {
	idp := newFakeIdp(t)
	v := idp.discover(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFromContext(r.Context())
		if !ok || id.Subject != "user-123" {
			t.Errorf("identity = %+v, ok=%v", id, ok)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := RequireAuth(AuthState{Validator: v})(next)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestRequireAuth_ValidatorRejectsGarbageJWT(t *testing.T) {
	idp := newFakeIdp(t)
	v := idp.discover(t)
	h := RequireAuth(AuthState{Validator: v})(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// --- fail-closed guards ---

func TestCheckBindAllowed(t *testing.T) {
	loopback := net.ParseIP("127.0.0.1")
	remote := net.ParseIP("10.0.0.5")

	cases := []struct {
		name                 string
		ip                   net.IP
		authConfigured       bool
		allowUnauthenticated bool
		wantErr              bool
	}{
		{"loopback, no auth", loopback, false, false, false},
		{"remote, no auth", remote, false, false, true},
		{"remote, auth configured", remote, true, false, false},
		{"remote, explicitly allowed", remote, false, true, false},
		{"nil ip (unresolvable), no auth", nil, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckBindAllowed(c.ip, c.authConfigured, c.allowUnauthenticated)
			if c.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr && !errors.Is(err, ErrAuthNotConfigured) {
				t.Errorf("error %v does not wrap ErrAuthNotConfigured", err)
			}
		})
	}
}

func TestRefuseNonLoopback(t *testing.T) {
	buf := captureLogs(t)
	h := RefuseNonLoopback(okHandler())

	loopbackReq := httptest.NewRequest(http.MethodGet, "/anything", nil)
	loopbackReq.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback peer: status = %d, want 200", rec.Code)
	}

	remoteReq := httptest.NewRequest(http.MethodGet, "/anything", nil)
	remoteReq.RemoteAddr = "203.0.113.7:54321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remoteReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote peer: status = %d, want 403", rec.Code)
	}
	assertErrorBody(t, rec, "unauthenticated_non_loopback")

	logged := buf.String()
	for _, want := range []string{"level=WARN", "decision=deny", "reason=unauthenticated_non_loopback", "203.0.113.7"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q, got: %s", want, logged)
		}
	}
	if strings.Contains(logged, "127.0.0.1:54321") {
		t.Errorf("the allowed loopback request should not have logged a denial, got: %s", logged)
	}
}

// --- test helpers ---

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func assertErrorBody(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, rec.Body.String())
	}
	if body.Error != wantCode {
		t.Errorf("error code = %q, want %q", body.Error, wantCode)
	}
}

// fakeUserStore is a minimal in-memory auth.LocalUserStore for tests.
type fakeUserStore struct {
	mu     sync.Mutex
	users  map[string]*core.LocalUserRecord
	tokens map[string]*core.ApiTokenRecord
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{users: map[string]*core.LocalUserRecord{}, tokens: map[string]*core.ApiTokenRecord{}}
}

func (s *fakeUserStore) GetLocalUser(_ context.Context, username string) (*core.LocalUserRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.users[username], nil
}

func (s *fakeUserStore) RecordLoginFailure(_ context.Context, _ string) error { return nil }
func (s *fakeUserStore) RecordLoginSuccess(_ context.Context, _ string) error { return nil }

func (s *fakeUserStore) CreateApiToken(_ context.Context, record core.ApiTokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := record
	s.tokens[record.Prefix] = &r
	return nil
}

func (s *fakeUserStore) GetApiTokenByPrefix(_ context.Context, prefix string) (*core.ApiTokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[prefix], nil
}

func (s *fakeUserStore) TouchApiToken(_ context.Context, prefix string, now uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.tokens[prefix]; ok {
		t := now
		r.LastUsedAt = &t
	}
	return nil
}

// fakeIdp is a minimal mock OIDC issuer (discovery + JWKS over a fresh
// RSA key), enough to exercise RequireAuth's Validator path without
// duplicating internal/auth's own (package-private) validator test
// fixtures.
type fakeIdp struct {
	server *httptest.Server
	issuer string
	priv   *rsa.PrivateKey
	kid    string
}

func newFakeIdp(t *testing.T) *fakeIdp {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := &fakeIdp{priv: priv, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   idp.issuer,
			"jwks_uri": idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
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

func (idp *fakeIdp) discover(t *testing.T) *auth.Validator {
	t.Helper()
	cfg := auth.AuthConfig{Issuer: idp.issuer, Audience: "bifrost", GroupsClaim: "groups"}
	v, err := auth.Discover(context.Background(), cfg, auth.IdpClient(), true)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return v
}

func (idp *fakeIdp) token(t *testing.T) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user-123",
		"iss": idp.issuer,
		"aud": "bifrost",
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	})
	tok.Header["kid"] = idp.kid
	s, err := tok.SignedString(idp.priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}
