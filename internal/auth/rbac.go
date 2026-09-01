// Package auth is Bifrost's OIDC/JWT identity and RBAC layer (ADR-0003 Phase
// 2 port of mobula-auth). Bifrost owns bearer-token validation: browser auth
// (NebariApp/SecurityPolicy style) and `ray job submit`-style Bearer clients
// both terminate here. Any compliant IdP works — the contract is OIDC
// discovery + JWKS + RS256.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// PermissionType is a permission verb, mirroring artifact-keeper's
// PermissionType (Read/Write/Delete/Admin) so Bifrost's RBAC vocabulary
// matches the rest of the ecosystem. Admin always wins.
//
// Reference: mobula-auth/src/lib.rs:16-25 (PermissionType).
type PermissionType int

const (
	Read PermissionType = iota
	Write
	Delete
	Admin
)

// AsStr returns the snake_case wire form. Rust's PermissionType has no
// Serialize derive (it's programmatic-only there); this exists so a Go
// handler that DOES embed a PermissionType in a response body emits
// "operator"-style text instead of a bare enum ordinal — see MarshalJSON.
func (p PermissionType) AsStr() string {
	switch p {
	case Read:
		return "read"
	case Write:
		return "write"
	case Delete:
		return "delete"
	case Admin:
		return "admin"
	}
	return fmt.Sprintf("invalid-permission(%d)", int(p))
}

// MarshalJSON emits the wire string form (AsStr), never the bare int, so a
// PermissionType embedded in a response body can't silently regress to
// "2" instead of "delete".
func (p PermissionType) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.AsStr())
}

// Target is what a permission applies to — artifact-keeper's target_type.
// This is what lets Operator (cluster lifecycle) differ from Developer (job
// code) with the same verbs.
//
// Reference: mobula-auth/src/lib.rs:27-48 (Target).
type Target int

const (
	// TargetJob is the proxied Ray job surface — submitting/stopping jobs is
	// "code".
	TargetJob Target = iota
	// TargetCluster is Bifrost's own cluster lifecycle
	// (create/suspend/terminate).
	TargetCluster
	// TargetService is Ray Serve services — deploying/updating a Serve app
	// is "code".
	TargetService
	// TargetPool is capacity pools and their allocations — platform
	// configuration, not app lifecycle, so mutations are Admin-only.
	TargetPool
	// TargetAudit is the persisted audit trail: its own target so
	// RoleAuditor can hold Read here and nothing anywhere else — separation
	// of duties: auditors inspect the trail without holding Admin.
	TargetAudit
)

// AsStr returns the snake_case wire form (see PermissionType.AsStr).
func (t Target) AsStr() string {
	switch t {
	case TargetJob:
		return "job"
	case TargetCluster:
		return "cluster"
	case TargetService:
		return "service"
	case TargetPool:
		return "pool"
	case TargetAudit:
		return "audit"
	}
	return fmt.Sprintf("invalid-target(%d)", int(t))
}

// MarshalJSON emits the wire string form (AsStr), never the bare int.
func (t Target) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.AsStr())
}

// Role is a built-in v0 role (ADR-0003). Roles are permission-sets over
// (verb, target), not an ordinal rank — RoleOperator (lifecycle but not
// code) overlaps RoleDeveloper without containing it, which a total order
// can't express.
//
// Reference: mobula-auth/src/lib.rs:50-126 (Role).
type Role int

const (
	RoleViewer Role = iota
	RoleDeveloper
	RoleOperator
	RoleAdmin
	// RoleAuditor is read-only on the audit surface and nothing else — a
	// compliance reader who must NOT get cluster/job/registry access.
	// Granted via the auditor group mapping; scoped role assignments don't
	// apply (the audit trail isn't project-scoped).
	RoleAuditor
)

// Grants reports whether this role grants action on target. A state-guard
// switch: every (Role, Target) pair is enumerated explicitly (no default),
// mirroring the exhaustive match in mobula-auth/src/lib.rs:70-101.
func (r Role) Grants(action PermissionType, target Target) bool {
	switch r {
	case RoleAdmin:
		// Admin: everything, everywhere.
		return true
	case RoleViewer:
		switch target {
		case TargetAudit:
			// Viewer: read-only, any target EXCEPT the audit trail — audit
			// subjects are Admin data, kept to Admin/Auditor only.
			return false
		case TargetJob, TargetCluster, TargetService, TargetPool:
			return action == Read
		}
	case RoleAuditor:
		switch target {
		case TargetAudit:
			// Auditor: reads the audit surface, nothing else — no cluster
			// reads, no registry, no writes anywhere.
			return action == Read
		case TargetJob, TargetCluster, TargetService, TargetPool:
			return false
		}
	case RoleDeveloper:
		switch target {
		case TargetJob, TargetService:
			// Developer: full job + service access (both are "code").
			return action == Read || action == Write || action == Delete
		case TargetCluster, TargetPool:
			// Read-only clusters and pools.
			return action == Read
		case TargetAudit:
			return false
		}
	case RoleOperator:
		switch target {
		case TargetCluster:
			// Operator: full cluster lifecycle.
			return action == Read || action == Write || action == Delete
		case TargetJob, TargetService, TargetPool:
			// Read-only code surfaces and pool topology (pools are platform
			// config — mutations are Admin-only).
			return action == Read
		case TargetAudit:
			return false
		}
	}
	return false
}

// AsStr returns the snake_case wire/storage form (role_assignments.role
// column, PUT /api/v1/access/assignments bodies).
func (r Role) AsStr() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleDeveloper:
		return "developer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	case RoleAuditor:
		return "auditor"
	}
	// Not a valid Role value at all (e.g. a raw int conversion from
	// outside this package) — an honest sentinel beats silently claiming
	// to be a real role.
	return fmt.Sprintf("invalid-role(%d)", int(r))
}

// MarshalJSON emits the wire string form (AsStr), never the bare int, so a
// Role embedded in a response body can't silently regress to "2" instead
// of "operator".
func (r Role) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.AsStr())
}

// UnmarshalJSON rejects any value other than a known Role wire string,
// mirroring serde's strict enum deserialization and this codebase's other
// strict-enum ingress guards (core.LocalRole.UnmarshalJSON's pattern,
// internal/core/auth.go:70). Added so a Role — and, transitively, a
// RoleScope embedding one — round-trips through JSON: Role already had
// MarshalJSON (the egress guard) but no UnmarshalJSON counterpart (the
// ingress guard) until now.
func (r *Role) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, ok := ParseRole(s)
	if !ok {
		return fmt.Errorf("auth: invalid Role %q", s)
	}
	*r = v
	return nil
}

// ParseRole is the inverse of AsStr; ok is false for anything else.
//
// Callers MUST check ok. Unlike Rust's Option<Role> (which makes an
// unchecked "unknown role" access a compile error), a caller that drops ok
// silently gets role's zero value — RoleViewer, not a safe "no role"
// sentinel — for any unrecognized string, including "" and garbage input.
func ParseRole(s string) (role Role, ok bool) {
	switch s {
	case "viewer":
		return RoleViewer, true
	case "developer":
		return RoleDeveloper, true
	case "operator":
		return RoleOperator, true
	case "admin":
		return RoleAdmin, true
	case "auditor":
		return RoleAuditor, true
	}
	return 0, false
}

// GlobalScope is the global assignment scope: an assignment at this scope
// applies to every target, exactly like a group-derived role.
const GlobalScope = "*"

// ValidScope is the scope grammar for role assignments: "*" (global) or
// "project:<name>". Cluster-scoped bindings ("cluster:<id>") and group
// principals are deferred — group bindings are the OIDC-mapping layer's job.
func ValidScope(scope string) bool {
	if scope == GlobalScope {
		return true
	}
	name, ok := strings.CutPrefix(scope, "project:")
	if !ok {
		return false
	}
	// Project names share the cluster-id grammar: a non-empty run of
	// lowercase alphanumerics, '-', '_', '.', '/' (NIC project slugs).
	if name == "" {
		return false
	}
	for _, c := range name {
		allowed := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '/'
		if !allowed {
			return false
		}
	}
	return true
}

// ScopeCovers reports whether an assignment scope covers project in a
// scoped check: "*" covers everything; "project:<name>" covers only that
// project.
func ScopeCovers(scope, project string) bool {
	return scope == GlobalScope || scope == "project:"+project
}

// RoleScope is a (role, scope) pair — a group-derived or explicitly
// assigned project-scoped grant. Go's named-struct equivalent of Rust's
// (Role, String) tuple.
type RoleScope struct {
	Role  Role   `json:"role"`
	Scope string `json:"scope"`
}

// Identity is an authenticated caller, attached to requests after
// validation. A caller may hold several roles (their union of permissions
// applies).
//
// Reference: mobula-auth/src/lib.rs:157-221 (Identity).
type Identity struct {
	Subject string
	// Username is the human username (preferred_username claim), when the
	// token carries one. Distinct from Subject, which is the opaque sub (a
	// UUID on Keycloak). Used as the tier-2 cluster owner label so it
	// matches the JupyterHub username the hub stamps on notebook pods.
	Username *string
	Email    *string
	Groups   []string
	Roles    []Role
	// ProjectRoles are group-derived project-scoped grants: (role,
	// "project:<name>") pairs implied by group membership via
	// [project_roles], with NO manual role_assignments row. Resolved once
	// at token validation from the caller's groups + the auth config, then
	// combined with any stored assignments at evaluation time (see
	// PermitsScoped). Empty for local auth and when [project_roles] is
	// unset — deny-by-default holds.
	ProjectRoles []RoleScope
}

// MarshalJSON always fails: Identity is a validated-request-scoped
// principal, not a wire type — it carries Email (and, via ProjectRoles,
// the caller's full scoped-grant set) with no `json` tags of its own and
// no vetted egress shape. The Rust reference never derives Serialize for
// Identity at all (a compile-time refusal); Go has no such compile-time
// guard, so this is the runtime equivalent — a handler that means to
// return identity information must build its own explicit response DTO
// (as internal/core's LocalUserRecord -> LocalUserView does), not
// serialize this type directly.
func (id Identity) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: Identity must never be marshaled directly — project to a wire-safe view")
}

// Owner is the identity to attribute owned resources to (tier-2 owned
// session clusters): the human Username when present, else Subject. For
// local auth Subject already IS the username, so the fallback is correct
// there too. This is the value stamped as bifrost.dev/owner and matched by
// the per-owner NetworkPolicy, so it must equal the JupyterHub username on
// the owner's notebook pod.
func (id *Identity) Owner() string {
	if id.Username != nil {
		return *id.Username
	}
	return id.Subject
}

// Permits reports whether any held role grants action on target
// (deny-by-default: an empty role set grants nothing).
func (id *Identity) Permits(action PermissionType, target Target) bool {
	for _, r := range id.Roles {
		if r.Grants(action, target) {
			return true
		}
	}
	return false
}

// PermitsScoped is the scoped check: the global fast path (Permits) OR any
// assignment in assignments whose scope covers project ("*" or
// "project:<project>") and whose role grants (action, target).
//
// Semantics are ADDITIVE ONLY: an assignment can grant, never subtract —
// there are no deny rules. A principal with no assignments gets exactly
// today's flat group->role mapping; one with assignments gets the union of
// group-derived roles and scope-matching assigned roles.
func (id *Identity) PermitsScoped(action PermissionType, target Target, assignments []RoleScope, project string) bool {
	if id.Permits(action, target) {
		return true
	}
	for _, a := range assignments {
		if ScopeCovers(a.Scope, project) && a.Role.Grants(action, target) {
			return true
		}
	}
	return false
}

// IsAuthorized reports whether id holds at least one role.
func (id *Identity) IsAuthorized() bool {
	return len(id.Roles) > 0
}

// RoleMappings is the mapping from IdP group names to Bifrost roles. "*"
// matches any authenticated caller (e.g. viewer = ["*"]).
//
// Reference: mobula-auth/src/lib.rs:225-239 (RoleMappings).
type RoleMappings struct {
	Admin     []string `json:"admin"`
	Operator  []string `json:"operator"`
	Developer []string `json:"developer"`
	Viewer    []string `json:"viewer"`
	// Auditor is the audit-trail readers (compliance) mapping.
	Auditor []string `json:"auditor"`
}

// roleMappingsAlias breaks the recursion MarshalJSON would otherwise cause.
type roleMappingsAlias RoleMappings

// MarshalJSON substitutes an empty slice for a nil group list, mirroring
// Rust's Vec::default(), which serde always writes as `[]`, never `null`.
func (m RoleMappings) MarshalJSON() ([]byte, error) {
	a := roleMappingsAlias(m)
	if a.Admin == nil {
		a.Admin = []string{}
	}
	if a.Operator == nil {
		a.Operator = []string{}
	}
	if a.Developer == nil {
		a.Developer = []string{}
	}
	if a.Viewer == nil {
		a.Viewer = []string{}
	}
	if a.Auditor == nil {
		a.Auditor = []string{}
	}
	return json.Marshal(a)
}

// HasWildcard reports whether any role maps a "*" wildcard.
func (m *RoleMappings) HasWildcard() bool {
	for _, patterns := range [][]string{m.Admin, m.Operator, m.Developer, m.Viewer, m.Auditor} {
		for _, p := range patterns {
			if p == "*" {
				return true
			}
		}
	}
	return false
}

// Clone returns a deep copy of m: the group-pattern slices get their own
// backing arrays, independent of m's. Required whenever a RoleMappings
// crosses out of the validator's control (e.g. Validator.RoleMappings()) —
// a plain struct copy shares the slice headers' backing arrays, so a
// caller mutating an element of the "copy" (or appending within its
// capacity) silently corrupts the live authz config, which can promote an
// otherwise-unmapped caller (e.g. writing "*" into a returned Viewer
// slice).
func (m *RoleMappings) Clone() RoleMappings {
	return RoleMappings{
		Admin:     append([]string(nil), m.Admin...),
		Operator:  append([]string(nil), m.Operator...),
		Developer: append([]string(nil), m.Developer...),
		Viewer:    append([]string(nil), m.Viewer...),
		Auditor:   append([]string(nil), m.Auditor...),
	}
}

// Resolve returns every role whose group mapping matches groups (a caller
// holds the union of their permissions). A "*" pattern matches any
// authenticated caller. Empty result = deny by default.
func (m *RoleMappings) Resolve(groups []string) []Role {
	matches := func(patterns []string) bool {
		for _, p := range patterns {
			if p == "*" {
				return true
			}
			for _, g := range groups {
				if g == p {
					return true
				}
			}
		}
		return false
	}
	var roles []Role
	if matches(m.Admin) {
		roles = append(roles, RoleAdmin)
	}
	if matches(m.Operator) {
		roles = append(roles, RoleOperator)
	}
	if matches(m.Developer) {
		roles = append(roles, RoleDeveloper)
	}
	if matches(m.Viewer) {
		roles = append(roles, RoleViewer)
	}
	if matches(m.Auditor) {
		roles = append(roles, RoleAuditor)
	}
	return roles
}

// ProjectRoleMappings is the group->project-role mapping: the self-service
// convention that lets a caller's IdP group membership imply a scoped role
// on the matching project with NO manual PUT /access/assignments. Same
// shape as RoleMappings (role -> group patterns), but a matching group
// grants the role scoped to "project:<group>" (the group name after
// StripPrefix), not globally. A "*" pattern means "every group the caller
// is in derives its own project role" — the plain "team-a => operator on
// project:team-a" rule. Empty lists = feature off, so deny-by-default is
// preserved.
//
// Admin/Operator/Developer are the meaningful entries (cluster lifecycle /
// job code on your own project); the rest exist for uniformity with
// RoleMappings.
//
// Reference: mobula-auth/src/lib.rs:261-353 (ProjectRoleMappings).
type ProjectRoleMappings struct {
	Admin     []string `json:"admin"`
	Operator  []string `json:"operator"`
	Developer []string `json:"developer"`
	Viewer    []string `json:"viewer"`
	Auditor   []string `json:"auditor"`
	// StripPrefix is stripped from each matching group name to derive the
	// project, e.g. "/" for Keycloak's /team-a groups (-> project:team-a).
	// Default "": the group name is the project name verbatim.
	StripPrefix string `json:"strip_prefix"`
}

// projectRoleMappingsAlias breaks the recursion MarshalJSON would
// otherwise cause.
type projectRoleMappingsAlias ProjectRoleMappings

// MarshalJSON substitutes an empty slice for a nil group list, mirroring
// Rust's Vec::default().
func (m ProjectRoleMappings) MarshalJSON() ([]byte, error) {
	a := projectRoleMappingsAlias(m)
	if a.Admin == nil {
		a.Admin = []string{}
	}
	if a.Operator == nil {
		a.Operator = []string{}
	}
	if a.Developer == nil {
		a.Developer = []string{}
	}
	if a.Viewer == nil {
		a.Viewer = []string{}
	}
	if a.Auditor == nil {
		a.Auditor = []string{}
	}
	return json.Marshal(a)
}

// IsEmpty reports whether any role has a group pattern configured (feature
// on).
func (m *ProjectRoleMappings) IsEmpty() bool {
	return len(m.Admin) == 0 && len(m.Operator) == 0 && len(m.Developer) == 0 &&
		len(m.Viewer) == 0 && len(m.Auditor) == 0
}

// HasWildcard reports whether any role maps a "*" wildcard — every group
// derives its own project role. Narrower than a global wildcard (grants
// are per-project), but still worth surfacing at boot.
func (m *ProjectRoleMappings) HasWildcard() bool {
	for _, patterns := range [][]string{m.Admin, m.Operator, m.Developer, m.Viewer, m.Auditor} {
		for _, p := range patterns {
			if p == "*" {
				return true
			}
		}
	}
	return false
}

// Resolve returns the group-derived (role, "project:<name>") grants for
// groups. For each role, a group matching its patterns ("*" matches any)
// yields that role scoped to the project named by the group (after
// StripPrefix). Groups that reduce to an empty or scope-grammar-invalid
// project name are skipped. Duplicates are de-duplicated; order is stable.
func (m *ProjectRoleMappings) Resolve(groups []string) []RoleScope {
	type roled struct {
		patterns []string
		role     Role
	}
	var out []RoleScope
	for _, rp := range []roled{
		{m.Admin, RoleAdmin},
		{m.Operator, RoleOperator},
		{m.Developer, RoleDeveloper},
		{m.Viewer, RoleViewer},
		{m.Auditor, RoleAuditor},
	} {
		if len(rp.patterns) == 0 {
			continue
		}
		for _, g := range groups {
			matched := false
			for _, p := range rp.patterns {
				if p == "*" || p == g {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			name := strings.TrimPrefix(g, m.StripPrefix)
			if name == "" {
				continue
			}
			scope := "project:" + name
			if !ValidScope(scope) {
				continue
			}
			entry := RoleScope{Role: rp.role, Scope: scope}
			dup := false
			for _, e := range out {
				if e == entry {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, entry)
			}
		}
	}
	return out
}

const defaultGroupsClaim = "groups"

// AuthConfig is the OIDC validator's static configuration.
//
// Reference: mobula-auth/src/lib.rs:241-259 (AuthConfig).
type AuthConfig struct {
	// Issuer is the OIDC issuer URL; {issuer}/.well-known/openid-configuration
	// must resolve. Trailing slash insignificant.
	Issuer string `json:"issuer"`
	// Audience is the required aud claim value.
	Audience string `json:"audience"`
	// GroupsClaim is the claim carrying group memberships (array of
	// strings, or one space-delimited string). Keycloak default: "groups".
	GroupsClaim string       `json:"groups_claim"`
	Roles       RoleMappings `json:"roles"`
	// ProjectRoles is the group->project-role automation. Zero value (empty
	// = off) so configs without the key keep exactly today's flat
	// behavior.
	ProjectRoles ProjectRoleMappings `json:"project_roles"`
}

// authConfigAlias breaks the recursion UnmarshalJSON would otherwise
// cause.
type authConfigAlias AuthConfig

// UnmarshalJSON defaults GroupsClaim to "groups" when the field is absent,
// mirroring serde's #[serde(default = "default_groups_claim")].
func (c *AuthConfig) UnmarshalJSON(data []byte) error {
	a := authConfigAlias{GroupsClaim: defaultGroupsClaim}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = AuthConfig(a)
	return nil
}

// sanitizeClaim replaces ASCII/Unicode control characters (newlines
// included) so a claim value cannot forge log lines when written to the
// plain-text layer.
func sanitizeClaim(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
