// Auth middleware: deny by default, and fail closed when nothing is
// configured to deny with. Ported from mobula-api's auth_layer.rs
// (require_auth, is_public, resolve_identity, is_jwt_shaped) and lib.rs
// (refuse_non_loopback, the serve_with_shutdown_and_limits bind guard).
//
// Scope note (Wave 1 T10): this wave wired authentication only — every
// request either carries a valid bearer identity or is refused. The
// per-route/target authorization checks (auth_layer.rs's authorize,
// authorize_scoped, target_for_path) apply once real handlers exist
// behind ClusterRegistry/Store state (Wave 1 T11/T12's job).
//
// Wave 1 T13 adds the one authorization check that belongs HERE rather
// than behind a route handler: the gateway's host-is-cluster override.
// auth_layer.rs's require_auth does this inline (it does NOT call the
// Rust reference's own authorize() helper) because cluster-host traffic
// never reaches a per-route handler at all — it goes straight to
// gateway.go's HostGateway middleware, which this package composes
// directly behind RequireAuth (see server.go's NewHandler). Two
// consequences, both ported verbatim: (1) a Host matching a registered
// cluster is NEVER public — isPublic's allowlist is for the
// control-plane host only, so e.g. GET /healthz on a cluster host still
// requires a valid bearer token; (2) once authenticated, that identity
// must additionally hold the Target::Job permission the request's verb
// requires (required_permission/target_for_path collapse to a fixed
// Target::Job here — the whole cluster-host surface IS the proxied Ray
// job surface) before the request is allowed to fall through to the
// gateway at all.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
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
// segments. Token dispatch is unambiguous (ADR-0011) — a `bfr_…` PAT
// contains no dots and a JWT never matches the `bfr_<prefix>_<hex>`
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
	// Registry backs the host-is-cluster gate (T13): a Host matching a
	// registered cluster is never public and is authorized against
	// Target::Job right here rather than by a per-route handler — see
	// the package doc comment above. nil disables the gateway override
	// entirely (every request is judged only against the control-plane
	// allowlist), matching a deployment with no gateway configured.
	Registry *core.ClusterRegistry
	// Store persists the audit trail for host-is-cluster authorization
	// denials (api-v1.md §5.9); nil keeps those denials trace-only, the
	// same nil-store contract every other EmitAudit call site has.
	Store controller.Store
}

// configured reports whether any authentication mechanism is wired up —
// the fail-closed rule's "is auth configured at all" test (mobula-api
// #36/#45: local auth counts exactly like an OIDC validator).
func (s AuthState) configured() bool {
	return s.Validator != nil || s.Local != nil
}

// hostIsCluster reports whether r's Host matches a registered cluster
// (auth_layer.rs's require_auth: `st.registry.by_hostname(h).is_some()`).
// Go's net/http moves the Host header out of r.Header into r.Host for
// server requests (unlike axum, which keeps it in the header map), so
// this reads r.Host rather than r.Header.Get("Host").
func hostIsCluster(registry *core.ClusterRegistry, r *http.Request) bool {
	_, ok := clusterForRequest(registry, r)
	return ok
}

// clusterForRequest resolves r's Host to its registry entry (hostIsCluster
// with the entry kept: the gateway authorization below needs its Project
// and Target).
func clusterForRequest(registry *core.ClusterRegistry, r *http.Request) (core.ClusterEndpoint, bool) {
	if registry == nil {
		return core.ClusterEndpoint{}, false
	}
	return registry.ByHostname(r.Host)
}

// requiredGatewayPermission mirrors auth_layer.rs's required_permission:
// reads (GET/HEAD/OPTIONS) need Read; every other verb (including the
// websocket log-tail's GET upgrade, which required_permission's own doc
// comment calls out explicitly) needs Write. DELETE on the proxied Ray
// surface is job deletion — a Developer action — so it maps to Write,
// not Delete.
func requiredGatewayPermission(method string) auth.PermissionType {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return auth.Read
	default:
		return auth.Write
	}
}

// authorizeGatewayRequest enforces the permission cluster-host traffic
// requires. identity is always non-nil here — RequireAuth only reaches
// this call after successfully resolving one.
//
// The target follows the entry: a `jobs` entry fronts a Ray Jobs API
// (auth.TargetJob), a `serve` entry a Serve application (auth.TargetService).
// A static entry (Project "") keeps the original global check; a dynamic
// entry registered by the reconciler for a project's cluster, job or
// service is authorized within that project — the same rule the project's
// own routes apply (authorizeInProject): a caller narrowed to other
// projects is refused, then global roles or a covering assignment must
// grant the verb's permission.
//
// Deliberately NOT authz.go's shared Authorize helper: auth_layer.rs's
// require_auth doesn't call its own authorize() either, because this
// denial's audit row carries Method and Path (core.AuditEvent's doc
// comment: "for gateway and authn/ext_authz rows") — fields Authorize's
// shared emitAuthzDenial doesn't set, since a per-route handler's own
// authorization call has no comparable use for them. EmitAudit,
// PermissionStr, TargetStr, grantedRoleStrs, and ErrForbidden ARE reused
// from authz.go — only the denial's field population differs.
func authorizeGatewayRequest(store controller.Store, identity *auth.Identity, r *http.Request, endpoint core.ClusterEndpoint) error {
	required := requiredGatewayPermission(r.Method)
	target := auth.TargetJob
	if endpoint.Target == core.RegistryTargetServe {
		target = auth.TargetService
	}
	if gatewayPermitted(r.Context(), store, identity, required, target, endpoint.Project) {
		return nil
	}
	subject := identity.Subject
	reason := "insufficient_permission"
	status := uint16(http.StatusForbidden)
	method := r.Method
	path := r.URL.Path
	EmitAudit(r.Context(), store, &core.AuditEvent{
		Ts:           controller.NowUnix(),
		Subject:      &subject,
		Decision:     core.AuditDecisionDeny,
		Reason:       &reason,
		Method:       &method,
		Path:         &path,
		Status:       &status,
		Required:     &core.AuditRequired{Action: PermissionStr(required), Target: TargetStr(target)},
		GrantedRoles: grantedRoleStrs(identity.Roles),
	})
	return ErrForbidden
}

// gatewayPermitted is the decision half of authorizeGatewayRequest: the
// global check for a static entry (project ""), the project-scoped rule
// (see authorizeInProject) for a dynamic one.
func gatewayPermitted(ctx context.Context, store controller.Store, identity *auth.Identity, required auth.PermissionType, target auth.Target, project string) bool {
	if project == "" {
		return identity.Permits(required, target)
	}
	assignments, narrowed := readScope(ctx, store, identity)
	if len(narrowed) > 0 && !containsString(narrowed, project) {
		return false
	}
	return identity.Permits(required, target) || identity.PermitsScoped(required, target, assignments, project)
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

type bearerTokenContextKey struct{}

// BearerTokenFromContext returns the raw bearer token RequireAuth
// authenticated the request with (T12, local_auth.go's Logout): the
// generated LogoutRequestObject carries no fields at all, so a handler
// that needs the presented token — to look up its PAT prefix and revoke
// it, mirroring local_auth.rs's logout reading the Authorization header
// straight off the request — has no other way to reach it once inside the
// strict-server layer. Only set when RequireAuth actually authenticated
// the request (never in dev mode, where no AuthState is configured at
// all).
func BearerTokenFromContext(ctx context.Context) (string, bool) {
	tok, ok := ctx.Value(bearerTokenContextKey{}).(string)
	return tok, ok
}

// ErrMissingBearerToken and ErrInvalidBearerToken back the 401 responses
// RequireAuth emits; both carry the canonical envelope via WriteError.
// Value types (see HTTPError's doc comment) — every use copies, never
// aliases, the sentinel.
var (
	ErrMissingBearerToken = HTTPError{Status: http.StatusUnauthorized, Code: "missing_token", Message: "missing bearer token"}
	ErrInvalidBearerToken = HTTPError{Status: http.StatusUnauthorized, Code: "invalid_token", Message: "invalid token"}
)

// auditDenial logs a structured access-denial record — never token
// contents — mirroring the shape mobula-api's auth_layer.rs/lib.rs emit
// via tracing (`decision=deny reason=...`). 401s log at Info: auth_layer.rs
// audits both 401 paths at INFO specifically so credential-stuffing /
// token-guessing is visible in the ordinary log stream, not buried at
// debug (#23); the fail-closed non-loopback refusal logs at Warn,
// matching lib.rs's `tracing::warn!`.
//
// TODO(T11/T12): once AuthState carries a persisted audit sink (the
// store-backed AuditEvent the Rust reference writes via
// crate::audit::emit), route these through it too — this slog record is
// the interim signal until that plumbing (ClusterRegistry/Store-backed
// handlers) exists.
func auditDenial(level slog.Level, r *http.Request, reason string) {
	slog.LogAttrs(r.Context(), level, "api: access denied",
		slog.String("decision", "deny"),
		slog.String("reason", reason),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("remote_addr", r.RemoteAddr),
	)
}

// RequireAuth is the deny-by-default auth middleware (auth_layer.rs's
// require_auth). When state carries no validator and no local
// authenticator, auth is disabled and every request passes through
// (dev mode). Otherwise every request needs a valid bearer token except
// the public allowlist (isPublic) — UNLESS its Host matches a
// registered cluster (state.Registry), in which case the allowlist is
// suppressed entirely and, once authenticated, the identity must also
// hold the Target::Job permission the request's verb requires (T13's
// host-is-cluster gate — see the package doc comment and
// authorizeGatewayRequest). A missing or invalid token gets 401 with a
// WWW-Authenticate: Bearer header and the canonical error body; an
// authenticated-but-unauthorized cluster-host request gets 403.
func RequireAuth(state AuthState) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !state.configured() {
				next.ServeHTTP(w, r)
				return
			}
			endpoint, onClusterHost := clusterForRequest(state.Registry, r)
			if !onClusterHost && isPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				auditDenial(slog.LevelInfo, r, "missing_token")
				w.Header().Set("WWW-Authenticate", "Bearer")
				WriteError(w, r, ErrMissingBearerToken)
				return
			}
			identity := resolveIdentity(r.Context(), state, token)
			if identity == nil {
				auditDenial(slog.LevelInfo, r, "invalid_token")
				w.Header().Set("WWW-Authenticate", "Bearer")
				WriteError(w, r, ErrInvalidBearerToken)
				return
			}
			if onClusterHost {
				if err := authorizeGatewayRequest(state.Store, identity, r, endpoint); err != nil {
					WriteError(w, r, err)
					return
				}
			}
			ctx := context.WithValue(r.Context(), identityContextKey{}, identity)
			ctx = context.WithValue(ctx, bearerTokenContextKey{}, token)
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
			auditDenial(slog.LevelWarn, r, "unauthenticated_non_loopback")
			WriteError(w, r, HTTPError{
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
