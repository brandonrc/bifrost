// OAuth 2.0 token-acquisition flows (ADR-0003 Phase 2 port of
// the predecessor's auth crate, src/flows.rs).
//
// Humans: Device Authorization Grant (RFC 8628) — `bifrost login` prints a
// code, the user approves in a browser, the CLI polls for the token.
// Machines: Client Credentials Grant (RFC 6749 §4.4) — service accounts
// exchange id/secret for a token; Bifrost never mints tokens itself.
// On-behalf-of: Token Exchange (RFC 8693) — a trusted service swaps its own
// credentials plus a user's token for a short-lived token that carries the
// USER as subject, so jobs submitted through the gateway attribute to the
// human, not the service account (#102, checkmaite-frontend#25).
//
// Reference: the predecessor's auth crate, src/flows.rs (retired Rust project; file:line
// cites below are for that file, kept here only as a build note).
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DeviceAuthorization is the response of a device authorization request
// (RFC 8628 §3.2).
//
// DeviceCode is a bearer-equivalent secret while the grant is pending
// (#33): it alone lets a holder poll the token endpoint and, once the user
// approves, claim the resulting token. MarshalJSON refuses to serialize
// this type for that reason (the never-marshal guard used throughout this
// codebase for secret-bearing types); String redacts it too so an
// accidental %v/%s log line can't leak it.
//
// Reference: the predecessor's auth crate, src/flows.rs:16-46 (DeviceAuthorization).
type DeviceAuthorization struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	// VerificationURIComplete is the URI with the user_code pre-filled, if
	// the provider offers one (e.g. for a QR code).
	VerificationURIComplete *string `json:"verification_uri_complete,omitempty"`
	// ExpiresIn is the number of seconds until the codes expire.
	ExpiresIn uint64 `json:"expires_in"`
	// Interval is the suggested polling interval in seconds (RFC 8628
	// default 5, applied when the provider omits the field).
	Interval uint64 `json:"interval"`
}

// deviceAuthorizationAlias breaks the recursion UnmarshalJSON would
// otherwise cause.
type deviceAuthorizationAlias DeviceAuthorization

// UnmarshalJSON defaults Interval to 5 when the field is absent, mirroring
// flows.rs:26-32's #[serde(default = "default_interval")].
func (d *DeviceAuthorization) UnmarshalJSON(data []byte) error {
	a := deviceAuthorizationAlias{Interval: 5}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*d = DeviceAuthorization(a)
	return nil
}

// MarshalJSON always fails: DeviceAuthorization carries DeviceCode, a
// bearer-equivalent secret while the grant is pending. The Rust reference
// derives Deserialize only (never Serialize) for this exact reason; this
// is the runtime equivalent of that compile-time refusal.
func (DeviceAuthorization) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: DeviceAuthorization must never be marshaled — device_code is a bearer-equivalent secret while the grant is pending")
}

// String redacts DeviceCode, mirroring flows.rs:36-46's custom (redacting)
// Debug impl. UserCode and the other fields are not secret.
func (d DeviceAuthorization) String() string {
	complete := "<nil>"
	if d.VerificationURIComplete != nil {
		complete = fmt.Sprintf("%q", *d.VerificationURIComplete)
	}
	return fmt.Sprintf(
		"DeviceAuthorization{device_code: [REDACTED], user_code: %q, verification_uri: %q, verification_uri_complete: %s, expires_in: %d, interval: %d}",
		d.UserCode, d.VerificationURI, complete, d.ExpiresIn, d.Interval,
	)
}

// GoString redacts the same way as String. %#v dispatches to GoStringer
// instead of Stringer, so without this a type with only String() still
// leaks DeviceCode under %#v — Rust's redacting Debug covers both {:?} and
// {:#?}; this and String together are its Go equivalent.
func (d DeviceAuthorization) GoString() string { return d.String() }

// TokenResponse is an OAuth 2.0 token response. AccessToken and
// RefreshToken are bearer secrets (#33): MarshalJSON refuses to serialize
// this type (mirroring flows.rs's Deserialize-only derive) and String
// redacts both fields for safe logging.
//
// Reference: the predecessor's auth crate, src/flows.rs:48-72 (TokenResponse).
type TokenResponse struct {
	AccessToken  string  `json:"access_token"`
	ExpiresIn    *uint64 `json:"expires_in,omitempty"`
	RefreshToken *string `json:"refresh_token,omitempty"`
	TokenType    *string `json:"token_type,omitempty"`
}

// MarshalJSON always fails; see DeviceAuthorization.MarshalJSON for the
// rationale — access_token/refresh_token are bearer secrets.
func (TokenResponse) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: TokenResponse must never be marshaled — access_token/refresh_token are bearer secrets")
}

// String redacts AccessToken and RefreshToken, mirroring flows.rs:60-71's
// custom Debug impl.
func (t TokenResponse) String() string {
	expires := "<nil>"
	if t.ExpiresIn != nil {
		expires = fmt.Sprintf("%d", *t.ExpiresIn)
	}
	refresh := "<nil>"
	if t.RefreshToken != nil {
		refresh = "[REDACTED]"
	}
	tokenType := "<nil>"
	if t.TokenType != nil {
		tokenType = fmt.Sprintf("%q", *t.TokenType)
	}
	return fmt.Sprintf(
		"TokenResponse{access_token: [REDACTED], expires_in: %s, refresh_token: %s, token_type: %s}",
		expires, refresh, tokenType,
	)
}

// GoString redacts the same way as String; see
// DeviceAuthorization.GoString for why this is needed alongside String.
func (t TokenResponse) GoString() string { return t.String() }

// tokenErrorBody is an RFC 6749 §5.2 error response body.
type tokenErrorBody struct {
	Error            string  `json:"error"`
	ErrorDescription *string `json:"error_description,omitempty"`
}

func (e tokenErrorBody) description() string {
	if e.ErrorDescription != nil {
		return *e.ErrorDescription
	}
	return ""
}

// parseTokenError decodes a token-endpoint RFC 6749 §5.2 error body. ok is
// false when the body doesn't decode to a well-formed error: a JSON parse
// failure, OR a body that parses but leaves "error" missing/empty. The
// Rust reference's TokenError.error has no #[serde(default)], so a body
// missing that field fails deserialize entirely there — this is the Go
// equivalent of that same required-field failure, shared by every call
// site that reads an error body so none of them can produce a stray
// "<empty>: <empty>" message from an unparseable body.
//
// What "not ok" means differs by caller: PollDeviceToken treats it as
// transient (no terminal grant error was legible, so keep polling — #22);
// doTokenRequest's callers (ClientCredentials, ExchangeToken) have no such
// fallback and treat it as fatal. See each call site.
func parseTokenError(body []byte) (tokenErrorBody, bool) {
	var tokErr tokenErrorBody
	if err := json.Unmarshal(body, &tokErr); err != nil || tokErr.Error == "" {
		return tokenErrorBody{}, false
	}
	return tokErr, true
}

// decodeTokenResponse decodes a 2xx token response body, rejecting one
// carrying no access_token. AccessToken is a required field in the Rust
// reference's serde struct (no #[serde(default)]) — a 2xx body missing it,
// or explicitly empty, fails deserialize there. json.Unmarshal has no
// required-field concept and would otherwise leave AccessToken as "",
// silently turning an IdP denial re-encoded with a 200 status (or any body
// that merely omits the field) into a "successful" flow holding an empty
// bearer that only fails much later, at the first API call.
func decodeTokenResponse(body []byte) (*TokenResponse, error) {
	var tok TokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	if tok.AccessToken == "" {
		return nil, flowErrf(nil, "token response carried no access_token")
	}
	return &tok, nil
}

// flowErrf builds an AuthErrFlow AuthError, optionally wrapping source for
// errors.Unwrap.
func flowErrf(source error, format string, args ...any) error {
	return AuthError{Kind: AuthErrFlow, Message: fmt.Sprintf(format, args...), Source: source}
}

// postForm POSTs form-urlencoded values and returns the raw response, or a
// flow error if the request couldn't even be sent.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	return resp, nil
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

// DeviceAuthorize starts a device authorization (RFC 8628 §3.1).
//
// Reference: the predecessor's auth crate, src/flows.rs:82-98 (device_authorize).
func DeviceAuthorize(ctx context.Context, client *http.Client, deviceAuthorizationEndpoint, clientID, scope string) (*DeviceAuthorization, error) {
	resp, err := postForm(ctx, client, deviceAuthorizationEndpoint, url.Values{
		"client_id": {clientID},
		"scope":     {scope},
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if !isSuccess(resp.StatusCode) {
		// Mirror reqwest's `.error_for_status().without_url()`: status line
		// only — no response body, no URL. device_authorize is a one-shot
		// request with no TokenError-decoding fallback (unlike the
		// token-endpoint calls below), matching the Rust reference exactly.
		return nil, flowErrf(nil, "device authorization request failed: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	var da DeviceAuthorization
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	return &da, nil
}

// DevicePoll is the result of one poll of the token endpoint for a pending
// device grant (RFC 8628 §3.4/§3.5). Exactly one of two shapes holds:
//   - Ready == false: still pending. SlowDown reports whether the IdP
//     asked the caller to widen its polling interval (RFC 8628 §3.5: add
//     5s and retry) — the caller owns the sleep/deadline loop, this
//     function stays testable and runtime-agnostic.
//   - Ready == true: Token is set to the acquired TokenResponse.
//
// Go equivalent of flows.rs:103-110's DevicePoll enum (Pending{slow_down}
// | Ready(TokenResponse)).
type DevicePoll struct {
	Ready    bool
	SlowDown bool
	Token    *TokenResponse
}

// PollDeviceToken performs one poll of the token endpoint for a pending
// device grant (RFC 8628 §3.4/§3.5).
//
// A transport hiccup (IdP/ingress blip, DNS, reset) is transient — the
// caller should keep polling until its own deadline rather than aborting
// the whole device flow (#22), so a send failure yields
// DevicePoll{Ready:false} with a nil error, not an error return — UNLESS
// ctx is what caused the send to fail: cancellation is not a blip, it's
// the caller's own poll loop asking to stop, and if it read back
// indistinguishably from authorization_pending that loop could never learn
// it was canceled (Ctrl-C on `bifrost login` would appear to hang). ctx's
// own error is returned in that case, checked before the transient
// fallback. The same transient-on-anything-illegible leniency applies to a
// non-2xx response whose body isn't a well-formed RFC 8628 error (a 502
// HTML page from an ingress, a truncated body, one with an empty/missing
// "error" field): treated as transient, not fatal. A malformed 2xx body,
// OR a 2xx body carrying no access_token, IS fatal — the IdP claims
// success but the token is unusable.
//
// Reference: the predecessor's auth crate, src/flows.rs:112-159 (poll_device_token).
func PollDeviceToken(ctx context.Context, client *http.Client, tokenEndpoint, clientID, deviceCode string) (DevicePoll, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {clientID},
	}.Encode()))
	if err != nil {
		return DevicePoll{}, flowErrf(err, "%s", err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return DevicePoll{}, ctxErr
		}
		// Transient transport hiccup (IdP/ingress blip, DNS, reset) — keep
		// polling until the caller's deadline (#22).
		return DevicePoll{}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)

	if isSuccess(resp.StatusCode) {
		if readErr != nil {
			return DevicePoll{}, flowErrf(readErr, "%s", readErr.Error())
		}
		tok, err := decodeTokenResponse(body)
		if err != nil {
			return DevicePoll{}, err
		}
		return DevicePoll{Ready: true, Token: tok}, nil
	}

	// Non-2xx: a well-formed RFC 8628 error acts; anything else is
	// transient (#22).
	if readErr != nil {
		return DevicePoll{}, nil
	}
	tokErr, ok := parseTokenError(body)
	if !ok {
		return DevicePoll{}, nil
	}
	switch tokErr.Error {
	case "authorization_pending":
		return DevicePoll{SlowDown: false}, nil
	case "slow_down":
		return DevicePoll{SlowDown: true}, nil
	default:
		return DevicePoll{}, flowErrf(nil, "%s: %s", tokErr.Error, tokErr.description())
	}
}

// ClientCredentials performs a client-credentials grant for service
// accounts (RFC 6749 §4.4).
//
// Reference: the predecessor's auth crate, src/flows.rs:162-197 (client_credentials).
func ClientCredentials(ctx context.Context, client *http.Client, tokenEndpoint, clientID, clientSecret string, scope *string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	if scope != nil {
		form.Set("scope", *scope)
	}
	return doTokenRequest(ctx, client, tokenEndpoint, form)
}

// RFC 8693 constants.
const (
	// GrantTypeTokenExchange is the RFC 8693 grant type.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// TokenTypeAccessToken is the RFC 8693 token-type URN for an OAuth 2.0
	// access token.
	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	// TokenTypeIDToken is the RFC 8693 token-type URN for an OIDC ID
	// token.
	TokenTypeIDToken = "urn:ietf:params:oauth:token-type:id_token"
)

// TokenExchangeParams holds the parameters for an RFC 8693 token exchange
// (see ExchangeToken). Construct with NewTokenExchangeParams (defaults
// SubjectTokenType to an access token), then set Audience/Scope.
//
// ClientSecret and SubjectToken are bearer-equivalent secrets (#33) — the
// subject token is the user's live credential. MarshalJSON refuses to
// serialize this type (it's a form-post parameter bag, never a wire DTO)
// and String redacts both fields.
//
// Reference: the predecessor's auth crate, src/flows.rs:206-264 (TokenExchange).
type TokenExchangeParams struct {
	// ClientID is the requesting (trusted) service's confidential client
	// id — the service that holds the user's gateway-verified token and
	// is submitting on their behalf (e.g. "checkmaite-svc").
	ClientID string
	// ClientSecret is the requesting service's client secret.
	ClientSecret string
	// SubjectToken is the token to exchange: the user's gateway-verified
	// access token (or id token), typically lifted from the gateway
	// session cookie. The exchanged token inherits THIS token's identity
	// — its sub becomes the exchanged token's subject, which is the whole
	// point (#102): the resulting token is the user's, not the service's.
	SubjectToken string
	// SubjectTokenType is the RFC 8693 type URN of SubjectToken:
	// TokenTypeAccessToken (the NewTokenExchangeParams default) or
	// TokenTypeIDToken.
	SubjectTokenType string
	// Audience is the requested audience for the exchanged token
	// (Keycloak's "audience" form field). Set this to Bifrost's
	// audience/client id so the result validates against the gateway with
	// aud=bifrost.
	Audience *string
	// Scope is the optional requested scope for the exchanged token.
	Scope *string
}

// NewTokenExchangeParams builds a token-exchange request for subjectToken
// (typed as an access token), authenticated by the given confidential
// client. Set Audience to the Bifrost audience before exchanging.
func NewTokenExchangeParams(clientID, clientSecret, subjectToken string) TokenExchangeParams {
	return TokenExchangeParams{
		ClientID:         clientID,
		ClientSecret:     clientSecret,
		SubjectToken:     subjectToken,
		SubjectTokenType: TokenTypeAccessToken,
	}
}

// MarshalJSON always fails: ClientSecret and SubjectToken are secrets, and
// this type is a form-post parameter bag, not a wire DTO.
func (TokenExchangeParams) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: TokenExchangeParams must never be marshaled — client_secret/subject_token are secrets")
}

// String redacts ClientSecret and SubjectToken, mirroring flows.rs's
// custom (redacting) Debug impl for TokenExchange.
func (p TokenExchangeParams) String() string {
	audience := "<nil>"
	if p.Audience != nil {
		audience = fmt.Sprintf("%q", *p.Audience)
	}
	scope := "<nil>"
	if p.Scope != nil {
		scope = fmt.Sprintf("%q", *p.Scope)
	}
	return fmt.Sprintf(
		"TokenExchangeParams{client_id: %q, client_secret: [REDACTED], subject_token: [REDACTED], subject_token_type: %q, audience: %s, scope: %s}",
		p.ClientID, p.SubjectTokenType, audience, scope,
	)
}

// GoString redacts the same way as String; see
// DeviceAuthorization.GoString for why this is needed alongside String.
func (p TokenExchangeParams) GoString() string { return p.String() }

// ExchangeToken performs an RFC 8693 OAuth 2.0 Token Exchange. A trusted
// service swaps its own client credentials plus a user's SubjectToken for
// a NEW token whose subject is the USER, scoped to the requested
// Audience.
//
// Bifrost uses this so a service submitting jobs on a human's behalf (e.g.
// checkmaite's api, #102 / checkmaite-frontend#25) obtains a short-lived,
// bifrost-audience token that carries the human as sub. The service then
// submits that token through the gateway and Bifrost attributes the job to
// the real user rather than the shared service account — closing the
// created_by spoof at its root. Bifrost itself mints nothing: the IdP
// performs the exchange and Bifrost validates the result like any other
// bearer (aud/iss/exp + JWKS signature).
//
// The requested_token_type is always an access token — that is what the
// `ray job submit` bearer path and the gateway consume.
//
// Reference: the predecessor's auth crate, src/flows.rs:266-320 (exchange_token).
func ExchangeToken(ctx context.Context, client *http.Client, tokenEndpoint string, params TokenExchangeParams) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":           {GrantTypeTokenExchange},
		"client_id":            {params.ClientID},
		"client_secret":        {params.ClientSecret},
		"subject_token":        {params.SubjectToken},
		"subject_token_type":   {params.SubjectTokenType},
		"requested_token_type": {TokenTypeAccessToken},
	}
	if params.Audience != nil {
		form.Set("audience", *params.Audience)
	}
	if params.Scope != nil {
		form.Set("scope", *params.Scope)
	}
	return doTokenRequest(ctx, client, tokenEndpoint, form)
}

// doTokenRequest is the shared POST-form-then-decode-TokenResponse path
// used by ClientCredentials and ExchangeToken: unlike PollDeviceToken,
// both a network failure and a decode failure (success or error path) are
// real errors here — there is no "keep polling" fallback for a one-shot
// grant. That includes an error body with an unreadable/empty "error"
// field (parseTokenError's ok=false): PollDeviceToken can shrug that off
// as transient, but a one-shot grant has nothing left to retry, so it's a
// hard failure here — not a "<empty>: <empty>" message synthesized from a
// body that didn't actually parse.
func doTokenRequest(ctx context.Context, client *http.Client, tokenEndpoint string, form url.Values) (*TokenResponse, error) {
	resp, err := postForm(ctx, client, tokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, flowErrf(err, "%s", err.Error())
	}
	if !isSuccess(resp.StatusCode) {
		tokErr, ok := parseTokenError(body)
		if !ok {
			return nil, flowErrf(nil, "token endpoint returned %s with an unreadable error body", resp.Status)
		}
		return nil, flowErrf(nil, "%s: %s", tokErr.Error, tokErr.description())
	}
	return decodeTokenResponse(body)
}
