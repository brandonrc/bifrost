package core

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Local-auth domain types (ADR-0011): users and opaque API tokens stored
// by the control plane. Bifrost stores credentials, never signs them.
//
// Hash discipline: PasswordHash / TokenHash live ONLY on the stored
// records (LocalUserRecord, ApiTokenRecord), which are deliberately never
// marshaled to JSON. The wire-facing projections (LocalUserView,
// ApiTokenView) carry no secret material at all, so a handler can never
// accidentally serialize a hash.

// LocalRole is a local user's role (ADR-0011: roles are a column on the
// user, resolved per request — no claim staleness). Stored as TEXT in
// local_users.role; the vocabulary mirrors the auth package's Role (kept
// as a separate type so core never depends on the auth package).
type LocalRole string

const (
	LocalRoleViewer    LocalRole = "viewer"
	LocalRoleDeveloper LocalRole = "developer"
	LocalRoleOperator  LocalRole = "operator"
	LocalRoleAdmin     LocalRole = "admin"
	// LocalRoleAuditor is audit-trail read access only (separation of
	// duties, issue #59).
	LocalRoleAuditor LocalRole = "auditor"
)

// AsStr returns the wire value.
func (r LocalRole) AsStr() string {
	switch r {
	case LocalRoleViewer:
		return "viewer"
	case LocalRoleDeveloper:
		return "developer"
	case LocalRoleOperator:
		return "operator"
	case LocalRoleAdmin:
		return "admin"
	case LocalRoleAuditor:
		return "auditor"
	}
	return string(r)
}

// ParseLocalRole parses a wire value into a LocalRole.
func ParseLocalRole(s string) (LocalRole, bool) {
	switch s {
	case "viewer":
		return LocalRoleViewer, true
	case "developer":
		return LocalRoleDeveloper, true
	case "operator":
		return LocalRoleOperator, true
	case "admin":
		return LocalRoleAdmin, true
	case "auditor":
		return LocalRoleAuditor, true
	}
	return "", false
}

// UnmarshalJSON rejects any value other than the known LocalRole variants,
// mirroring serde's strict enum deserialization.
func (r *LocalRole) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, ok := ParseLocalRole(s)
	if !ok {
		return fmt.Errorf("core: invalid LocalRole %q", s)
	}
	*r = v
	return nil
}

// LocalUserRecord is a stored local user — the full row, INCLUDING the
// bcrypt password hash. Never marshaled to JSON; handlers project to
// LocalUserView.
type LocalUserRecord struct {
	Username string
	Email    *string
	// PasswordHash is the bcrypt hash of the password. Store-facing only.
	PasswordHash string
	Role         LocalRole
	Disabled     bool
	// CreatedAt is unix seconds.
	CreatedAt uint64
	// FailedLogins is the count of consecutive failed logins since the
	// last success (reset on lock).
	FailedLogins uint32
	// LockedUntil is unix seconds until which logins are refused; nil
	// when not locked.
	LockedUntil *uint64
}

// MarshalJSON always fails: the Rust reference makes LocalUserRecord
// deliberately non-Serialize (a compile error to marshal it), since it
// carries the bcrypt password hash. Go has no such compile-time guard, so
// this is the runtime equivalent — callers must go through View().
func (r LocalUserRecord) MarshalJSON() ([]byte, error) {
	return nil, errors.New("core: LocalUserRecord must never be marshaled — use View()")
}

// LocalUserView is the public projection of a local user — everything
// EXCEPT the password hash and the lockout internals that are nobody
// else's business.
type LocalUserView struct {
	Username string    `json:"username"`
	Email    *string   `json:"email"`
	Role     LocalRole `json:"role"`
	Disabled bool      `json:"disabled"`
	// CreatedAt is unix seconds.
	CreatedAt uint64 `json:"created_at"`
}

// localUserViewAlias breaks the recursion MarshalJSON would otherwise
// cause by re-entering LocalUserView's own MarshalJSON.
type localUserViewAlias LocalUserView

// MarshalJSON refuses to serialize a zero-value Role. Unlike Engine or
// AuditDecision, LocalRole has no documented Rust #[derive(Default)] — a
// LocalUserView built without setting Role would otherwise marshal as
// `"role":""`, which the frozen OpenAPI contract's LocalRole enum schema
// rejects, and there is no safe default to substitute (a role is a
// security-relevant field; guessing one would be worse than failing).
// Failing loudly here mirrors LocalUserRecord's error-guard style, catching
// the bug at the marshal site instead of shipping contract-invalid egress.
func (v LocalUserView) MarshalJSON() ([]byte, error) {
	if v.Role == "" {
		return nil, errors.New("core: LocalUserView.Role must be set — a zero-value Role is not a valid wire enum and has no documented default")
	}
	return json.Marshal(localUserViewAlias(v))
}

// View projects a LocalUserRecord to its wire-safe LocalUserView.
func (r *LocalUserRecord) View() LocalUserView {
	return LocalUserView{
		Username:  r.Username,
		Email:     r.Email,
		Role:      r.Role,
		Disabled:  r.Disabled,
		CreatedAt: r.CreatedAt,
	}
}

// ApiTokenRecord is a stored opaque API token (ADR-0011) — the full row,
// INCLUDING the bcrypt token hash. Never marshaled to JSON; handlers
// project to ApiTokenView. The plaintext token is shown exactly once at
// issuance and never stored.
type ApiTokenRecord struct {
	// Prefix is the first 8 url-safe characters of the token — the
	// lookup key. Not secret on its own (the remaining 32 hex chars
	// carry the entropy).
	Prefix string
	// TokenHash is the bcrypt hash of the full token. Store-facing only.
	TokenHash string
	Username  string
	Label     string
	// CreatedAt is unix seconds.
	CreatedAt uint64
	// ExpiresAt is unix seconds after which the token no longer
	// authenticates.
	ExpiresAt  uint64
	Revoked    bool
	LastUsedAt *uint64
}

// MarshalJSON always fails: the Rust reference makes ApiTokenRecord
// deliberately non-Serialize (a compile error to marshal it), since it
// carries the bcrypt token hash. Go has no such compile-time guard, so
// this is the runtime equivalent — callers must go through View().
func (r ApiTokenRecord) MarshalJSON() ([]byte, error) {
	return nil, errors.New("core: ApiTokenRecord must never be marshaled — use View()")
}

// ApiTokenView is the public projection of an API token — no hash, no
// plaintext.
type ApiTokenView struct {
	Prefix     string  `json:"prefix"`
	Username   string  `json:"username"`
	Label      string  `json:"label"`
	CreatedAt  uint64  `json:"created_at"`
	ExpiresAt  uint64  `json:"expires_at"`
	Revoked    bool    `json:"revoked"`
	LastUsedAt *uint64 `json:"last_used_at"`
}

// View projects an ApiTokenRecord to its wire-safe ApiTokenView.
func (r *ApiTokenRecord) View() ApiTokenView {
	return ApiTokenView{
		Prefix:     r.Prefix,
		Username:   r.Username,
		Label:      r.Label,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		Revoked:    r.Revoked,
		LastUsedAt: r.LastUsedAt,
	}
}
