// Local-auth routes (ADR-0011): login, provider metadata, personal access
// tokens, logout, and Admin-only local user management.
//
// mobula-api mounts this router only when local auth is enabled
// (`serve --local-auth`), except GET /api/v1/auth/providers, which is
// always mounted and public (it's in the unauthenticated allowlist,
// middleware.go's isPublic) so the login page can render the right form.
// Go's generated strict-server has no such conditional mount — every
// operation is always reachable — so requireLocal below is this port's
// equivalent: every route except Providers answers 404 "local auth is not
// enabled" when s.Local is nil, the same outcome a caller gets from the
// Rust reference's absent route. PAT management requires an authenticated
// identity and is owner-scoped: a caller only ever sees/revokes their own
// tokens. Ported from mobula-api's local_auth.rs.
//
// Wire-contract notes:
//   - every login failure — unknown user, wrong password, locked, disabled,
//     even a backend error — returns the SAME 401 {"error":"invalid_credentials"}
//     body (no user enumeration); the distinction lives only in the audit
//     trail;
//   - token plaintext is returned exactly once (POST .../tokens, 201); list
//     views never contain hashes (ApiTokenView);
//   - revoking someone else's token and revoking a nonexistent one are both
//     404 (no ownership probing).
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// requireLocal returns s.Local, or a 404 error when local auth isn't
// enabled — see the package doc comment above for why this exists.
func (s *Server) requireLocal() (*auth.LocalAuthenticator, error) {
	if s.Local == nil {
		return nil, notFound("local auth is not enabled")
	}
	return s.Local, nil
}

func unauthorized(msg string) error {
	return HTTPError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: msg}
}

// invalidCredentials is the one and only login-failure wire shape.
// Lockout/disablement is visible only in the audit trail (ADR-0011).
var invalidCredentials = HTTPError{Status: http.StatusUnauthorized, Code: "invalid_credentials", Message: "invalid credentials"}

func (s *Server) auditLogin(ctx context.Context, username string, decision core.AuditDecision, reason *string, status int) {
	action := "login"
	method := "POST"
	path := "/api/v1/auth/login"
	st := uint16(status)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: &username, Decision: decision, Reason: reason,
		Action: &action, Method: &method, Path: &path, Status: &st,
	})
}

// Login authenticates a username/password (ADR-0011). Public (allowlisted
// in middleware.go's isPublic); rate-limited by bcrypt cost and the
// 5-strikes/5-minute account lockout inside auth.LocalAuthenticator.
func (s *Server) Login(ctx context.Context, req LoginRequestObject) (LoginResponseObject, error) {
	local, err := s.requireLocal()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	outcome, lerr := local.Login(ctx, req.Body.Username, req.Body.Password)
	if lerr != nil {
		reason := "invalid_credentials"
		var authErr auth.LocalAuthError
		if errors.As(lerr, &authErr) {
			// Exhaustive over every LocalAuthErrorKind (ported verbatim from
			// local_auth.rs's login match): InvalidCredentials/UnknownUser
			// collapse to the same "invalid_credentials" reason as the
			// fallback above, spelled out explicitly rather than left to
			// the zero-value default so a new LocalAuthErrorKind variant
			// fails this switch at compile time (exhaustive lint) instead
			// of silently inheriting whatever the fallback happens to be.
			switch authErr.Kind {
			case auth.LocalAuthErrInvalidCredentials, auth.LocalAuthErrUnknownUser:
				reason = "invalid_credentials"
			case auth.LocalAuthErrLocked:
				reason = "locked"
			case auth.LocalAuthErrDisabled:
				reason = "disabled"
			case auth.LocalAuthErrBackend, auth.LocalAuthErrTTLTooLong:
				reason = "backend_error"
			}
		}
		s.auditLogin(ctx, req.Body.Username, core.AuditDecisionDeny, &reason, http.StatusUnauthorized)
		return nil, invalidCredentials
	}
	s.auditLogin(ctx, req.Body.Username, core.AuditDecisionAllow, nil, http.StatusOK)
	roles := make([]string, len(outcome.Identity.Roles))
	for i, r := range outcome.Identity.Roles {
		roles[i] = RoleStr(r)
	}
	return Login200JSONResponse(LoginResponse{
		Token: outcome.Token.Token, TokenType: "bearer", ExpiresAt: int64(outcome.ExpiresAt),
		Identity: LoginIdentity{Subject: outcome.Identity.Subject, Roles: roles},
	}), nil
}

// Providers reports which auth providers this deployment offers
// (login-page metadata). Always mounted, always public.
func (s *Server) Providers(_ context.Context, _ ProvidersRequestObject) (ProvidersResponseObject, error) {
	var oidc *OidcProviderInfo
	if s.Validator != nil {
		oidc = &OidcProviderInfo{Issuer: s.Validator.Issuer()}
	}
	return Providers200JSONResponse(ProvidersResponse{Local: s.Local != nil, Oidc: oidc}), nil
}

// CreateToken mints a personal access token for the caller (ADR-0011). Any
// authenticated identity; the plaintext is shown once.
func (s *Server) CreateToken(ctx context.Context, req CreateTokenRequestObject) (CreateTokenResponseObject, error) {
	local, err := s.requireLocal()
	if err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if identity == nil {
		return nil, unauthorized("authentication required")
	}
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	minted, record, ierr := local.IssueToken(ctx, identity.Subject, req.Body.Label, uint64(req.Body.ExpiresInDays))
	if ierr != nil {
		var authErr auth.LocalAuthError
		if errors.As(ierr, &authErr) && authErr.Kind == auth.LocalAuthErrTTLTooLong {
			return nil, badRequest("expires_in_days must be between 1 and the server maximum (90)")
		}
		return nil, wrapStoreErr(ierr)
	}
	action := "issue_token"
	method := "POST"
	path := "/api/v1/auth/tokens"
	status := uint16(http.StatusCreated)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity), Decision: core.AuditDecisionAllow,
		Action: &action, Method: &method, Path: &path, Status: &status,
	})
	return CreateToken201JSONResponse(CreateTokenResponse{
		Prefix: minted.Prefix, Token: minted.Token, ExpiresAt: int64(record.ExpiresAt),
	}), nil
}

// ListTokens lists the caller's own tokens. Hashes are never serialized.
func (s *Server) ListTokens(ctx context.Context, _ ListTokensRequestObject) (ListTokensResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if identity == nil {
		return nil, unauthorized("authentication required")
	}
	tokens, err := s.Store.ListApiTokens(ctx, identity.Subject)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	views := make([]ApiTokenView, len(tokens))
	for i := range tokens {
		v := tokens[i].View()
		views[i] = ApiTokenView{
			Prefix: v.Prefix, Username: v.Username, Label: v.Label, CreatedAt: int64(v.CreatedAt),
			ExpiresAt: int64(v.ExpiresAt), Revoked: v.Revoked, LastUsedAt: uint64PtrToInt64Ptr(v.LastUsedAt),
		}
	}
	return ListTokens200JSONResponse(views), nil
}

func uint64PtrToInt64Ptr(p *uint64) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

// RevokeToken revokes one of the caller's own tokens. Someone else's token
// (or a nonexistent prefix) is a 404 — ownership can't be probed.
func (s *Server) RevokeToken(ctx context.Context, req RevokeTokenRequestObject) (RevokeTokenResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if identity == nil {
		return nil, unauthorized("authentication required")
	}
	if err := s.Store.RevokeApiToken(ctx, req.Prefix, identity.Subject); err != nil {
		return nil, notFound("no such token")
	}
	action := "revoke_token"
	method := "DELETE"
	path := "/api/v1/auth/tokens/" + req.Prefix
	status := uint16(http.StatusNoContent)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity), Decision: core.AuditDecisionAllow,
		Action: &action, Method: &method, Path: &path, Status: &status,
	})
	return RevokeToken204Response{}, nil
}

// Logout logs the caller out: if they authenticated with a PAT, it is
// revoked; otherwise a 204 no-op (JWTs are stateless — there is nothing
// server-side to kill).
func (s *Server) Logout(ctx context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if identity == nil {
		return nil, unauthorized("authentication required")
	}
	if bearer, ok := BearerTokenFromContext(ctx); ok {
		if prefix, ok := auth.TokenPrefix(bearer); ok {
			// Owner-scoped revoke; a nonexistent/already-revoked token is fine.
			_ = s.Store.RevokeApiToken(ctx, prefix, identity.Subject)
		}
	}
	action := "logout"
	method := "POST"
	path := "/api/v1/auth/logout"
	status := uint16(http.StatusNoContent)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity), Decision: core.AuditDecisionAllow,
		Action: &action, Method: &method, Path: &path, Status: &status,
	})
	return Logout204Response{}, nil
}

// --- Local user management (Admin-only; api-v1.md §5.15) ---

func (s *Server) requireAdmin(ctx context.Context, identity *auth.Identity) error {
	return Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster)
}

func passwordOK(password string) bool { return len(password) >= 8 }

func localUserView(r *core.LocalUserRecord) LocalUserView {
	v := r.View()
	return LocalUserView{Username: v.Username, Email: v.Email, Role: LocalRole(v.Role), Disabled: v.Disabled, CreatedAt: int64(v.CreatedAt)}
}

// ListUsers lists all local users. Admin-only; hashes never serialize.
func (s *Server) ListUsers(ctx context.Context, _ ListUsersRequestObject) (ListUsersResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if err := s.requireAdmin(ctx, identity); err != nil {
		return nil, err
	}
	users, err := s.Store.ListLocalUsers(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	views := make([]LocalUserView, len(users))
	for i := range users {
		views[i] = localUserView(&users[i])
	}
	return ListUsers200JSONResponse(views), nil
}

// CreateUser creates a local user. Admin-only. 201 with the wire-safe
// view; 400 on a bad username/short password; 409 when the username is
// taken.
func (s *Server) CreateUser(ctx context.Context, req CreateUserRequestObject) (CreateUserResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if err := s.requireAdmin(ctx, identity); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	body := req.Body
	if !core.IsK8sName(body.Username) {
		return nil, badRequest("username must be a valid Kubernetes name (RFC 1123 subdomain)")
	}
	if !passwordOK(body.Password) {
		return nil, badRequest("password must be at least 8 characters")
	}
	role, ok := core.ParseLocalRole(string(body.Role))
	if !ok {
		return nil, badRequest("invalid role")
	}
	existing, err := s.Store.GetLocalUser(ctx, body.Username)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if existing != nil {
		return nil, conflict("username already taken")
	}
	hash, herr := auth.HashPassword(body.Password)
	if herr != nil {
		return nil, wrapStoreErr(herr)
	}
	if err := s.Store.CreateLocalUser(ctx, body.Username, body.Email, hash, role); err != nil {
		return nil, wrapStoreErr(err)
	}
	action := "create_user"
	method := "POST"
	path := "/api/v1/auth/users"
	status := uint16(http.StatusCreated)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity), Decision: core.AuditDecisionAllow,
		Action: &action, Method: &method, Path: &path, Status: &status,
	})
	return CreateUser201JSONResponse(LocalUserView{
		Username: body.Username, Email: body.Email, Role: body.Role, Disabled: false, CreatedAt: int64(controller.NowUnix()),
	}), nil
}

// UpdateUser updates a local user's role, disabled flag, and/or password.
// Admin-only; 404 for an unknown user. Changing your OWN role/disabled is
// allowed in v0 (no footgun guard) but is audit-logged loudly.
func (s *Server) UpdateUser(ctx context.Context, req UpdateUserRequestObject) (UpdateUserResponseObject, error) {
	if _, err := s.requireLocal(); err != nil {
		return nil, err
	}
	identity, _ := IdentityFromContext(ctx)
	if err := s.requireAdmin(ctx, identity); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, badRequest("missing request body")
	}
	body := req.Body
	if body.Password != nil && !passwordOK(*body.Password) {
		return nil, badRequest("password must be at least 8 characters")
	}
	user, err := s.Store.GetLocalUser(ctx, req.Username)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if user == nil {
		return nil, notFound("no such user")
	}

	var role core.LocalRole
	if body.Role != nil {
		r, ok := core.ParseLocalRole(string(*body.Role))
		if !ok {
			return nil, badRequest("invalid role")
		}
		role = r
		if err := s.Store.SetLocalUserRole(ctx, req.Username, role); err != nil {
			return nil, wrapStoreErr(err)
		}
	}
	if body.Disabled != nil {
		if err := s.Store.SetLocalUserDisabled(ctx, req.Username, *body.Disabled); err != nil {
			return nil, wrapStoreErr(err)
		}
	}
	if body.Password != nil {
		hash, herr := auth.HashPassword(*body.Password)
		if herr != nil {
			return nil, wrapStoreErr(herr)
		}
		if err := s.Store.SetLocalUserPassword(ctx, req.Username, hash); err != nil {
			return nil, wrapStoreErr(err)
		}
	}

	action := "update_user"
	method := "PUT"
	path := "/api/v1/auth/users/" + req.Username
	status := uint16(http.StatusOK)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity), Decision: core.AuditDecisionAllow,
		Action: &action, Method: &method, Path: &path, Status: &status,
	})

	view := LocalUserView{Username: user.Username, Email: user.Email, Role: LocalRole(user.Role), Disabled: user.Disabled, CreatedAt: int64(user.CreatedAt)}
	if body.Role != nil {
		view.Role = *body.Role
	}
	if body.Disabled != nil {
		view.Disabled = *body.Disabled
	}
	return UpdateUser200JSONResponse(view), nil
}
