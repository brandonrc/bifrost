package api

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

func newLocalServer(t *testing.T) (*Server, controller.Store) {
	t.Helper()
	store := newMemStore(t)
	return &Server{Store: store, Local: auth.NewLocalAuthenticator(store, 3600, 90)}, store
}

func seedLocalUser(t *testing.T, s *Server, username, password string, role core.LocalRole) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := s.Store.CreateLocalUser(t.Context(), username, nil, hash, role); err != nil {
		t.Fatalf("create local user: %v", err)
	}
}

// --- Login (ADR-0011: local auth not enabled -> 404; every failure kind
// collapses to the same 401 invalid_credentials body) ---

func TestLogin_NotConfiguredIs404(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.Login(t.Context(), LoginRequestObject{Body: &LoginRequest{Username: "a", Password: "b"}})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestLogin_Success(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "alice", "hunter22", core.LocalRoleOperator)
	resp, err := s.Login(t.Context(), LoginRequestObject{Body: &LoginRequest{Username: "alice", Password: "hunter22"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := mustResponse[Login200JSONResponse](t, resp)
	if body.Token == "" || body.TokenType != "bearer" || body.Identity.Subject != "alice" {
		t.Errorf("login response = %+v", body)
	}
	if len(body.Identity.Roles) != 1 || body.Identity.Roles[0] != "operator" {
		t.Errorf("roles = %v, want [operator]", body.Identity.Roles)
	}
}

func TestLogin_UnknownUserAndWrongPasswordSameBody(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "bob", "correcthorse", core.LocalRoleViewer)

	_, err1 := s.Login(t.Context(), LoginRequestObject{Body: &LoginRequest{Username: "ghost", Password: "x"}})
	_, err2 := s.Login(t.Context(), LoginRequestObject{Body: &LoginRequest{Username: "bob", Password: "wrong"}})
	mustHTTPError(t, err1, 401)
	mustHTTPError(t, err2, 401)
	var h1, h2 HTTPError
	_ = errorsAs(err1, &h1)
	_ = errorsAs(err2, &h2)
	if h1.Code != "invalid_credentials" || h2.Code != "invalid_credentials" || h1.Message != h2.Message {
		t.Errorf("bodies differ: %+v vs %+v (ADR-0011: no user enumeration)", h1, h2)
	}
}

func errorsAs(err error, target *HTTPError) bool {
	he, ok := err.(HTTPError)
	if !ok {
		return false
	}
	*target = he
	return true
}

func TestLogin_MissingBodyRejected(t *testing.T) {
	s, _ := newLocalServer(t)
	_, err := s.Login(t.Context(), LoginRequestObject{})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

// --- Providers (always public, always mounted) ---

func TestProviders_LocalOnly(t *testing.T) {
	s, _ := newLocalServer(t)
	resp, err := s.Providers(t.Context(), ProvidersRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := mustResponse[Providers200JSONResponse](t, resp)
	if !body.Local || body.Oidc != nil {
		t.Errorf("providers = %+v, want local only", body)
	}
}

func TestProviders_NeitherConfigured(t *testing.T) {
	s := &Server{}
	resp, err := s.Providers(t.Context(), ProvidersRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := mustResponse[Providers200JSONResponse](t, resp)
	if body.Local || body.Oidc != nil {
		t.Errorf("providers = %+v, want neither", body)
	}
}

// --- Tokens (CreateToken/ListTokens/RevokeToken): authenticated + owner-scoped ---

func TestCreateToken_RequiresAuthentication(t *testing.T) {
	s, _ := newLocalServer(t)
	_, err := s.CreateToken(t.Context(), CreateTokenRequestObject{Body: &CreateTokenRequest{Label: "ci", ExpiresInDays: 7}})
	if err == nil {
		t.Fatal("expected 401")
	}
	mustHTTPError(t, err, 401)
}

func TestCreateToken_SuccessThenListThenRevoke(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "carol", "hunter22222", core.LocalRoleDeveloper)
	ctx := ctxWithIdentity(&auth.Identity{Subject: "carol"})

	resp, err := s.CreateToken(ctx, CreateTokenRequestObject{Body: &CreateTokenRequest{Label: "laptop", ExpiresInDays: 30}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	created := mustResponse[CreateToken201JSONResponse](t, resp)
	if created.Token == "" || created.Prefix == "" {
		t.Fatalf("created token = %+v", created)
	}

	listResp, err := s.ListTokens(ctx, ListTokensRequestObject{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	tokens := mustResponse[ListTokens200JSONResponse](t, listResp)
	if len(tokens) != 1 || tokens[0].Prefix != created.Prefix {
		t.Fatalf("tokens = %+v", tokens)
	}

	revokeResp, err := s.RevokeToken(ctx, RevokeTokenRequestObject{Prefix: created.Prefix})
	if err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	mustResponse[RevokeToken204Response](t, revokeResp)

	// Revoking an already-revoked (but still owned) token is idempotent
	// success, not 404 — the store's RevokeApiToken keys the "no such
	// token" failure on (prefix, owner), not on the current Revoked flag.
	// A nonexistent PREFIX is what 404s (covered by
	// TestRevokeToken_SomeoneElsesTokenIs404 for the ownership half).
	revokeAgain, err := s.RevokeToken(ctx, RevokeTokenRequestObject{Prefix: created.Prefix})
	if err != nil {
		t.Fatalf("idempotent re-revoke should succeed: %v", err)
	}
	mustResponse[RevokeToken204Response](t, revokeAgain)

	_, err = s.RevokeToken(ctx, RevokeTokenRequestObject{Prefix: "nonexistent"})
	if err == nil {
		t.Fatal("expected 404 for an unknown prefix")
	}
	mustHTTPError(t, err, 404)
}

func TestCreateToken_TTLTooLongRejected(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "dave", "hunter222222", core.LocalRoleDeveloper)
	ctx := ctxWithIdentity(&auth.Identity{Subject: "dave"})
	_, err := s.CreateToken(ctx, CreateTokenRequestObject{Body: &CreateTokenRequest{Label: "x", ExpiresInDays: 9999}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestRevokeToken_SomeoneElsesTokenIs404(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "erin", "hunter2222222", core.LocalRoleDeveloper)
	seedLocalUser(t, s, "frank", "hunter22222222", core.LocalRoleDeveloper)
	erinCtx := ctxWithIdentity(&auth.Identity{Subject: "erin"})
	frankCtx := ctxWithIdentity(&auth.Identity{Subject: "frank"})

	resp, err := s.CreateToken(erinCtx, CreateTokenRequestObject{Body: &CreateTokenRequest{Label: "x", ExpiresInDays: 7}})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	created := mustResponse[CreateToken201JSONResponse](t, resp)

	_, err = s.RevokeToken(frankCtx, RevokeTokenRequestObject{Prefix: created.Prefix})
	if err == nil {
		t.Fatal("expected 404: ownership must not be probed")
	}
	mustHTTPError(t, err, 404)
}

// --- Logout ---

func TestLogout_RequiresAuthentication(t *testing.T) {
	s, _ := newLocalServer(t)
	_, err := s.Logout(t.Context(), LogoutRequestObject{})
	if err == nil {
		t.Fatal("expected 401")
	}
	mustHTTPError(t, err, 401)
}

func TestLogout_RevokesPresentedPAT(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "gina", "hunter222222222", core.LocalRoleDeveloper)
	ctx := ctxWithIdentity(&auth.Identity{Subject: "gina"})

	created, err := s.CreateToken(ctx, CreateTokenRequestObject{Body: &CreateTokenRequest{Label: "x", ExpiresInDays: 7}})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	token := mustResponse[CreateToken201JSONResponse](t, created)

	logoutCtx := context.WithValue(ctx, bearerTokenContextKey{}, token.Token)
	resp, err := s.Logout(logoutCtx, LogoutRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[Logout204Response](t, resp)

	list, err := s.ListTokens(ctx, ListTokensRequestObject{})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	tokens := mustResponse[ListTokens200JSONResponse](t, list)
	if len(tokens) != 1 || !tokens[0].Revoked {
		t.Errorf("token should be revoked after logout: %+v", tokens)
	}
}

// --- Local user management (Admin-only) ---

func TestListUsers_AdminOnly(t *testing.T) {
	s, _ := newLocalServer(t)
	_, err := s.ListUsers(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), ListUsersRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestListUsers_ReturnsWireSafeView(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "hank", "hunter2222222222", core.LocalRoleAdmin)
	resp, err := s.ListUsers(ctxWithIdentity(admin()), ListUsersRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListUsers200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Username != "hank" || views[0].Role != "admin" {
		t.Fatalf("views = %+v", views)
	}
}

func TestCreateUser_SuccessThenConflictThenValidation(t *testing.T) {
	s, _ := newLocalServer(t)
	body := &CreateUserRequest{Username: "newuser", Password: "longenough1", Role: LocalRole("viewer")}
	resp, err := s.CreateUser(ctxWithIdentity(admin()), CreateUserRequestObject{Body: body})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[CreateUser201JSONResponse](t, resp)
	if view.Username != "newuser" || view.Role != "viewer" || view.Disabled {
		t.Errorf("view = %+v", view)
	}

	_, err = s.CreateUser(ctxWithIdentity(admin()), CreateUserRequestObject{Body: body})
	if err == nil {
		t.Fatal("expected 409 on duplicate username")
	}
	mustHTTPError(t, err, 409)

	shortPw := &CreateUserRequest{Username: "another", Password: "short", Role: LocalRole("viewer")}
	_, err = s.CreateUser(ctxWithIdentity(admin()), CreateUserRequestObject{Body: shortPw})
	if err == nil {
		t.Fatal("expected 400 for short password")
	}
	mustHTTPError(t, err, 400)

	badName := &CreateUserRequest{Username: "Not_Valid!", Password: "longenough1", Role: LocalRole("viewer")}
	_, err = s.CreateUser(ctxWithIdentity(admin()), CreateUserRequestObject{Body: badName})
	if err == nil {
		t.Fatal("expected 400 for invalid username")
	}
	mustHTTPError(t, err, 400)
}

func TestUpdateUser_RoleDisabledPasswordAndNotFound(t *testing.T) {
	s, _ := newLocalServer(t)
	seedLocalUser(t, s, "target", "hunter2222222222", core.LocalRoleViewer)

	newRole := LocalRole("operator")
	disabled := true
	resp, err := s.UpdateUser(ctxWithIdentity(admin()), UpdateUserRequestObject{
		Username: "target", Body: &UpdateUserRequest{Role: &newRole, Disabled: &disabled},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[UpdateUser200JSONResponse](t, resp)
	if view.Role != "operator" || !view.Disabled {
		t.Errorf("view = %+v", view)
	}

	_, err = s.UpdateUser(ctxWithIdentity(admin()), UpdateUserRequestObject{Username: "ghost", Body: &UpdateUserRequest{}})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)

	shortPw := "short"
	_, err = s.UpdateUser(ctxWithIdentity(admin()), UpdateUserRequestObject{Username: "target", Body: &UpdateUserRequest{Password: &shortPw}})
	if err == nil {
		t.Fatal("expected 400 for short password")
	}
	mustHTTPError(t, err, 400)
}
