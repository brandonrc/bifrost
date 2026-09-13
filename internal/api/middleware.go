// Auth middleware: deny by default, and fail closed when nothing is
// configured to deny with. Ported from the Rust predecessor's auth_layer.rs
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
// job surface) AND sit inside the target entry's tenant boundary
// (admin/owner/holder of a project assignment whose role licenses the
// verb — see authorizeGatewayRequest) before the request is allowed to
// fall through to the gateway at all.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/core"
)

// isPublic mirrors auth_layer.rs's is_public: the narrow allowlist
// reachable without a bearer token. Exact matches only — matching the Rust
// comment verbatim: "everything else under /api/v1/auth/ requires an
// identity." The reference also exempted /docs and /docs/* for a Swagger
// UI; nothing in this server serves those paths (server.go mounts only the
// spec at SpecPath), so the dead entries were dropped (F10) rather than
// left standing as unauthenticated attack surface.
func isPublic(path string) bool {
	return path == "/healthz" ||
		path == "/api/v1/version" ||
		path == SpecPath ||
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
// the fail-closed rule's "is auth configured at all" test (the Rust predecessor
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
// requires, PLUS the tenant boundary: the verb's role check is necessary
// but not sufficient — it says nothing about WHICH cluster the request is
// aimed at. A developer holding a global role but zero project ties must
// not drive arbitrary commands into another tenant's cluster through its
// hostname (red-team finding: gateway dispatch checked only the verb, so
// any developer could submit a Ray job — remote code execution — to, and
// any viewer read job history/logs of, a foreign cluster). The target
// follows the entry: a `jobs` entry fronts a Ray Jobs API
// (auth.TargetJob), a `serve` entry a Serve application
// (auth.TargetService). Ownership crosses the tenant boundary but never
// waives the verb's permission — a viewer who owns a cluster still may
// not submit jobs through it. Auditor gets nothing here: the gateway is
// the job surface, not an audit surface, and RoleAuditor holds no
// Target::Job/Service grant anyway. The denial follows the mutation
// convention: 403, audited with Method and Path.
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
	target := gatewayTarget(&endpoint)
	permitted, within := gatewayDecision(r.Context(), store, identity, required, target, endpoint)
	if permitted && within {
		return nil
	}
	subject := identity.Subject
	reason := "insufficient_permission"
	if permitted {
		// The verb's permission was satisfied but the tenant boundary
		// was not — distinguishable in the audit trail.
		reason = "foreign_cluster"
	}
	status := uint16(http.StatusForbidden)
	method := r.Method
	path := r.URL.Path
	action := gatewayAuditAction
	EmitAudit(r.Context(), store, &core.AuditEvent{
		Ts:           controller.NowUnix(),
		Subject:      &subject,
		Decision:     core.AuditDecisionDeny,
		Reason:       &reason,
		Action:       &action,
		Method:       &method,
		Path:         &path,
		Status:       &status,
		Required:     &core.AuditRequired{Action: PermissionStr(required), Target: TargetStr(target)},
		GrantedRoles: grantedRoleStrs(identity.Roles),
	})
	return ErrForbidden
}

// gatewayDecision is the decision half of authorizeGatewayRequest, split
// into its two necessary conditions so the denial can say which one
// failed:
//
//   - permitted: the verb's permission, by a global role OR a
//     project-scoped assignment covering the entry's project whose role
//     grants it (a project-scoped developer may submit where a global
//     developer cannot reach);
//   - within: the tenant boundary — Admin, the recorded owner of a
//     store-backed cluster row, or a project-scoped assignment covering
//     the project WHOSE ROLE GRANTS this request's (verb, target). The
//     boundary is role-aware, matching the control plane's own
//     clusterTenantAccess (authz.go): a role-agnostic membership check
//     (any covering assignment, even auditor) used to let a global
//     developer submit jobs to a cluster the API layer would refuse them
//     — the gateway and the control plane must agree on what membership
//     means. A global role alone never crosses the tenant boundary.
//
// The project/owner come from the store row when one exists (a `jobs`
// entry may front a lifecycle cluster even via a static registry entry
// that predates the reconciler's dynamic registration — the store row is
// authoritative); otherwise from the entry's own Project (the reconciler
// stamps it on dynamic entries for clusters, jobs and services). An entry
// with no tenant information at all — static, no Project, no store row —
// is an externally-managed cluster and keeps the original global check.
// A store lookup failure fails closed.
func gatewayDecision(ctx context.Context, store controller.Store, identity *auth.Identity, required auth.PermissionType, target auth.Target, endpoint core.ClusterEndpoint) (permitted, within bool) {
	if identity == nil {
		return true, true // dev mode (RequireAuth never reaches here authenticated-less)
	}
	project := endpoint.Project
	var owner *string
	if endpoint.Target != core.RegistryTargetServe && store != nil {
		// Serve entries front RayServices, which the cluster store does
		// not hold; every other entry may name a lifecycle cluster.
		c, err := store.Get(ctx, endpoint.Id)
		if err != nil {
			slog.Warn("api: gateway tenant-scope cluster lookup failed", "cluster", endpoint.Id, "error", err)
			return false, false
		}
		if c != nil {
			project = c.Spec.Project
			owner = c.Spec.Owner
		}
	}
	if project == "" && owner == nil {
		return identity.Permits(required, target), true
	}
	// One assignment evaluation feeds both conditions (grant is the
	// role-aware tenant check AND the scoped-permission half of
	// permitted), so the decision costs a single ListRoleAssignments read.
	var grant bool
	if project != "" {
		grant = projectAssignmentGrants(ctx, store, identity, project, required, target)
	}
	permitted = identity.Permits(required, target) || grant
	within = hasRole(identity, auth.RoleAdmin) || grant ||
		(owner != nil && *owner == identity.Owner())
	return permitted, within
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

// gatewayEndpointContextKey carries RequireAuth's registry resolution down
// to HostGateway (gateway.go). RequireAuth resolves the request's Host to
// a registry entry ONCE — to decide allowlist suppression and gateway
// authorization — and pins the outcome here; HostGateway consumes the pin
// instead of re-resolving. Without the pin, a dynamic entry registered
// between the two lookups flips a request already admitted as
// control-plane traffic (e.g. a public-path 200) into "proxied southbound
// with the cluster token injected" mid-request (red-team TOCTOU finding).
type gatewayEndpointContextKey struct{}

// pinnedGatewayEndpoint is the pinned resolution outcome: ok distinguishes
// "checked, not a cluster host" (fall through, do NOT re-resolve) from
// "no pin present" (HostGateway's fallback lookup — see
// gatewayEndpointForRequest).
type pinnedGatewayEndpoint struct {
	endpoint core.ClusterEndpoint
	ok       bool
}

// pinGatewayEndpoint returns r with the registry resolution attached.
func pinGatewayEndpoint(r *http.Request, endpoint core.ClusterEndpoint, ok bool) *http.Request {
	return r.WithContext(context.WithValue(
		r.Context(), gatewayEndpointContextKey{}, pinnedGatewayEndpoint{endpoint: endpoint, ok: ok}))
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
// contents — mirroring the shape the Rust predecessor's auth_layer.rs/lib.rs emit
// via tracing (`decision=deny reason=...`). 401s log at Info: auth_layer.rs
// audits both 401 paths at INFO specifically so credential-stuffing /
// token-guessing is visible in the ordinary log stream, not buried at
// debug (#23); the fail-closed non-loopback refusal logs at Warn,
// matching lib.rs's `tracing::warn!`.
//
// These rows stay slog-only BY DECISION, though the durable sink now
// exists (AuthState.Store -> EmitAudit -> the store's hash-chained
// RecordAudit, wired in internal/app's New): this middleware answers
// UNAUTHENTICATED traffic, so persisting every refusal would let any
// anonymous client append unbounded rows to the audit table. The
// authenticated denials — host-is-cluster authorization failures — do
// persist through EmitAudit (authorizeGatewayRequest above), where the
// caller's identity is proven and the row is attributable.
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
			// Resolve the host->cluster mapping ONCE, up front, and pin it
			// on the request context for HostGateway (see
			// gatewayEndpointContextKey) — every pass-through below serves
			// the pinned request so both layers see the same decision even
			// if the registry changes mid-request.
			endpoint, onClusterHost := clusterForRequest(state.Registry, r)
			r = pinGatewayEndpoint(r, endpoint, onClusterHost)
			if !state.configured() {
				next.ServeHTTP(w, r)
				return
			}
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
// the predecessor's lib.rs's serve_with_shutdown_and_limits: refuse to bind a
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
// the predecessor's lib.rs's refuse_non_loopback: when installed (see
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
