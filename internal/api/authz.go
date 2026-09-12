// Shared authorization + audit-emission helpers used by every handler this
// wave and the next port behind the strict-server interface. Ported from
// the Rust predecessor's auth_layer.rs (authorize, authorize_scoped,
// effective_assignments, StoreAssignments) and audit.rs (emit, permission_str,
// target_str, role_str).
//
// T10's RequireAuth (middleware.go) only established AUTHENTICATION —
// "every request either carries a valid bearer identity or is refused" — and
// deliberately deferred per-route AUTHORIZATION to this wave (see
// middleware.go's package doc scope note). This file is that piece: every
// handler in clusters.go/registry.go/settings.go/access.go (T11) and every
// handler T12 adds behind pools.go/services.go/usage.go/audit.go/
// local_auth.go calls Authorize or AuthorizeScoped before touching state.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/core"
)

// PermissionStr returns the snake_case wire form of a permission verb (for
// AuditRequired.Action), mirroring audit.rs's permission_str.
func PermissionStr(action auth.PermissionType) string { return action.AsStr() }

// TargetStr returns the snake_case wire form of a permission target (for
// AuditRequired.Target), mirroring audit.rs's target_str.
func TargetStr(target auth.Target) string { return target.AsStr() }

// RoleStr returns the snake_case wire form of a role (for
// AuditEvent.GrantedRoles), mirroring audit.rs's role_str.
func RoleStr(role auth.Role) string { return role.AsStr() }

// EmitAudit appends one audit event: traced at Info (the predecessor's `audit` tracing
// target's Go equivalent — every field mirrored so the JSONL log-export
// story T15 wires up later has everything it needs) and, when store is
// non-nil, persisted. A persistence failure is logged and NEVER propagated —
// audit persistence must never fail the request being audited (audit.rs's
// emit doc comment).
func EmitAudit(ctx context.Context, store controller.Store, event *core.AuditEvent) {
	attrs := []any{
		"ts", event.Ts,
		"decision", event.Decision.AsStr(),
	}
	if event.Subject != nil {
		attrs = append(attrs, "subject", *event.Subject)
	}
	if event.Reason != nil {
		attrs = append(attrs, "reason", *event.Reason)
	}
	if event.Action != nil {
		attrs = append(attrs, "action", *event.Action)
	}
	if event.Cluster != nil {
		attrs = append(attrs, "cluster", *event.Cluster)
	}
	if event.Method != nil {
		attrs = append(attrs, "method", *event.Method)
	}
	if event.Path != nil {
		attrs = append(attrs, "path", *event.Path)
	}
	if event.Status != nil {
		attrs = append(attrs, "status", *event.Status)
	}
	if event.Required != nil {
		attrs = append(attrs, "required_action", event.Required.Action, "required_target", event.Required.Target)
	}
	if len(event.GrantedRoles) > 0 {
		attrs = append(attrs, "granted", event.GrantedRoles)
	}
	slog.Info("api: audit event", attrs...)

	if store == nil {
		return
	}
	if _, err := store.RecordAudit(ctx, event); err != nil {
		slog.Warn("api: failed to persist audit event", "error", err)
	}
}

// ErrForbidden backs Authorize/AuthorizeScoped's denial — an authenticated
// caller lacking the required permission gets 403 (deny-by-default). Value
// type (see HTTPError's doc comment in errors.go).
var ErrForbidden = HTTPError{Status: http.StatusForbidden, Code: "insufficient_permission", Message: "insufficient permission"}

func grantedRoleStrs(roles []auth.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = RoleStr(r)
	}
	return out
}

func emitAuthzDenial(ctx context.Context, store controller.Store, id *auth.Identity, action auth.PermissionType, target auth.Target) {
	subject := id.Subject
	reason := "insufficient_permission"
	status := uint16(http.StatusForbidden)
	EmitAudit(ctx, store, &core.AuditEvent{
		Ts:           controller.NowUnix(),
		Subject:      &subject,
		Decision:     core.AuditDecisionDeny,
		Reason:       &reason,
		Status:       &status,
		Required:     &core.AuditRequired{Action: PermissionStr(action), Target: TargetStr(target)},
		GrantedRoles: grantedRoleStrs(id.Roles),
	})
}

// Authorize is the route-handler authorization helper (auth_layer.rs's
// authorize). identity == nil ONLY happens in dev/no-auth mode (T10's
// RequireAuth attaches no identity when neither a validator nor local auth
// is configured) — in that mode every access is permitted, matching the
// Rust reference's `None => None` arm. Otherwise it's deny-by-default
// against (action, target); a denial is also an audit event (persisted when
// store is non-nil — a nil store keeps the denial trace-only, matching
// the Rust predecessor's store-less registry/services routers).
func Authorize(ctx context.Context, store controller.Store, identity *auth.Identity, action auth.PermissionType, target auth.Target) error {
	if identity == nil {
		return nil
	}
	if identity.Permits(action, target) {
		return nil
	}
	emitAuthzDenial(ctx, store, identity, action, target)
	return ErrForbidden
}

// AuthorizeScoped is Authorize's scoped variant (#49, auth_layer.rs's
// authorize_scoped): grants when the identity's global roles suffice (fast
// path, no store read) OR when a stored/group-derived assignment covers
// project ("*" or "project:<project>"). Additive-only.
func AuthorizeScoped(ctx context.Context, store controller.Store, identity *auth.Identity, action auth.PermissionType, target auth.Target, project string) error {
	if identity == nil {
		return nil
	}
	if identity.Permits(action, target) {
		return nil
	}
	assignments := EffectiveAssignments(ctx, store, identity)
	if identity.PermitsScoped(action, target, assignments, project) {
		return nil
	}
	emitAuthzDenial(ctx, store, identity, action, target)
	return ErrForbidden
}

// EffectiveAssignments is the effective scoped assignments for id: the
// group-derived project roles already resolved onto the identity (#103,
// Identity.ProjectRoles) combined with stored grants matched by BOTH the
// token subject and the preferred_username (#88), de-duplicated. Works with
// or without a store — group-derived grants apply either way. Ported from
// auth_layer.rs's effective_assignments + StoreAssignments::for_identity.
func EffectiveAssignments(ctx context.Context, store controller.Store, id *auth.Identity) []auth.RoleScope {
	out := append([]auth.RoleScope(nil), id.ProjectRoles...)
	if store == nil {
		return out
	}
	for _, a := range storeAssignmentsForIdentity(ctx, store, id) {
		if !containsRoleScope(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func storeAssignmentsForIdentity(ctx context.Context, store controller.Store, id *auth.Identity) []auth.RoleScope {
	out := assignmentsFor(ctx, store, id.Subject)
	if id.Username != nil && *id.Username != id.Subject {
		for _, a := range assignmentsFor(ctx, store, *id.Username) {
			if !containsRoleScope(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// assignmentsFor is StoreAssignments::assignments_for: one indexed row read,
// matched-subject role assignments for subject. Fails closed on a store
// error — scoped extras are withheld, but the identity's global roles still
// apply via Permits' fast path (matches the Rust reference's tracing::warn +
// empty-Vec fallback).
func assignmentsFor(ctx context.Context, store controller.Store, subject string) []auth.RoleScope {
	rows, err := store.ListRoleAssignments(ctx, &subject)
	if err != nil {
		slog.Warn("api: assignment lookup failed", "subject", subject, "error", err)
		return nil
	}
	var out []auth.RoleScope
	for _, a := range rows {
		role, ok := auth.ParseRole(a.Role)
		if !ok {
			continue
		}
		out = append(out, auth.RoleScope{Role: role, Scope: a.Scope})
	}
	return out
}

// hasRole reports whether id holds any of the given roles.
func hasRole(id *auth.Identity, roles ...auth.Role) bool {
	if id == nil {
		return false
	}
	for _, r := range id.Roles {
		for _, want := range roles {
			if r == want {
				return true
			}
		}
	}
	return false
}

// projectAssignmentGrants reports whether any effective project-scoped
// assignment (NOT the global "*" scope) covers project with a role that
// grants (action, target). This is the "the assignment itself licenses
// the verb" half of the rule (a project-scoped developer may submit where
// a global developer may not reach); it is NOT "project membership" —
// see projectScopedAssignmentCovers.
func projectAssignmentGrants(ctx context.Context, store controller.Store, identity *auth.Identity, project string, action auth.PermissionType, target auth.Target) bool {
	if identity == nil {
		return false
	}
	for _, a := range EffectiveAssignments(ctx, store, identity) {
		if a.Scope != auth.GlobalScope && auth.ScopeCovers(a.Scope, project) && a.Role.Grants(action, target) {
			return true
		}
	}
	return false
}

// projectScopedAssignmentCovers reports whether the identity holds ANY
// effective project-scoped assignment covering project — project
// membership in this tree's RBAC model: the scoped binding defines where
// the caller operates (readScope's pinned edge case), while their global
// roles decide what they may do there. Deliberately role-agnostic: the
// r03 principal "dev-a" is a global developer holding operator on
// team-a, and must reach team-a's surfaces as a member (the operator
// grant does not itself carry Write-on-Job).
func projectScopedAssignmentCovers(ctx context.Context, store controller.Store, identity *auth.Identity, project string) bool {
	if identity == nil {
		return false
	}
	for _, a := range EffectiveAssignments(ctx, store, identity) {
		if a.Scope != auth.GlobalScope && auth.ScopeCovers(a.Scope, project) {
			return true
		}
	}
	return false
}

// clusterTenantAccess reports whether identity sits inside the tenant
// boundary of the stored cluster c for (action, target). The read-side
// scoping rule (red-team findings, #49 posture narrowed to match the
// mutation convention):
//   - Admin: everything, everywhere.
//   - Auditor: every READ when auditorAll is set (audit evidence is its
//     purpose — consistent with the audit surface staying Admin/Auditor-only);
//     never any write.
//   - the cluster's recorded Owner (spec.owner, matched against
//     identity.Owner() — the same value CreateCluster stamps server-side).
//   - an effective assignment scoped to c.Spec.Project ("project:<name>",
//     NOT the global "*") whose role grants (action, target) — i.e. project
//     membership, with the role still deciding what the member may do.
//
// Global ("*") assignments and unscoped global roles do NOT cross the
// tenant boundary on their own: a viewer/developer/operator with zero
// project ties reads only clusters they own. identity == nil (dev mode)
// permits everything, exactly like Authorize/AuthorizeScoped.
func clusterTenantAccess(ctx context.Context, store controller.Store, identity *auth.Identity, c *controller.StoredCluster, action auth.PermissionType, target auth.Target, auditorAll bool) bool {
	if identity == nil {
		return true
	}
	if hasRole(identity, auth.RoleAdmin) {
		return true
	}
	if auditorAll && action == auth.Read && hasRole(identity, auth.RoleAuditor) {
		return true
	}
	if c.Spec.Owner != nil && *c.Spec.Owner == identity.Owner() {
		return true
	}
	return projectAssignmentGrants(ctx, store, identity, c.Spec.Project, action, target)
}

// identitySubject returns a pointer to id's subject, or nil for an
// unauthenticated (dev mode) caller — the shape every AuditEvent.Subject
// field needs.
func identitySubject(id *auth.Identity) *string {
	if id == nil {
		return nil
	}
	subject := id.Subject
	return &subject
}

func containsRoleScope(haystack []auth.RoleScope, needle auth.RoleScope) bool {
	for _, a := range haystack {
		if a == needle {
			return true
		}
	}
	return false
}

// wrapStoreErr converts a controller.Store failure into the canonical 500
// HTTPError, after logging it server-side (every Rust handler file's
// repeated `store_err` helper — clusters.rs/registry.rs/settings.rs/
// access.rs each define their own copy of the same two lines; this is the
// one shared Go equivalent).
func wrapStoreErr(err error) error {
	slog.Warn("api: store error", "error", err)
	return HTTPError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "store error"}
}

// storeErrContains reports whether err's message contains substr — the Go
// equivalent of the Rust reference's repeated
// `Err(StoreError::Backend(m)) if m.contains("no such ...")` pattern for
// distinguishing "not found" from a genuine backend failure.
func storeErrContains(err error, substr string) bool {
	return err != nil && strings.Contains(err.Error(), substr)
}
