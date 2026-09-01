package api

import (
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
