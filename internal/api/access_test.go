package api

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
)

func TestIdentity_DevModeDefault(t *testing.T) {
	s := &Server{}
	resp, err := s.Identity(ctxWithIdentity(nil), IdentityRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := mustResponse[Identity200JSONResponse](t, resp)
	if id.Subject != "dev" || len(id.Roles) != 1 || id.Roles[0] != "admin" {
		t.Errorf("dev identity = %+v, want subject=dev roles=[admin]", id)
	}
}

func TestIdentity_AuthenticatedCaller(t *testing.T) {
	s := &Server{}
	email := "alice@example.com"
	who := testIdentity("alice", auth.RoleViewer, auth.RoleOperator)
	who.Email = &email
	who.Groups = []string{"team-a"}
	resp, err := s.Identity(ctxWithIdentity(who), IdentityRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := mustResponse[Identity200JSONResponse](t, resp)
	if id.Subject != "alice" || id.Email == nil || *id.Email != email {
		t.Errorf("identity = %+v", id)
	}
	if len(id.Roles) != 2 || id.Roles[0] != "viewer" || id.Roles[1] != "operator" {
		t.Errorf("roles = %v", id.Roles)
	}
}

// The projects a caller may name are part of their identity, because a client
// that has to name one otherwise guesses — which is how the JupyterLab
// extension came to ship a hardcoded default project and answer its users'
// first Start click with a 403 (bifrost-jupyter#3).
func TestIdentity_NamesTheProjectsTheCallerHoldsGrantsIn(t *testing.T) {
	store := newMemStore(t)
	s := &Server{Store: store}
	who := testIdentity("alice", auth.RoleDeveloper)
	// One grant from the token's groups, one written by an administrator, and
	// a second role in a project already named — the response merges them.
	who.ProjectRoles = []auth.RoleScope{{Role: auth.RoleViewer, Scope: "project:team-b"}}
	for _, a := range []struct{ role, scope string }{
		{"operator", "project:team-a"},
		{"operator", "project:team-b"},
		{"admin", "*"},
	} {
		if err := store.UpsertRoleAssignment(context.Background(), "alice", a.role, a.scope); err != nil {
			t.Fatalf("seeding %s@%s: %v", a.role, a.scope, err)
		}
	}
	resp, err := s.Identity(ctxWithIdentity(who), IdentityRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := mustResponse[Identity200JSONResponse](t, resp)

	// Sorted by name, one entry per project, roles merged and sorted, and the
	// global grant absent: no list can enumerate where a global admin may act.
	want := []ProjectGrant{
		{Name: "team-a", Roles: []string{"operator"}},
		{Name: "team-b", Roles: []string{"operator", "viewer"}},
	}
	if len(id.Projects) != len(want) {
		t.Fatalf("projects = %+v, want %+v", id.Projects, want)
	}
	for i, w := range want {
		got := id.Projects[i]
		if got.Name != w.Name || len(got.Roles) != len(w.Roles) {
			t.Fatalf("projects[%d] = %+v, want %+v", i, got, w)
		}
		for j, role := range w.Roles {
			if got.Roles[j] != role {
				t.Errorf("projects[%d].roles = %v, want %v", i, got.Roles, w.Roles)
			}
		}
	}
}

// A caller whose only grant is global sees no projects and a global role. The
// empty list is the truth — they may act in every project, so none is named —
// and a client reads `roles` before concluding the caller may do nothing.
func TestIdentity_AGlobalGrantNamesNoProject(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	resp, err := s.Identity(ctxWithIdentity(testIdentity("root", auth.RoleAdmin)), IdentityRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id := mustResponse[Identity200JSONResponse](t, resp)
	if len(id.Projects) != 0 {
		t.Errorf("projects = %+v, want none", id.Projects)
	}
	if len(id.Roles) != 1 || id.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin]", id.Roles)
	}
}

// The field is required by the contract, so it is a list on every path —
// including the store-less dev identity, where a nil would serialise as null
// and break a client that iterates it without a guard.
func TestIdentity_ProjectsIsAlwaysAList(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Server
		id   *auth.Identity
	}{
		{"dev mode", &Server{}, nil},
		{"no store", &Server{}, testIdentity("alice", auth.RoleDeveloper)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.s.Identity(ctxWithIdentity(tc.id), IdentityRequestObject{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			id := mustResponse[Identity200JSONResponse](t, resp)
			if id.Projects == nil {
				t.Error("projects is nil; the contract requires a list")
			}
		})
	}
}

func TestListRoles_AdminOnlyAndSourceReflectsValidator(t *testing.T) {
	s := &Server{}
	if _, err := s.ListRoles(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), ListRolesRequestObject{}); err == nil {
		t.Fatal("expected non-admin to be denied")
	} else {
		mustHTTPError(t, err, 403)
	}

	resp, err := s.ListRoles(ctxWithIdentity(admin()), ListRolesRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rr := mustResponse[ListRoles200JSONResponse](t, resp)
	if rr.Source != "local" || rr.Mappings != nil || rr.Editable {
		t.Errorf("no-validator response = %+v, want source=local mappings=nil editable=false", rr)
	}
}

func TestListAssignments_UpsertAndDelete(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	who := admin()

	// Validation: unknown role, invalid scope, empty principal.
	if _, err := s.UpsertAssignment(ctxWithIdentity(who), UpsertAssignmentRequestObject{
		Principal: "alice", Body: &UpsertAssignment{Role: "wizard", Scope: "*"},
	}); err == nil {
		t.Fatal("unknown role should be rejected")
	} else {
		mustHTTPError(t, err, 400)
	}
	if _, err := s.UpsertAssignment(ctxWithIdentity(who), UpsertAssignmentRequestObject{
		Principal: "alice", Body: &UpsertAssignment{Role: "operator", Scope: "not-a-scope"},
	}); err == nil {
		t.Fatal("invalid scope should be rejected")
	} else {
		mustHTTPError(t, err, 400)
	}
	if _, err := s.UpsertAssignment(ctxWithIdentity(who), UpsertAssignmentRequestObject{
		Principal: "", Body: &UpsertAssignment{Role: "operator", Scope: "*"},
	}); err == nil {
		t.Fatal("empty principal should be rejected")
	} else {
		mustHTTPError(t, err, 400)
	}

	// Happy path: upsert, then it shows up in the list.
	resp, err := s.UpsertAssignment(ctxWithIdentity(who), UpsertAssignmentRequestObject{
		Principal: "alice", Body: &UpsertAssignment{Role: "operator", Scope: "project:proj-a"},
	})
	if err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	view := mustResponse[UpsertAssignment200JSONResponse](t, resp)
	if view.Principal != "alice" || view.Role != "operator" || view.Scope != "project:proj-a" {
		t.Errorf("view = %+v", view)
	}

	listResp, err := s.ListAssignments(ctxWithIdentity(who), ListAssignmentsRequestObject{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	rows := mustResponse[ListAssignments200JSONResponse](t, listResp)
	if len(rows) != 1 || rows[0].Principal != "alice" {
		t.Fatalf("rows = %+v", rows)
	}

	// Delete: 404 for a non-matching triple, 204 for the real one.
	if _, err := s.DeleteAssignment(ctxWithIdentity(who), DeleteAssignmentRequestObject{
		Principal: "alice", Params: DeleteAssignmentParams{Role: "viewer", Scope: "project:proj-a"},
	}); err == nil {
		t.Fatal("mismatched triple should 404")
	} else {
		mustHTTPError(t, err, 404)
	}

	delResp, err := s.DeleteAssignment(ctxWithIdentity(who), DeleteAssignmentRequestObject{
		Principal: "alice", Params: DeleteAssignmentParams{Role: "operator", Scope: "project:proj-a"},
	})
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, ok := delResp.(DeleteAssignment204Response); !ok {
		t.Fatalf("response = %#v, want 204", delResp)
	}

	listResp, err = s.ListAssignments(ctxWithIdentity(who), ListAssignmentsRequestObject{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if rows := mustResponse[ListAssignments200JSONResponse](t, listResp); len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, want empty", rows)
	}
}

func TestAssignmentRoutes_AdminOnly(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	notAdmin := testIdentity("op", auth.RoleOperator)

	if _, err := s.ListAssignments(ctxWithIdentity(notAdmin), ListAssignmentsRequestObject{}); err == nil {
		t.Fatal("expected denial")
	} else {
		mustHTTPError(t, err, 403)
	}
	if _, err := s.UpsertAssignment(ctxWithIdentity(notAdmin), UpsertAssignmentRequestObject{
		Principal: "alice", Body: &UpsertAssignment{Role: "viewer", Scope: "*"},
	}); err == nil {
		t.Fatal("expected denial")
	} else {
		mustHTTPError(t, err, 403)
	}
	if _, err := s.DeleteAssignment(ctxWithIdentity(notAdmin), DeleteAssignmentRequestObject{
		Principal: "alice", Params: DeleteAssignmentParams{Role: "viewer", Scope: "*"},
	}); err == nil {
		t.Fatal("expected denial")
	} else {
		mustHTTPError(t, err, 403)
	}
}

func TestAssignmentRoutes_NoStoreIs503(t *testing.T) {
	s := &Server{}
	who := admin()
	if resp, err := s.ListAssignments(ctxWithIdentity(who), ListAssignmentsRequestObject{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(ListAssignments503Response); !ok {
		t.Fatalf("response = %#v, want 503", resp)
	}
	if resp, err := s.UpsertAssignment(ctxWithIdentity(who), UpsertAssignmentRequestObject{
		Principal: "alice", Body: &UpsertAssignment{Role: "viewer", Scope: "*"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(UpsertAssignment503Response); !ok {
		t.Fatalf("response = %#v, want 503", resp)
	}
	if resp, err := s.DeleteAssignment(ctxWithIdentity(who), DeleteAssignmentRequestObject{
		Principal: "alice", Params: DeleteAssignmentParams{Role: "viewer", Scope: "*"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if _, ok := resp.(DeleteAssignment503Response); !ok {
		t.Fatalf("response = %#v, want 503", resp)
	}
}
