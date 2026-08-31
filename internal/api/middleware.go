// Auth middleware: deny by default, and fail closed when nothing is
// configured to deny with. Ported from mobula-api's auth_layer.rs
// (require_auth, is_public, resolve_identity, is_jwt_shaped) and lib.rs
// (refuse_non_loopback, the serve_with_shutdown_and_limits bind guard).
//
// Scope note (Wave 1 T10): this wave wires authentication only — every
// request either carries a valid bearer identity or is refused. The
// per-route/target authorization checks (auth_layer.rs's authorize,
// authorize_scoped, target_for_path) apply once real handlers exist
// behind ClusterRegistry/Store state, which is Wave 1 T11/T12's job; the
// gateway's host-is-cluster override (never-public cluster hostnames) is
// T13.
package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
)

// isPublic mirrors auth_layer.rs's is_public: the narrow allowlist
// reachable without a bearer token. Exact matches only for everything
// except the Swagger UI's own asset tree — matching the Rust comment
// verbatim: "everything else under /api/v1/auth/ requires an identity."
func isPublic(path string) bool {
	return path == "/healthz" ||
		path == "/api/v1/version" ||
		path == SpecPath ||
		path == "/docs" ||
		strings.HasPrefix(path, "/docs/") ||
		path == "/api/v1/auth/login" ||
		path == "/api/v1/auth/providers"
}

// isJWTShaped mirrors auth_layer.rs's is_jwt_shaped: three dot-delimited
// segments. Token dispatch is unambiguous (ADR-0011) — a `mob_…` PAT
// contains no dots and a JWT never matches the `mob_<prefix>_<hex>`
// scheme, so the two paths can coexist without misclassification.
func isJWTShaped(token string) bool {
	return strings.Count(token, ".") == 2
}

// AuthState is the auth middleware's configuration: an optional OIDC
// validator, an optional local (IdP-free) authenticator, or both. When
// both are nil, auth is disabled (dev mode) — the caller is responsible
// for the fail-closed non-loopback guard in that case (see
// NewHandler/RefuseNonLoopback/CheckBindAllowed).
type AuthState struct {
	Validator *auth.Validator
	Local     *auth.LocalAuthenticator
}

// configured reports whether any authentication mechanism is wired up —
// the fail-closed rule's "is auth configured at all" test (mobula-api
// #36/#45: local auth counts exactly like an OIDC validator).
func (s AuthState) configured() bool {
	return s.Validator != nil || s.Local != nil
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}

// resolveIdentity mirrors auth_layer.rs's resolve_identity: when a
// Validator exists and the token is JWT-shaped, the OIDC path;
// otherwise, when local auth is enabled, the opaque-PAT path.
func resolveIdentity(ctx context.Context, s AuthState, token string) *auth.Identity {
	if s.Validator != nil && isJWTShaped(token) {
		id, err := s.Validator.Validate(ctx, token)
		if err != nil {
			return nil
		}
		return id
	}
	if s.Local != nil {
		return s.Local.AuthenticateToken(ctx, token)
	}
	return nil
}

type identityContextKey struct{}

// IdentityFromContext returns the identity RequireAuth attached to the
// request context, when one was authenticated (dev mode with no
// AuthState configured attaches none).
func IdentityFromContext(ctx context.Context) (*auth.Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(*auth.Identity)
	return id, ok
}

// ErrMissingBearerToken and ErrInvalidBearerToken back the 401 responses
// RequireAuth emits; both carry the canonical envelope via WriteError.
var (
	ErrMissingBearerToken = &HTTPError{Status: http.StatusUnauthorized, Code: "missing_token", Message: "missing bearer token"}
	ErrInvalidBearerToken = &HTTPError{Status: http.StatusUnauthorized, Code: "invalid_token", Message: "invalid token"}
)

// RequireAuth is the deny-by-default auth middleware (auth_layer.rs's
// require_auth). When state carries no validator and no local
// authenticator, auth is disabled and every request passes through
// (dev mode). Otherwise every request needs a valid bearer token except
// the public allowlist (isPublic); a missing or invalid token gets 401
// with a WWW-Authenticate: Bearer header and the canonical error body.
func RequireAuth(state AuthState) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !state.configured() {
				next.ServeHTTP(w, r)
				return
			}
			if isPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				WriteError(w, r, ErrMissingBearerToken)
				return
			}
			identity := resolveIdentity(r.Context(), state, token)
			if identity == nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				WriteError(w, r, ErrInvalidBearerToken)
				return
			}
			ctx := context.WithValue(r.Context(), identityContextKey{}, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ErrAuthNotConfigured backs both fail-closed guards below: refusing a
// non-loopback bind (CheckBindAllowed) and refusing a non-loopback peer
// at the router level (RefuseNonLoopback), when neither a validator nor
// local auth is configured.
var ErrAuthNotConfigured = errors.New("no authentication is configured")

// CheckBindAllowed is the bind-time fail-closed guard, ported from
// mobula-api lib.rs's serve_with_shutdown_and_limits: refuse to bind a
// non-loopback address when no authentication is configured, unless
// explicitly overridden. A caller (Wave 1 T15's CLI) invokes this before
// opening its listener; loopback is decided from the bind IP itself,
// exactly as the Rust guard decides from the SocketAddr passed to serve.
func CheckBindAllowed(bindIP net.IP, authConfigured, allowUnauthenticated bool) error {
	if authConfigured || allowUnauthenticated {
		return nil
	}
	if bindIP != nil && bindIP.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"refusing to bind: %w, so a non-loopback bind exposes the control plane to "+
			"unauthenticated access; configure a validator or local auth, or allow unauthenticated access explicitly",
		ErrAuthNotConfigured,
	)
}

// RefuseNonLoopback is the router-level fail-closed guard, ported from
// mobula-api lib.rs's refuse_non_loopback: when installed (see
// NewHandler — only when no authentication is configured at all and it
// hasn't been explicitly overridden), it refuses any request whose peer
// isn't loopback, so a direct http.Serve(handler) on this Handler also
// fails closed for remote clients regardless of the bind address the
// caller chose — defense in depth alongside CheckBindAllowed. If the
// peer address can't be parsed, it is NOT provably loopback, so the
// request is refused.
func RefuseNonLoopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !peerIsLoopback(r.RemoteAddr) {
			WriteError(w, r, &HTTPError{
				Status:  http.StatusForbidden,
				Code:    "unauthenticated_non_loopback",
				Message: "no authentication is configured; non-loopback access is refused",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// No port present (e.g. httptest peers sometimes omit it) — try
		// the whole string as a bare host.
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
