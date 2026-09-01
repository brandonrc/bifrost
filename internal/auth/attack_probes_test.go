package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
)

// Negative tests for the credential-handling boundary in flows.go and
// local.go. Each one pins a property the Rust reference gets for free from
// its type system — serde's required fields, an exhaustive match over an
// enum, a hand-written redacting Debug — and which Go will silently drop
// under a refactor unless a test holds it down.
//
// Several of these FAIL against the code as first written; they encode the
// intended behavior, not the current behavior. Where that is so, the test
// says which finding it belongs to.

// ---------------------------------------------------------------------------
// Finding 1: a 200 response carrying no access_token must not read as success
// ---------------------------------------------------------------------------

// jsonServer serves one fixed status+body on every request.
func jsonServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Rust's TokenResponse.access_token is a required serde field, so a 2xx body
// lacking it is a decode error and the flow fails. Go's decoder leaves the
// field as "" instead, which would turn an IdP's denial — or a proxy that
// rewrote a 4xx into a 200 — into a "successful" flow holding an empty
// bearer that only fails much later, at the first API call.
func TestSuccessResponseWithoutAccessTokenIsRejected(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"access_token":""}`,
		`{"token_type":"Bearer","expires_in":300}`,
		`{"error":"access_denied","error_description":"user declined"}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := jsonServer(t, http.StatusOK, body)

			poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
			if err == nil && poll.Ready {
				got := ""
				if poll.Token != nil {
					got = poll.Token.AccessToken
				}
				t.Errorf("PollDeviceToken reported Ready with access_token=%q for a 2xx body carrying no token", got)
			}

			tok, err := ClientCredentials(context.Background(), srv.Client(), srv.URL, "cid", "secret", nil)
			if err == nil {
				t.Errorf("ClientCredentials succeeded with access_token=%q for a 2xx body carrying no token", tok.AccessToken)
			}

			px := NewTokenExchangeParams("svc", "secret", "subject")
			tok, err = ExchangeToken(context.Background(), srv.Client(), srv.URL, px)
			if err == nil {
				t.Errorf("ExchangeToken succeeded with access_token=%q for a 2xx body carrying no token", tok.AccessToken)
			}
		})
	}
}

// The honest success path must still work — the guard above must reject only
// a missing token, not a real one.
func TestSuccessResponseWithAccessTokenStillSucceeds(t *testing.T) {
	srv := jsonServer(t, http.StatusOK, `{"access_token":"real-token","token_type":"Bearer","expires_in":300}`)

	poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil || !poll.Ready || poll.Token == nil || poll.Token.AccessToken != "real-token" {
		t.Fatalf("poll: got ready=%v err=%v", poll.Ready, err)
	}
	tok, err := ClientCredentials(context.Background(), srv.Client(), srv.URL, "cid", "secret", nil)
	if err != nil || tok.AccessToken != "real-token" {
		t.Fatalf("client credentials: got %v err=%v", tok, err)
	}
}

// ---------------------------------------------------------------------------
// Finding 3: a canceled context must stop the caller's poll loop
// ---------------------------------------------------------------------------

// PollDeviceToken treats a transport failure as transient so a network blip
// doesn't abort a pending device grant. Context cancellation is NOT a blip:
// the caller owns the sleep/deadline loop, so if cancellation comes back
// indistinguishable from authorization_pending, a loop that checks only
// (err, Ready) spins locally until its own deadline — Ctrl-C on `bifrost
// login` would appear to hang.
func TestCanceledContextIsNotReportedAsPending(t *testing.T) {
	srv := jsonServer(t, http.StatusBadRequest, `{"error":"authorization_pending"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	poll, err := PollDeviceToken(ctx, srv.Client(), srv.URL, "cli", "dc")
	if err == nil && !poll.Ready {
		t.Error("a canceled context is indistinguishable from authorization_pending; the caller's poll loop can never learn it was canceled")
	}
}

// The distinction the fix must preserve: a genuine transport failure (no
// listener at all) is still transient, so a real network blip keeps polling.
func TestTransportFailureIsStillTransient(t *testing.T) {
	// Port 1 on loopback: connection refused, immediately.
	poll, err := PollDeviceToken(context.Background(), http.DefaultClient, "http://127.0.0.1:1/token", "cli", "dc")
	if err != nil {
		t.Fatalf("a transport failure must stay transient (keep polling), got %v", err)
	}
	if poll.Ready {
		t.Fatal("a transport failure must not report Ready")
	}
}

// ---------------------------------------------------------------------------
// Finding 4: an unrecognized role column value must deny, not grant Viewer
// ---------------------------------------------------------------------------

// core.LocalRole is a bare string type, and its validating UnmarshalJSON only
// guards JSON ingress — a value scanned straight out of a DB text column
// never passes through it. Rust's exhaustive match over an enum makes this
// case unrepresentable; Go needs a test. Mapping an unknown value to any real
// role is fail-open: the row should authorize nothing.
func TestUnrecognizedLocalRoleDeniesByDefault(t *testing.T) {
	for _, bad := range []core.LocalRole{"", "superuser", "VIEWER", "root", "admin ", "Admin"} {
		t.Run(fmt.Sprintf("%q", string(bad)), func(t *testing.T) {
			id := identityOf(&core.LocalUserRecord{Username: "alice", Role: bad})
			if id.IsAuthorized() {
				t.Errorf("an unrecognized role column value granted %v — it must grant nothing", id.Roles)
			}
			for _, target := range []Target{TargetJob, TargetCluster, TargetService, TargetPool, TargetAudit} {
				for _, action := range []PermissionType{Read, Write, Delete, Admin} {
					if id.Permits(action, target) {
						t.Errorf("unrecognized role permitted %v on %v", action.AsStr(), target.AsStr())
					}
				}
			}
		})
	}
}

// The five real values must still map, so the guard above can't be satisfied
// by breaking the mapping outright.
func TestKnownLocalRolesStillMap(t *testing.T) {
	for role, want := range map[core.LocalRole]Role{
		core.LocalRoleViewer:    RoleViewer,
		core.LocalRoleDeveloper: RoleDeveloper,
		core.LocalRoleOperator:  RoleOperator,
		core.LocalRoleAdmin:     RoleAdmin,
		core.LocalRoleAuditor:   RoleAuditor,
	} {
		id := identityOf(&core.LocalUserRecord{Username: "alice", Role: role})
		if len(id.Roles) != 1 || id.Roles[0] != want {
			t.Errorf("LocalRole(%q) mapped to %v, want [%v]", string(role), id.Roles, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Finding 2 + Minor 5: secret redaction across every fmt verb
// ---------------------------------------------------------------------------

// secretFixtures returns each secret-bearing type alongside the substrings
// that must never appear in any formatted rendering of it.
func secretFixtures() map[string]struct {
	val     any
	secrets []string
} {
	expiresIn := uint64(300)
	refresh := "REFRESH-SECRET"
	tokenType := "Bearer"
	complete := "https://idp/device?code=USER"

	tok := TokenResponse{
		AccessToken: "ACCESS-SECRET", ExpiresIn: &expiresIn,
		RefreshToken: &refresh, TokenType: &tokenType,
	}
	da := DeviceAuthorization{
		DeviceCode: "DEVICE-SECRET", UserCode: "WDJB-MJHT",
		VerificationURI: "https://idp/device", VerificationURIComplete: &complete,
		ExpiresIn: 600, Interval: 5,
	}
	px := NewTokenExchangeParams("checkmaite-svc", "CLIENT-SECRET", "SUBJECT-SECRET")
	mt := MintedToken{Prefix: "abcd1234", Token: "bfr_abcd1234_PLAINTEXT-SECRET", TokenHash: "$2b$12$hash"}
	lo := LoginOutcome{Token: mt, ExpiresAt: 1234, Identity: Identity{Subject: "alice"}}

	return map[string]struct {
		val     any
		secrets []string
	}{
		"TokenResponse":        {tok, []string{"ACCESS-SECRET", "REFRESH-SECRET"}},
		"*TokenResponse":       {&tok, []string{"ACCESS-SECRET", "REFRESH-SECRET"}},
		"DeviceAuthorization":  {da, []string{"DEVICE-SECRET"}},
		"*DeviceAuthorization": {&da, []string{"DEVICE-SECRET"}},
		"TokenExchangeParams":  {px, []string{"CLIENT-SECRET", "SUBJECT-SECRET"}},
		"*TokenExchangeParams": {&px, []string{"CLIENT-SECRET", "SUBJECT-SECRET"}},
		"MintedToken":          {mt, []string{"bfr_abcd1234_PLAINTEXT-SECRET"}},
		"*MintedToken":         {&mt, []string{"bfr_abcd1234_PLAINTEXT-SECRET"}},
		"LoginOutcome":         {lo, []string{"bfr_abcd1234_PLAINTEXT-SECRET"}},
		"*LoginOutcome":        {&lo, []string{"bfr_abcd1234_PLAINTEXT-SECRET"}},
	}
}

// %v, %s and %+v are the verbs that reach production logs — slog's default
// TextHandler renders attribute values with %+v — so a bare struct dump of
// any of these types writes a live bearer credential to the log.
//
// %#v is the one Go's Stringer does NOT cover: it dispatches to GoStringer
// instead, so a type with only a String() method still leaks under it. Rust's
// redacting Debug covers both {:?} and {:#?}, so closing this needs an
// explicit GoString().
func TestSecretsAreRedactedUnderEveryFormatVerb(t *testing.T) {
	for _, verb := range []string{"%v", "%s", "%+v", "%#v"} {
		for name, f := range secretFixtures() {
			out := fmt.Sprintf(verb, f.val)
			for _, secret := range f.secrets {
				if strings.Contains(out, secret) {
					t.Errorf("%s leaked %q under %s: %s", name, secret, verb, out)
				}
			}
			if !strings.Contains(out, "REDACTED") {
				t.Errorf("%s under %s produced no [REDACTED] marker: %s", name, verb, out)
			}
		}
	}
}

// Redaction must not blind the operator: the non-secret fields that make a
// log line useful have to survive.
func TestRedactionKeepsNonSecretFieldsVisible(t *testing.T) {
	f := secretFixtures()
	if got := fmt.Sprintf("%v", f["DeviceAuthorization"].val); !strings.Contains(got, "WDJB-MJHT") {
		t.Errorf("user_code is not secret and must stay visible: %s", got)
	}
	if got := fmt.Sprintf("%v", f["TokenExchangeParams"].val); !strings.Contains(got, "checkmaite-svc") {
		t.Errorf("client_id is not secret and must stay visible: %s", got)
	}
	if got := fmt.Sprintf("%v", f["MintedToken"].val); !strings.Contains(got, "abcd1234") {
		t.Errorf("the lookup prefix is not secret and must stay visible: %s", got)
	}
}

// The never-marshal guards, exercised through the containers a handler would
// realistically reach them by: a pointer, a slice, a wrapper struct, a map.
// json.Marshal propagates a field's MarshalJSON error, so a guard on the leaf
// type protects every shape that embeds it.
func TestSecretBearingTypesRefuseToMarshalThroughContainers(t *testing.T) {
	f := secretFixtures()
	tok, ok := f["TokenResponse"].val.(TokenResponse)
	if !ok {
		t.Fatalf("secretFixtures()[%q].val is not a TokenResponse", "TokenResponse")
	}
	mt, ok := f["MintedToken"].val.(MintedToken)
	if !ok {
		t.Fatalf("secretFixtures()[%q].val is not a MintedToken", "MintedToken")
	}

	for name, val := range map[string]any{
		"TokenResponse":            tok,
		"*TokenResponse":           &tok,
		"DeviceAuthorization":      f["DeviceAuthorization"].val,
		"TokenExchangeParams":      f["TokenExchangeParams"].val,
		"MintedToken":              mt,
		"LoginOutcome":             f["LoginOutcome"].val,
		"[]MintedToken":            []MintedToken{mt},
		"map[string]TokenResponse": map[string]TokenResponse{"a": tok},
		"wrapped MintedToken": struct {
			M MintedToken `json:"m"`
		}{mt},
		"wrapped TokenResponse": struct {
			T TokenResponse `json:"t"`
		}{tok},
	} {
		if b, err := json.Marshal(val); err == nil {
			t.Errorf("%s marshaled instead of refusing: %s", name, b)
		}
	}
}

// ---------------------------------------------------------------------------
// Minor 8: bcrypt's 72-byte boundary
// ---------------------------------------------------------------------------

// bcrypt ignores input past 72 bytes. x/crypto refuses to HASH a longer
// password but still VERIFIES one, silently truncating — so a hash of an
// exactly-72-byte password also accepts that password followed by anything.
// Rejecting over-long input on both sides closes the family.
func TestBcryptSeventyTwoByteBoundary(t *testing.T) {
	const limit = 72

	if _, err := HashPassword(strings.Repeat("a", limit+1)); err == nil {
		t.Error("HashPassword accepted a password longer than bcrypt's 72-byte limit")
	}

	atLimit := strings.Repeat("a", limit)
	hash, err := HashPassword(atLimit)
	if err != nil {
		t.Fatalf("hashing a %d-byte password should succeed: %v", limit, err)
	}
	if !VerifyPassword(atLimit, hash) {
		t.Fatal("the exact password must verify")
	}
	if VerifyPassword(atLimit+"ANY-SUFFIX-AT-ALL", hash) {
		t.Error("a password sharing only the first 72 bytes verified — VerifyPassword must reject input past the bcrypt limit")
	}
}

// A corrupt stored hash must fail closed rather than error out, so one bad
// row can't turn a 401 into a 500.
func TestMalformedStoredHashFailsClosed(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$2b$12$", "$2b$99$" + strings.Repeat("x", 53)} {
		if VerifyPassword("anything", bad) {
			t.Errorf("a malformed stored hash (%q) verified true", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Minor 6: the polling interval must never be zero
// ---------------------------------------------------------------------------

// RFC 8628 §3.5 specifies a default of 5 seconds when the field is omitted
// and does not contemplate zero. The Interval default only fires on ABSENCE,
// so a provider sending an explicit 0 hands the caller a zero-second sleep.
// Whoever owns the poll loop must clamp; this test documents the input.
func TestExplicitZeroIntervalIsSurfaced(t *testing.T) {
	srv := jsonServer(t, http.StatusOK,
		`{"device_code":"dc","user_code":"UC","verification_uri":"https://idp/d","expires_in":600,"interval":0}`)

	da, err := DeviceAuthorize(context.Background(), srv.Client(), srv.URL, "cli", "openid")
	if err != nil {
		t.Fatalf("device authorize: %v", err)
	}
	if da.Interval == 0 {
		t.Log("interval=0 passes through: the poll-loop owner MUST clamp to at least 1s or it will hot-spin the IdP")
	}
	// The absent-field default must still apply.
	srv2 := jsonServer(t, http.StatusOK,
		`{"device_code":"dc","user_code":"UC","verification_uri":"https://idp/d","expires_in":600}`)
	da2, err := DeviceAuthorize(context.Background(), srv2.Client(), srv2.URL, "cli", "openid")
	if err != nil {
		t.Fatalf("device authorize: %v", err)
	}
	if da2.Interval != 5 {
		t.Errorf("an omitted interval must default to 5 (RFC 8628 §3.5), got %d", da2.Interval)
	}
}

// ---------------------------------------------------------------------------
// Standing properties: these hold today and must keep holding
// ---------------------------------------------------------------------------

// Credentials belong in the form body. A secret in the query string lands in
// IdP access logs, proxy logs, and browser history.
func TestSecretsNeverAppearInTheRequestURL(t *testing.T) {
	var sawURL, sawBody, sawContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawURL = r.URL.String()
		sawContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		sawBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"access_token":"ok"}`))
	}))
	defer srv.Close()

	audience := "bifrost"
	px := NewTokenExchangeParams("checkmaite-svc", "CLIENT-SECRET", "SUBJECT-SECRET")
	px.Audience = &audience
	if _, err := ExchangeToken(context.Background(), srv.Client(), srv.URL, px); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if sawContentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", sawContentType)
	}
	for _, secret := range []string{"CLIENT-SECRET", "SUBJECT-SECRET"} {
		if strings.Contains(sawURL, secret) {
			t.Errorf("secret in the request URL: %s", sawURL)
		}
		if !strings.Contains(sawBody, secret) {
			t.Errorf("expected %s in the form body, got %s", secret, sawBody)
		}
	}
	// RFC 8693 requires all four of these on the exchange request.
	for _, param := range []string{
		"grant_type=" + escape(GrantTypeTokenExchange),
		"subject_token_type=" + escape(TokenTypeAccessToken),
		"requested_token_type=" + escape(TokenTypeAccessToken),
		"audience=bifrost",
	} {
		if !strings.Contains(sawBody, param) {
			t.Errorf("missing form parameter %s in %s", param, sawBody)
		}
	}

	if _, err := ClientCredentials(context.Background(), srv.Client(), srv.URL, "cid", "CC-SECRET", nil); err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	if strings.Contains(sawURL, "CC-SECRET") {
		t.Errorf("client_secret in the request URL: %s", sawURL)
	}
}

// escape percent-encodes a URN the way url.Values.Encode does, so the
// assertions above compare against what actually goes on the wire.
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, ":", "%3A"), "/", "%2F")
}

// The token scheme is bfr_ + 8 alphanumerics + _ + 32 hex. Anything else must
// be refused before it ever reaches a store lookup or a bcrypt.
func TestTokenPrefixRejectsMalformedTokens(t *testing.T) {
	valid := "bfr_abcd1234_0123456789abcdef0123456789abcdef"
	if p, ok := TokenPrefix(valid); !ok || p != "abcd1234" {
		t.Fatalf("a well-formed token must parse: got %q, %v", p, ok)
	}
	// Uppercase is accepted in both halves, matching Rust's
	// is_ascii_alphanumeric / is_ascii_hexdigit.
	if _, ok := TokenPrefix("bfr_ABCD1234_0123456789ABCDEF0123456789ABCDEF"); !ok {
		t.Error("uppercase prefix and hex must be accepted (Rust parity)")
	}
	for _, bad := range []string{
		"",
		"bfr_",
		"bfr__",
		valid[:len(valid)-1], // one byte short
		valid + "a",          // one byte long
		"MOB_abcd1234_0123456789abcdef0123456789abcdef", // wrong scheme case
		"nope_abcd1234_0123456789abcdef0123456789abcde", // wrong scheme
		"bfr_abcd1234-0123456789abcdef0123456789abcdef", // dash for underscore
		"bfr_abcd!234_0123456789abcdef0123456789abcdef", // non-alnum prefix
		"bfr_abcd1234_0123456789abcdef0123456789abcdeg", // non-hex suffix
		"bfr_abcd123\x00_0123456789abcdef0123456789abcdef",
		" bfr_abcd1234_0123456789abcdef0123456789abcdef", // leading space
		"bfr_abcd1234_0123456789abcdef0123456789abcde\n", // trailing newline
		"bfr_bfr_1234_0123456789abcdef0123456789abcdef",  // nested scheme
		"bfr_ábcd123_0123456789abcdef0123456789abcdef",   // multi-byte prefix
	} {
		if p, ok := TokenPrefix(bad); ok {
			t.Errorf("TokenPrefix(%q) accepted, returning %q", bad, p)
		}
	}
}

// Minting must be unbiased and collision-free: the 128-bit suffix is the only
// thing standing between an attacker and someone else's PAT.
func TestMintedTokensAreUniqueAndUnbiased(t *testing.T) {
	const mints = 2000
	seen := make(map[string]bool, mints)
	for i := 0; i < mints; i++ {
		prefix, token, err := MintTokenParts()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if len(token) != len("bfr_")+tokenPrefixLen+1+32 {
			t.Fatalf("wrong token length %d: %q", len(token), token)
		}
		if seen[token] {
			t.Fatalf("token collision after %d mints", i)
		}
		seen[token] = true
		if got, ok := TokenPrefix(token); !ok || got != prefix {
			t.Fatalf("minted token failed its own prefix parse: %q", token)
		}
	}

	// Rejection sampling must reach every character; a biased reduction
	// (byte %% 62) would still hit all 62, but a broken alphabet would not.
	counts := map[byte]int{}
	for i := 0; i < 200; i++ {
		s, err := randomAlphanumeric(64)
		if err != nil {
			t.Fatalf("randomAlphanumeric: %v", err)
		}
		for j := 0; j < len(s); j++ {
			counts[s[j]]++
		}
	}
	if len(counts) != len(alphanumericAlphabet) {
		t.Errorf("generator produced %d distinct characters, want %d", len(counts), len(alphanumericAlphabet))
	}
}

// Unknown user, wrong password and disabled account must be indistinguishable
// to a caller: same error text, and each pays exactly one bcrypt so response
// time doesn't enumerate accounts either.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	newAuth := func(rec *core.LocalUserRecord) *LocalAuthenticator {
		return NewLocalAuthenticator(&probeStore{user: rec}, 3600, 90)
	}
	live := &core.LocalUserRecord{Username: "alice", PasswordHash: hash, Role: core.LocalRoleViewer}
	off := &core.LocalUserRecord{Username: "alice", PasswordHash: hash, Role: core.LocalRoleViewer, Disabled: true}

	elapsed := map[string]time.Duration{}
	for _, tc := range []struct {
		name, user, pass string
		rec              *core.LocalUserRecord
	}{
		{"unknown user", "ghost", "correct-horse", live},
		{"wrong password", "alice", "wrong", live},
		{"disabled", "alice", "correct-horse", off},
	} {
		start := time.Now()
		_, err := newAuth(tc.rec).Login(context.Background(), tc.user, tc.pass)
		elapsed[tc.name] = time.Since(start)
		if err == nil {
			t.Fatalf("%s: expected failure", tc.name)
		}
		// Every one of these must cost a bcrypt; a near-instant return means
		// the dummy-hash verify was skipped and timing enumerates accounts.
		if elapsed[tc.name] < 10*time.Millisecond {
			t.Errorf("%s returned in %v — too fast to have paid a bcrypt", tc.name, elapsed[tc.name])
		}
	}
	t.Logf("timings: %v", elapsed)
}

// probeStore is a minimal LocalUserStore holding one user, for tests that
// only need Login's read path.
type probeStore struct{ user *core.LocalUserRecord }

func (s *probeStore) GetLocalUser(_ context.Context, username string) (*core.LocalUserRecord, error) {
	if s.user != nil && s.user.Username == username {
		return s.user, nil
	}
	return nil, nil
}
func (s *probeStore) RecordLoginFailure(context.Context, string) error { return nil }
func (s *probeStore) RecordLoginSuccess(context.Context, string) error { return nil }
func (s *probeStore) CreateApiToken(context.Context, core.ApiTokenRecord) error {
	return nil
}
func (s *probeStore) GetApiTokenByPrefix(context.Context, string) (*core.ApiTokenRecord, error) {
	return nil, nil
}
func (s *probeStore) TouchApiToken(context.Context, string, uint64) error { return nil }
