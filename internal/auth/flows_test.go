package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Redaction tests (ported from the predecessor's auth crate, src/flows.rs:326-365) ---

func TestTokenExchangeParamsStringRedactsSecrets(t *testing.T) {
	aud := "bifrost"
	x := NewTokenExchangeParams("checkmaite-svc", "svc-secret", "user-subject-token")
	x.Audience = &aud
	s := x.String()
	if strings.Contains(s, "svc-secret") {
		t.Fatalf("client secret leaked: %s", s)
	}
	if strings.Contains(s, "user-subject-token") {
		t.Fatalf("subject token leaked: %s", s)
	}
	if !strings.Contains(s, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", s)
	}
	// Non-secret fields stay visible for debugging.
	if !strings.Contains(s, "checkmaite-svc") {
		t.Fatalf("client_id should stay visible: %s", s)
	}
	if !strings.Contains(s, "bifrost") {
		t.Fatalf("audience should stay visible: %s", s)
	}
	if x.SubjectTokenType != TokenTypeAccessToken {
		t.Fatalf("expected default subject_token_type, got %q", x.SubjectTokenType)
	}
}

func TestDebugRedactsSecrets(t *testing.T) {
	expires := uint64(300)
	refresh := "refresh-secret"
	tokenType := "Bearer"
	tr := TokenResponse{
		AccessToken:  "super-secret-token",
		ExpiresIn:    &expires,
		RefreshToken: &refresh,
		TokenType:    &tokenType,
	}
	s := tr.String()
	if strings.Contains(s, "super-secret-token") {
		t.Fatalf("access token leaked: %s", s)
	}
	if strings.Contains(s, "refresh-secret") {
		t.Fatalf("refresh token leaked: %s", s)
	}
	if !strings.Contains(s, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", s)
	}

	da := DeviceAuthorization{
		DeviceCode:      "device-secret",
		UserCode:        "WDJB-MJHT",
		VerificationURI: "https://idp/device",
		ExpiresIn:       600,
		Interval:        5,
	}
	s = da.String()
	if strings.Contains(s, "device-secret") {
		t.Fatalf("device code leaked: %s", s)
	}
	if !strings.Contains(s, "WDJB-MJHT") {
		t.Fatalf("user_code is not secret, should be visible: %s", s)
	}
}

// --- Never-marshal guards ---

func TestSecretBearingTypesNeverMarshal(t *testing.T) {
	if _, err := json.Marshal(DeviceAuthorization{DeviceCode: "d"}); err == nil {
		t.Fatal("expected DeviceAuthorization to refuse marshal")
	}
	if _, err := json.Marshal(TokenResponse{AccessToken: "a"}); err == nil {
		t.Fatal("expected TokenResponse to refuse marshal")
	}
	if _, err := json.Marshal(NewTokenExchangeParams("c", "s", "t")); err == nil {
		t.Fatal("expected TokenExchangeParams to refuse marshal")
	}
	if _, err := json.Marshal(MintedToken{Token: "bfr_x"}); err == nil {
		t.Fatal("expected MintedToken to refuse marshal")
	}
}

// --- DeviceAuthorization interval default ---

func TestDeviceAuthorizationIntervalDefaultsTo5(t *testing.T) {
	var da DeviceAuthorization
	if err := json.Unmarshal([]byte(`{"device_code":"d","user_code":"u","verification_uri":"https://x","expires_in":600}`), &da); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if da.Interval != 5 {
		t.Fatalf("expected default interval 5, got %d", da.Interval)
	}

	var da2 DeviceAuthorization
	if err := json.Unmarshal([]byte(`{"device_code":"d","user_code":"u","verification_uri":"https://x","expires_in":600,"interval":10}`), &da2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if da2.Interval != 10 {
		t.Fatalf("expected explicit interval 10, got %d", da2.Interval)
	}
}

// --- HTTP-level behavior (Go-specific: the Rust reference has no
// transport-level tests in flows.rs, only the redaction tests above; the
// polling state machine and error handling below are new coverage for the
// port). ---

func TestDeviceAuthorizeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("client_id") != "cli" || r.FormValue("scope") != "openid" {
			t.Fatalf("unexpected form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"https://idp/device","expires_in":600}`))
	}))
	defer srv.Close()

	da, err := DeviceAuthorize(context.Background(), srv.Client(), srv.URL, "cli", "openid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if da.DeviceCode != "dc" || da.UserCode != "UC" || da.Interval != 5 {
		t.Fatalf("unexpected result: %+v", da)
	}
}

func TestDeviceAuthorizeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	_, err := DeviceAuthorize(context.Background(), srv.Client(), srv.URL, "cli", "openid")
	if err == nil {
		t.Fatal("expected error")
	}
	var ae AuthError
	if !errors.As(err, &ae) || ae.Kind != AuthErrFlow {
		t.Fatalf("expected AuthErrFlow, got %v", err)
	}
}

func TestPollDeviceTokenPendingAndSlowDown(t *testing.T) {
	responses := []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`},
		{http.StatusBadRequest, `{"error":"slow_down"}`},
	}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := responses[i]
		i++
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	defer srv.Close()

	poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if poll.Ready || poll.SlowDown {
		t.Fatalf("expected plain pending, got %+v", poll)
	}

	poll, err = PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if poll.Ready || !poll.SlowDown {
		t.Fatalf("expected slow_down pending, got %+v", poll)
	}
}

func TestPollDeviceTokenReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer"}`))
	}))
	defer srv.Close()

	poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !poll.Ready || poll.Token == nil || poll.Token.AccessToken != "at" {
		t.Fatalf("expected ready token, got %+v", poll)
	}
}

func TestPollDeviceTokenTerminalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user declined"}`))
	}))
	defer srv.Close()

	_, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err == nil {
		t.Fatal("expected terminal error")
	}
	var ae AuthError
	if !errors.As(err, &ae) || ae.Kind != AuthErrFlow {
		t.Fatalf("expected AuthErrFlow, got %v", err)
	}
	if !strings.Contains(ae.Message, "access_denied") || !strings.Contains(ae.Message, "user declined") {
		t.Fatalf("expected message to carry error+description, got %q", ae.Message)
	}
}

func TestPollDeviceTokenTransientTransportErrorKeepsPolling(t *testing.T) {
	// A connection to a closed server: transient (#22), yields Pending,
	// not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil {
		t.Fatalf("transient transport failure must not be a hard error: %v", err)
	}
	if poll.Ready || poll.SlowDown {
		t.Fatalf("expected plain pending on transport failure, got %+v", poll)
	}
}

func TestPollDeviceTokenMalformedNon2xxBodyIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
	}))
	defer srv.Close()

	poll, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err != nil {
		t.Fatalf("malformed non-2xx body must be treated as transient: %v", err)
	}
	if poll.Ready {
		t.Fatalf("expected pending, got %+v", poll)
	}
}

func TestPollDeviceTokenMalformed2xxBodyIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	_, err := PollDeviceToken(context.Background(), srv.Client(), srv.URL, "cli", "dc")
	if err == nil {
		t.Fatal("expected error: IdP claimed success but body is unreadable")
	}
}

func TestClientCredentialsSuccessAndError(t *testing.T) {
	ok := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("grant_type") != "client_credentials" {
			t.Fatalf("unexpected grant_type: %v", r.Form)
		}
		if ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"svc-token"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	defer srv.Close()

	scope := "clusters:write"
	tok, err := ClientCredentials(context.Background(), srv.Client(), srv.URL, "svc", "secret", &scope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "svc-token" {
		t.Fatalf("unexpected token: %+v", tok)
	}

	ok = false
	_, err = ClientCredentials(context.Background(), srv.Client(), srv.URL, "svc", "wrong", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "bad secret") {
		t.Fatalf("expected error+description in message, got %q", err.Error())
	}
}

func TestExchangeTokenSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("grant_type") != GrantTypeTokenExchange {
			t.Fatalf("unexpected grant_type: %v", r.Form)
		}
		if r.FormValue("subject_token") != "user-token" {
			t.Fatalf("unexpected subject_token: %v", r.Form)
		}
		if r.FormValue("audience") != "bifrost" {
			t.Fatalf("unexpected audience: %v", r.Form)
		}
		if r.FormValue("requested_token_type") != TokenTypeAccessToken {
			t.Fatalf("unexpected requested_token_type: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"exchanged"}`))
	}))
	defer srv.Close()

	aud := "bifrost"
	params := NewTokenExchangeParams("checkmaite-svc", "svc-secret", "user-token")
	params.Audience = &aud
	tok, err := ExchangeToken(context.Background(), srv.Client(), srv.URL, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "exchanged" {
		t.Fatalf("unexpected token: %+v", tok)
	}
}

// A deadline that fires mid-request must be reported as a real error, NOT
// swallowed as a transient transport hiccup — see finding I3 (fix round
// 1): the caller's poll loop must be able to tell "the IdP blipped, keep
// polling" apart from "my own deadline/cancellation fired, stop." Before
// the fix this asserted the OPPOSITE (that cancellation reads as
// transient); TestCanceledContextIsNotReportedAsPending in
// attack_probes_test.go covers the already-canceled-before-the-call case,
// this one covers a deadline that expires WHILE the request is in flight.
func TestPollDeviceTokenContextDeadlineDuringRequestIsAHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	poll, err := PollDeviceToken(ctx, srv.Client(), srv.URL, "cli", "dc")
	if err == nil {
		t.Fatal("expected the deadline to surface as an error, not transient Pending")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected errors.Is(err, context.DeadlineExceeded), got %v", err)
	}
	if poll.Ready {
		t.Fatalf("expected a non-ready poll, got %+v", poll)
	}
}
