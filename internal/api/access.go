// Identity & access read/write surface (api-v1.md §5.8). GET /api/v1/identity
// is "who am I" for the shell's identity chip and role-gated rendering;
// GET /api/v1/access/roles exposes the effective group->role mappings; the
// /api/v1/access/assignments routes are the scoped role-binding CRUD (#49).
// Ported from mobula-api's access.rs.
//
// GET /identity mounts unconditionally and needs no permission check — every
// deployment has an identity, and in dev mode (no validator, no local auth)
// it returns the specced dev identity so the unauthenticated dev loop
// renders the full console. The rest of this file is Admin-only
// (access-control surfaces, api-v1.md §2.2), classified with
// auth.TargetCluster like the registry/settings surfaces.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// Identity is "who am I": the resolved identity for any authenticated
// caller. In dev mode (no validator AND no local auth) the auth middleware
// attaches no identity, and this returns the specced dev identity so the
// unauthenticated dev loop renders the full console.
func (s *Server) Identity(ctx context.Context, _ IdentityRequestObject) (IdentityResponseObject, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok || identity == nil {
		return Identity200JSONResponse(IdentityResponse{
			Subject: "dev",
			Groups:  []string{},
			Roles:   []string{"admin"},
		}), nil
	}
	roles := make([]string, len(identity.Roles))
	for i, r := range identity.Roles {
		roles[i] = RoleStr(r)
	}
	groups := identity.Groups
	if groups == nil {
		groups = []string{}
	}
	return Identity200JSONResponse(IdentityResponse{
		Subject: identity.Subject,
		Email:   identity.Email,
		Groups:  groups,
		Roles:   roles,
	}), nil
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// ListRoles returns the effective role mappings for the access page.
// Admin-only. v1 is read-only: with an OIDC validator the mappings come
// from the auth config file; without one (local auth) mappings is null and
// roles are managed per-user via the (T12) local-auth users API.
func (s *Server) ListRoles(ctx context.Context, _ ListRolesRequestObject) (ListRolesResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	if s.Validator == nil {
		return ListRoles200JSONResponse(RolesResponse{Mappings: nil, Source: "local", Editable: false}), nil
	}
	m := s.Validator.RoleMappings()
	return ListRoles200JSONResponse(RolesResponse{
		Mappings: &RoleMappingsView{
			Admin:     nonNilStrings(m.Admin),
			Auditor:   nonNilStrings(m.Auditor),
			Developer: nonNilStrings(m.Developer),
			Operator:  nonNilStrings(m.Operator),
			Viewer:    nonNilStrings(m.Viewer),
		},
		Source:   "file",
		Editable: false,
	}), nil
}

func assignmentView(a controller.RoleAssignment) AssignmentView {
	return AssignmentView{
		Principal: a.Principal,
		Role:      a.Role,
		Scope:     a.Scope,
		CreatedAt: int64(a.CreatedAt),
	}
}

// ListAssignments lists every scoped role assignment, Admin-only.
func (s *Server) ListAssignments(ctx context.Context, _ ListAssignmentsRequestObject) (ListAssignmentsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return ListAssignments503Response{}, nil
	}
	rows, err := s.Store.ListRoleAssignments(ctx, nil)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	views := make([]AssignmentView, len(rows))
	for i, a := range rows {
		views[i] = assignmentView(a)
	}
	return ListAssignments200JSONResponse(views), nil
}

// looksLikeSubUUID reports whether principal is shaped like a Keycloak sub
// (canonical UUID: 8-4-4-4-12 lowercase hex). Used only to decide whether to
// log a hint that a non-UUID principal must match a preferred_username
// (#88). Ported from access.rs's looks_like_sub_uuid.
func looksLikeSubUUID(principal string) bool {
	groupLens := [5]int{8, 4, 4, 4, 12}
	parts := strings.Split(principal, "-")
	if len(parts) != len(groupLens) {
		return false
	}
	for i, p := range parts {
		if len(p) != groupLens[i] {
			return false
		}
		for _, c := range p {
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// UpsertAssignment creates or replaces one scoped role assignment,
// Admin-only. The role and scope grammar are validated here — the store is
// dumb persistence.
func (s *Server) UpsertAssignment(ctx context.Context, req UpsertAssignmentRequestObject) (UpsertAssignmentResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	if req.Principal == "" {
		return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: "principal must not be empty"}
	}
	if req.Body == nil {
		return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: "missing request body"}
	}
	if _, ok := auth.ParseRole(req.Body.Role); !ok {
		return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request",
			Message: fmt.Sprintf("unknown role %q (viewer|developer|operator|admin)", req.Body.Role)}
	}
	if !auth.ValidScope(req.Body.Scope) {
		return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request",
			Message: fmt.Sprintf("invalid scope %q (\"*\" or \"project:<name>\")", req.Body.Scope)}
	}
	// #88: assignments are matched at evaluation time against BOTH the
	// token sub (opaque Keycloak UUID) and preferred_username. A
	// principal that is neither UUID-shaped nor obviously a username is
	// likely a typo that will never match a caller — surface it
	// (non-blocking; the grant is still stored).
	if !looksLikeSubUUID(req.Principal) {
		slog.Info("api: assignment principal is not a Keycloak sub UUID: it will match a caller "+
			"only if it equals their token preferred_username (#88)", "principal", req.Principal)
	}
	if s.Store == nil {
		return UpsertAssignment503Response{}, nil
	}
	if err := s.Store.UpsertRoleAssignment(ctx, req.Principal, req.Body.Role, req.Body.Scope); err != nil {
		return nil, wrapStoreErr(err)
	}
	action := "upsert_assignment"
	method := "PUT"
	path := "/api/v1/access/assignments/" + req.Principal
	status := uint16(http.StatusOK)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts:       controller.NowUnix(),
		Subject:  identitySubject(identity),
		Decision: core.AuditDecisionAllow,
		Action:   &action,
		Method:   &method,
		Path:     &path,
		Status:   &status,
	})

	rows, err := s.Store.ListRoleAssignments(ctx, &req.Principal)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	for _, a := range rows {
		if a.Role == req.Body.Role && a.Scope == req.Body.Scope {
			return UpsertAssignment200JSONResponse(assignmentView(a)), nil
		}
	}
	return nil, wrapStoreErr(fmt.Errorf("assignment vanished after upsert"))
}

// DeleteAssignment removes one scoped role assignment, Admin-only; 404 when
// the (principal, role, scope) triple doesn't exist.
func (s *Server) DeleteAssignment(ctx context.Context, req DeleteAssignmentRequestObject) (DeleteAssignmentResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	if s.Store == nil {
		return DeleteAssignment503Response{}, nil
	}
	err := s.Store.DeleteRoleAssignment(ctx, req.Principal, req.Params.Role, req.Params.Scope)
	switch {
	case err == nil:
		action := "delete_assignment"
		method := "DELETE"
		path := "/api/v1/access/assignments/" + req.Principal
		status := uint16(http.StatusNoContent)
		EmitAudit(ctx, s.Store, &core.AuditEvent{
			Ts:       controller.NowUnix(),
			Subject:  identitySubject(identity),
			Decision: core.AuditDecisionAllow,
			Action:   &action,
			Method:   &method,
			Path:     &path,
			Status:   &status,
		})
		return DeleteAssignment204Response{}, nil
	case storeErrContains(err, "no such assignment"):
		return nil, HTTPError{Status: http.StatusNotFound, Code: "not_found", Message: "no such assignment"}
	default:
		return nil, wrapStoreErr(err)
	}
}
