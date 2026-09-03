package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

// AuthErrorKind discriminates AuthError failures.
type AuthErrorKind int

const (
	// AuthErrDiscovery is an OIDC discovery failure (network, decode, or
	// issuer cross-check).
	AuthErrDiscovery AuthErrorKind = iota
	// AuthErrJwks is a JWKS fetch/decode failure.
	AuthErrJwks
	// AuthErrInvalidToken covers malformed tokens, signature failures, and
	// claim validation failures (exp/nbf/iss/aud).
	AuthErrInvalidToken
	// AuthErrUnknownKeyID is returned when a token's kid isn't in the JWKS,
	// even after a refresh attempt.
	AuthErrUnknownKeyID
	// AuthErrFlow is an OAuth token-acquisition flow failure (Task 8).
	AuthErrFlow
	// AuthErrInsecureIssuer is returned when the configured issuer isn't
	// https and no explicit override was passed.
	AuthErrInsecureIssuer
	// AuthErrMissingSubject is returned when a token has no (or an empty)
	// sub claim.
	AuthErrMissingSubject
)

// AuthError is Bifrost's auth error type: value-typed with an Unwrap chain
// to the underlying cause, mirroring Rust's thiserror source fields.
//
// Reference: the predecessor's auth crate, src/lib.rs:367-387 (AuthError).
type AuthError struct {
	Kind AuthErrorKind
	// Message carries the detail text for Discovery, Jwks, InvalidToken,
	// and Flow.
	Message string
	// Issuer is set for InsecureIssuer.
	Issuer string
	// Source is the wrapped underlying error, if any (for errors.Unwrap).
	Source error
}

func (e AuthError) Error() string {
	switch e.Kind {
	case AuthErrDiscovery:
		return fmt.Sprintf("OIDC discovery failed: %s", e.Message)
	case AuthErrJwks:
		return fmt.Sprintf("JWKS fetch failed: %s", e.Message)
	case AuthErrInvalidToken:
		return fmt.Sprintf("invalid token: %s", e.Message)
	case AuthErrUnknownKeyID:
		return "token key id not found in JWKS"
	case AuthErrFlow:
		return fmt.Sprintf("token flow failed: %s", e.Message)
	case AuthErrInsecureIssuer:
		return fmt.Sprintf(
			"issuer %s is not https — JWKS would be fetched over cleartext, letting a "+
				"network attacker substitute signing keys. Use https, or pass an explicit "+
				"insecure-transport override for local dev.",
			e.Issuer,
		)
	case AuthErrMissingSubject:
		return "token has no subject (sub) claim"
	}
	return "auth error"
}

// Unwrap exposes the wrapped Source error, mirroring Rust's thiserror
// #[source] fields, so errors.Is/errors.As can reach it.
func (e AuthError) Unwrap() error {
	return e.Source
}

// AssignmentSource is where scoped role assignments come from at request
// time. The interface lives here (next to Identity/RBAC evaluation) so the
// semantics stay next to the model; the implementation belongs in the API
// layer over the Store (one indexed role_assignments read per request —
// caching is a documented follow-up, not built).
//
// Reference: the predecessor's auth crate, src/lib.rs:392-404 (AssignmentSource).
type AssignmentSource interface {
	// AssignmentsFor returns all assignments for subject, as (role, scope)
	// rows. Implementations must fail CLOSED on backend errors (return
	// nil — global roles still apply, scoped extras are withheld).
	AssignmentsFor(ctx context.Context, subject string) []RoleScope
}

// ProviderMetadata is the subset of the OIDC provider metadata Bifrost
// uses.
//
// Reference: the predecessor's auth crate, src/lib.rs:406-417 (ProviderMetadata).
type ProviderMetadata struct {
	// Issuer is the issuer the provider claims; cross-checked against
	// config (#16). Absent on some minimal providers.
	Issuer                      *string `json:"issuer"`
	JwksURI                     string  `json:"jwks_uri"`
	TokenEndpoint               *string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint *string `json:"device_authorization_endpoint"`
}

// IdpClient returns an *http.Client configured with bounded timeouts so a
// hung/trickling IdP cannot park a request forever: a 10s connect timeout
// and a 15s overall timeout, mirroring the southbound posture.
//
// Reference: the predecessor's auth crate, src/lib.rs:419-427 (idp_client).
func IdpClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		},
	}
}

// DiscoverMetadata fetches {issuer}/.well-known/openid-configuration.
//
// Discovery itself is delegated to go-oidc's Provider (which builds the
// well-known URL and does the HTTP round trip); go-oidc's own issuer
// equality check is deliberately bypassed via InsecureIssuerURLContext,
// because it requires an EXACT string match with no trailing-slash
// tolerance — Discover performs its own trailing-slash-insensitive
// cross-check (#16) afterward, against the raw claims returned here.
//
// Reference: the predecessor's auth crate, src/lib.rs:429-447 (discover_metadata).
func DiscoverMetadata(ctx context.Context, client *http.Client, issuer string) (*ProviderMetadata, error) {
	ctx = oidc.ClientContext(ctx, client)
	ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, AuthError{Kind: AuthErrDiscovery, Message: err.Error(), Source: err}
	}
	var meta ProviderMetadata
	if err := provider.Claims(&meta); err != nil {
		return nil, AuthError{Kind: AuthErrDiscovery, Message: err.Error(), Source: err}
	}
	return &meta, nil
}

// DefaultRefreshCooldown is the minimum interval between JWKS refreshes
// triggered by an unknown kid: key rotation still works (a genuinely new
// key becomes visible within one cooldown window), but a garbage token
// cannot drive a refresh request flood at the IdP.
const DefaultRefreshCooldown = 30 * time.Second

// Validator validates Bearer JWTs against the issuer's JWKS.
//
// Keys are cached; an unknown kid triggers at most one JWKS refresh per
// refreshCooldown so key rotation works without letting a garbage token
// drive request floods at the IdP.
//
// go-oidc's RemoteKeySet (its "remote JWKS" type) was deliberately NOT
// used here: it re-fetches from the network on every unknown-kid lookup
// with no cooldown gate of its own (only in-flight de-duplication for
// concurrent callers), which would defeat the flood-prevention invariant
// above. The JWKS document is instead fetched and cached by hand,
// mirroring the predecessor's auth crate, src/lib.rs:459-586 structurally (a keys map guarded
// by one lock, a last-refresh timestamp guarded by another, released
// before the network call).
//
// Reference: the predecessor's auth crate, src/lib.rs:454-465 (Validator).
type Validator struct {
	config  AuthConfig
	client  *http.Client
	jwksURI string

	keysMu sync.RWMutex
	keys   map[string]*rsa.PublicKey

	refreshMu       sync.Mutex
	lastRefresh     time.Time
	refreshCooldown time.Duration
}

// Issuer returns the configured OIDC issuer (for GET /api/v1/auth/providers).
func (v *Validator) Issuer() string {
	return v.config.Issuer
}

// RoleMappings returns the configured group->role mappings (for GET
// /api/v1/access/roles).
func (v *Validator) RoleMappings() RoleMappings {
	// Deep-copy: the caller must not be able to mutate the live authz
	// config through the slices a plain struct copy would alias (#2, fix
	// round 1).
	return v.config.Roles.Clone()
}

// Discover runs OIDC discovery and the initial JWKS fetch. It fails
// fast — a control plane that cannot validate tokens must not start
// serving.
//
// Reference: the predecessor's auth crate, src/lib.rs:480-548 (Validator::discover).
func Discover(ctx context.Context, config AuthConfig, client *http.Client, allowInsecure bool) (*Validator, error) {
	if !strings.HasPrefix(config.Issuer, "https://") && !allowInsecure {
		return nil, AuthError{Kind: AuthErrInsecureIssuer, Issuer: config.Issuer}
	}

	meta, err := DiscoverMetadata(ctx, client, config.Issuer)
	if err != nil {
		return nil, err
	}

	// Cross-check the advertised issuer against the configured one,
	// trailing-slash-insensitive: a provider that answers discovery for a
	// different issuer than we trust is misconfigured or hostile (#16).
	if meta.Issuer != nil {
		advertised := strings.TrimRight(*meta.Issuer, "/")
		configured := strings.TrimRight(config.Issuer, "/")
		if advertised != configured {
			return nil, AuthError{
				Kind: AuthErrDiscovery,
				Message: fmt.Sprintf(
					"issuer mismatch: configured %s, provider advertises %s",
					config.Issuer, *meta.Issuer,
				),
			}
		}
	}

	// A wildcard viewer mapping turns deny-by-default into "any
	// authenticated caller reads everything" — warn loudly (#35).
	if config.Roles.HasWildcard() {
		slog.Warn("role mapping contains a \"*\" wildcard: every authenticated token " +
			"(including IdP service accounts) receives that role — deny-by-default " +
			"is disabled for it")
	}

	// #103: surface group->project-role automation at boot so operators
	// know self-service scoped grants are active without a manual
	// assignment.
	if !config.ProjectRoles.IsEmpty() {
		if config.ProjectRoles.HasWildcard() {
			slog.Info("project_roles maps a \"*\" wildcard: every group a caller " +
				"belongs to grants the mapped role scoped to project:<group> " +
				"automatically (self-service, no manual assignment)")
		} else {
			slog.Info("project_roles is configured: matching groups grant scoped " +
				"roles on project:<group> automatically (#103)")
		}
	}

	v := &Validator{
		config:  config,
		client:  client,
		jwksURI: meta.JwksURI,
		keys:    map[string]*rsa.PublicKey{},
		// Backdated so the very first refresh always proceeds regardless
		// of cooldown.
		lastRefresh:     time.Now().Add(-DefaultRefreshCooldown),
		refreshCooldown: DefaultRefreshCooldown,
	}
	if err := v.refreshJWKS(ctx); err != nil {
		return nil, err
	}
	// A provider that returns zero usable keys can never validate a
	// token; fail fast rather than boot into a permanently-401 state that
	// also invites JWKS refresh floods (#28).
	v.keysMu.RLock()
	n := len(v.keys)
	v.keysMu.RUnlock()
	if n == 0 {
		return nil, AuthError{Kind: AuthErrJwks, Message: "provider returned no usable signing keys"}
	}
	return v, nil
}

// refreshJWKS fetches and replaces the key cache, gated by
// refreshCooldown. The cooldown is claimed on a time basis alone —
// independent of whether the last fetch yielded keys (#28) — and the lock
// is released before the network call so a hung JWKS endpoint can't park
// every caller behind the mutex (#29).
func (v *Validator) refreshJWKS(ctx context.Context) error {
	v.refreshMu.Lock()
	if time.Since(v.lastRefresh) < v.refreshCooldown {
		v.refreshMu.Unlock()
		return nil
	}
	v.lastRefresh = time.Now()
	v.refreshMu.Unlock()

	// This fetch is shared, cooldown-gated background infrastructure, not
	// work scoped to the caller who happened to trigger it: if the
	// caller's request context is canceled (client disconnect, handler
	// timeout) mid-fetch, the fetch must still run to completion. Without
	// this, a single canceled caller aborts the network call AFTER the
	// cooldown slot above was already claimed, and no other caller can
	// retry for a full cooldown window — a trivial DoS against key
	// rotation (one canceled request every 30s starves it forever). The
	// client's own bounded timeout (see IdpClient) still caps the call.
	fetchCtx := context.WithoutCancel(ctx)
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, v.jwksURI, nil)
	if err != nil {
		return AuthError{Kind: AuthErrJwks, Message: err.Error(), Source: err}
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return AuthError{Kind: AuthErrJwks, Message: err.Error(), Source: err}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AuthError{Kind: AuthErrJwks, Message: err.Error(), Source: err}
	}
	if resp.StatusCode != http.StatusOK {
		return AuthError{
			Kind:    AuthErrJwks,
			Message: fmt.Sprintf("%s: %s", resp.Status, body),
		}
	}

	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return AuthError{Kind: AuthErrJwks, Message: err.Error(), Source: err}
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, raw := range doc.Keys {
		var jwk struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		}
		if err := json.Unmarshal(raw, &jwk); err != nil {
			continue
		}
		if jwk.Kty != "RSA" || jwk.Kid == "" {
			continue
		}
		pub, err := rsaPublicKeyFromJWK(jwk.N, jwk.E)
		if err != nil {
			continue
		}
		keys[jwk.Kid] = pub
	}

	slog.Info("JWKS refreshed", "keys", len(keys))
	v.keysMu.Lock()
	v.keys = keys
	v.keysMu.Unlock()
	return nil
}

// rsaPublicKeyFromJWK builds an *rsa.PublicKey from a JWK's base64url-encoded
// n (modulus) and e (exponent) fields.
func rsaPublicKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("auth: invalid JWK modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("auth: invalid JWK exponent: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() <= 0 || e.Int64() > int64(^uint32(0)) {
		return nil, fmt.Errorf("auth: JWK exponent out of range")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}

// keyFunc returns a jwt.Keyfunc that looks up the token's kid in the key
// cache, triggering a cooldown-gated refresh on a miss.
func (v *Validator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		kidRaw, present := token.Header["kid"]
		kid, _ := kidRaw.(string)
		if !present || kid == "" {
			return nil, AuthError{Kind: AuthErrInvalidToken, Message: "missing kid"}
		}

		v.keysMu.RLock()
		key, found := v.keys[kid]
		v.keysMu.RUnlock()
		if found {
			return key, nil
		}

		if err := v.refreshJWKS(ctx); err != nil {
			return nil, err
		}
		v.keysMu.RLock()
		key, found = v.keys[kid]
		v.keysMu.RUnlock()
		if !found {
			return nil, AuthError{Kind: AuthErrUnknownKeyID}
		}
		return key, nil
	}
}

// authLeeway is the clock-skew allowance applied to exp/nbf checks. This
// isn't a deliberate Bifrost choice: it mirrors jsonwebtoken's
// Validation::default() leeway (60s), which the Rust reference inherits
// silently (it never overrides the field) — ported here for fidelity
// rather than independently chosen. golang-jwt defaults leeway to zero, so
// it must be set explicitly to match.
const authLeeway = 60 * time.Second

// Validate validates a Bearer token: RS256 signature against JWKS, iss,
// aud, exp/nbf. Returns the mapped identity.
//
// Reference: the predecessor's auth crate, src/lib.rs:588-626 (Validator::validate).
func (v *Validator) Validate(ctx context.Context, tokenString string) (*Identity, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		// jsonwebtoken (the Rust reference) defaults required_spec_claims
		// to {"exp"} only, so a token *omitting* iss/aud passes by
		// default (only mismatches are caught) — a cross-audience
		// confused-deputy risk. golang-jwt has the same default posture:
		// WithExpirationRequired/WithIssuer/WithAudience each make their
		// claim mandatory, not merely checked-if-present (#16, #27).
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(strings.TrimRight(v.config.Issuer, "/")),
		jwt.WithAudience(v.config.Audience),
		jwt.WithLeeway(authLeeway),
		// nbf is validated whenever present (golang-jwt's default), but
		// not required — matching Rust's validate_nbf=true without adding
		// nbf to required_spec_claims.
	)

	token, err := parser.ParseWithClaims(tokenString, jwt.MapClaims{}, v.keyFunc(ctx))
	if err != nil {
		var ae AuthError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, AuthError{Kind: AuthErrInvalidToken, Message: err.Error(), Source: err}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, AuthError{Kind: AuthErrInvalidToken, Message: "unexpected claims type"}
	}

	identity := v.identityFromClaims(claims)
	if identity.Subject == "" {
		return nil, AuthError{Kind: AuthErrMissingSubject}
	}
	return &identity, nil
}

// identityFromClaims maps validated JWT claims to an Identity.
//
// Reference: the predecessor's auth crate, src/lib.rs:628-661 (Validator::identity_from_claims).
func (v *Validator) identityFromClaims(claims jwt.MapClaims) Identity {
	groups := extractGroups(claims, v.config.GroupsClaim)
	roles := v.config.Roles.Resolve(groups)
	// #103: derive project-scoped grants from group membership (empty
	// when [project_roles] is unset — deny-by-default preserved).
	projectRoles := v.config.ProjectRoles.Resolve(groups)

	return Identity{
		// Sanitize: sub reaches the plain-text log layer, and a sub
		// containing newlines/control chars could forge audit lines
		// (#34). Replace control chars with '?'.
		Subject: sanitizeClaim(stringClaimOr(claims, "sub", "")),
		// preferred_username reaches the audit log and a k8s label
		// value; sanitize like sub. Absent (some IdPs omit it) -> nil,
		// and Owner() falls back to Subject.
		Username:     sanitizedStringClaimPtr(claims, "preferred_username"),
		Email:        stringClaimPtr(claims, "email"),
		Groups:       groups,
		Roles:        roles,
		ProjectRoles: projectRoles,
	}
}

// extractGroups reads the configured groups claim: an array of strings, or
// one space-delimited string. Anything else (absent, wrong type) yields no
// groups.
func extractGroups(claims jwt.MapClaims, groupsClaim string) []string {
	v, present := claims[groupsClaim]
	if !present {
		return nil
	}
	switch t := v.(type) {
	case []interface{}:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	}
	return nil
}

// stringClaimOr returns claims[key] as a string, or def if the key is
// absent or not a string.
func stringClaimOr(claims jwt.MapClaims, key, def string) string {
	v, present := claims[key]
	if !present {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	return s
}

// stringClaimPtr returns a pointer to claims[key] as a string, or nil if
// the key is absent or not a string. Unlike sanitizedStringClaimPtr, the
// value is returned verbatim (Rust doesn't sanitize the email claim).
func stringClaimPtr(claims jwt.MapClaims, key string) *string {
	v, present := claims[key]
	if !present {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

// sanitizedStringClaimPtr is stringClaimPtr with control characters
// replaced, for claims that reach the log/label layer.
func sanitizedStringClaimPtr(claims jwt.MapClaims, key string) *string {
	s := stringClaimPtr(claims, key)
	if s == nil {
		return nil
	}
	sanitized := sanitizeClaim(*s)
	return &sanitized
}
