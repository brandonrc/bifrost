package auth

import (
	"encoding/json"
	"sort"
	"testing"
)

// Port of mobula-auth/src/lib.rs #[cfg(test)] mod tests (lines 707-1064).

func testMappings() RoleMappings {
	return RoleMappings{
		Admin:     []string{"/platform-admins"},
		Operator:  []string{"/sre"},
		Developer: []string{"/ml-eng", "/data-sci"},
		Viewer:    []string{"*"},
		Auditor:   []string{"/compliance"},
	}
}

// Port of role_permission_sets_by_target (lib.rs:721-770).
func TestRolePermissionSetsByTarget(t *testing.T) {
	// Admin: everything.
	if !RoleAdmin.Grants(Admin, TargetCluster) || !RoleAdmin.Grants(Write, TargetJob) {
		t.Fatal("admin should grant everything")
	}
	if !RoleAdmin.Grants(Read, TargetAudit) {
		t.Fatal("admin should read audit")
	}
	// Developer: job code yes, cluster lifecycle no.
	if !RoleDeveloper.Grants(Write, TargetJob) {
		t.Fatal("developer should write jobs")
	}
	if RoleDeveloper.Grants(Write, TargetCluster) {
		t.Fatal("developer should not write clusters")
	}
	if !RoleDeveloper.Grants(Read, TargetCluster) {
		t.Fatal("developer should read clusters")
	}
	// Operator: cluster lifecycle yes, job/service code no.
	if !RoleOperator.Grants(Write, TargetCluster) {
		t.Fatal("operator should write clusters")
	}
	if RoleOperator.Grants(Write, TargetJob) {
		t.Fatal("operator should not write jobs")
	}
	if !RoleOperator.Grants(Read, TargetJob) {
		t.Fatal("operator should read jobs")
	}
	// Services are code: Developer deploys, Operator read-only.
	if !RoleDeveloper.Grants(Write, TargetService) {
		t.Fatal("developer should write services")
	}
	if RoleOperator.Grants(Write, TargetService) {
		t.Fatal("operator should not write services")
	}
	// Pools are platform config: everyone reads, only Admin mutates.
	if RoleDeveloper.Grants(Write, TargetPool) {
		t.Fatal("developer should not write pools")
	}
	if !RoleDeveloper.Grants(Read, TargetPool) {
		t.Fatal("developer should read pools")
	}
	if !RoleOperator.Grants(Read, TargetPool) {
		t.Fatal("operator should read pools")
	}
	if RoleOperator.Grants(Write, TargetPool) {
		t.Fatal("operator should not write pools")
	}
	if RoleOperator.Grants(Delete, TargetPool) {
		t.Fatal("operator should not delete pools")
	}
	if !RoleAdmin.Grants(Write, TargetPool) || !RoleAdmin.Grants(Delete, TargetPool) {
		t.Fatal("admin should write and delete pools")
	}
	if !RoleViewer.Grants(Read, TargetPool) {
		t.Fatal("viewer should read pools")
	}
	if RoleViewer.Grants(Write, TargetPool) {
		t.Fatal("viewer should not write pools")
	}
	// Viewer: read-only everywhere EXCEPT the audit trail.
	if !RoleViewer.Grants(Read, TargetCluster) {
		t.Fatal("viewer should read clusters")
	}
	if RoleViewer.Grants(Write, TargetJob) {
		t.Fatal("viewer should not write jobs")
	}
	if RoleViewer.Grants(Read, TargetAudit) {
		t.Fatal("viewer should not read audit")
	}
	if RoleDeveloper.Grants(Read, TargetAudit) {
		t.Fatal("developer should not read audit")
	}
	if RoleOperator.Grants(Read, TargetAudit) {
		t.Fatal("operator should not read audit")
	}
	// Auditor: reads the audit surface and NOTHING else.
	if !RoleAuditor.Grants(Read, TargetAudit) {
		t.Fatal("auditor should read audit")
	}
	if RoleAuditor.Grants(Write, TargetAudit) {
		t.Fatal("auditor should not write audit")
	}
	if RoleAuditor.Grants(Delete, TargetAudit) {
		t.Fatal("auditor should not delete audit")
	}
	if RoleAuditor.Grants(Admin, TargetAudit) {
		t.Fatal("auditor should not admin audit")
	}
	if RoleAuditor.Grants(Write, TargetCluster) {
		t.Fatal("auditor should not write clusters")
	}
	if RoleAuditor.Grants(Read, TargetCluster) {
		t.Fatal("auditor should not read clusters")
	}
	if RoleAuditor.Grants(Admin, TargetCluster) {
		t.Fatal("auditor should not admin clusters")
	}
	if RoleAuditor.Grants(Read, TargetJob) {
		t.Fatal("auditor should not read jobs")
	}
	if RoleAuditor.Grants(Read, TargetService) {
		t.Fatal("auditor should not read services")
	}
	if RoleAuditor.Grants(Read, TargetPool) {
		t.Fatal("auditor should not read pools")
	}
}

// Port of identity_permits_is_union_of_roles (lib.rs:772-798).
func TestIdentityPermitsIsUnionOfRoles(t *testing.T) {
	id := Identity{
		Subject: "u",
		Roles:   []Role{RoleDeveloper, RoleOperator},
	}
	if !id.Permits(Write, TargetJob) {
		t.Fatal("expected write job")
	}
	if !id.Permits(Write, TargetCluster) {
		t.Fatal("expected write cluster")
	}
	if !id.IsAuthorized() {
		t.Fatal("expected authorized")
	}

	none := Identity{Subject: "u"}
	if none.IsAuthorized() {
		t.Fatal("expected unauthorized")
	}
	if none.Permits(Read, TargetJob) {
		t.Fatal("expected no permits")
	}
}

// Port of resolve_returns_all_matching_roles (lib.rs:800-817).
func TestResolveReturnsAllMatchingRoles(t *testing.T) {
	m := testMappings()
	r := m.Resolve([]string{"/ml-eng", "/platform-admins"})
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	if !containsRole(r, RoleAdmin) || !containsRole(r, RoleDeveloper) {
		t.Fatalf("expected admin+developer, got %v", r)
	}
	if !containsRole(m.Resolve([]string{"/random"}), RoleViewer) {
		t.Fatal("wildcard viewer should match any group")
	}
	if !containsRole(m.Resolve(nil), RoleViewer) {
		t.Fatal("wildcard viewer should match no groups")
	}
	if !containsRole(m.Resolve([]string{"/sre"}), RoleOperator) {
		t.Fatal("expected operator")
	}
	r2 := m.Resolve([]string{"/compliance"})
	if !containsRole(r2, RoleAuditor) {
		t.Fatalf("expected auditor, got %v", r2)
	}
}

// Port of wildcard_detection (lib.rs:819-827).
func TestWildcardDetection(t *testing.T) {
	m := testMappings()
	if !m.HasWildcard() {
		t.Fatal("expected wildcard")
	}
	only := RoleMappings{Developer: []string{"/ml-eng"}}
	if only.HasWildcard() {
		t.Fatal("expected no wildcard")
	}
}

// Port of role_wire_names_round_trip (lib.rs:829-841).
func TestRoleWireNamesRoundTrip(t *testing.T) {
	for _, r := range []Role{RoleViewer, RoleDeveloper, RoleOperator, RoleAdmin, RoleAuditor} {
		got, ok := ParseRole(r.AsStr())
		if !ok || got != r {
			t.Fatalf("round trip failed for %v", r)
		}
	}
	if _, ok := ParseRole("superuser"); ok {
		t.Fatal("expected parse failure for unknown role")
	}
}

// TestRoleUnmarshalJSONRoundTrip covers the ingress guard T11 added
// (Role.UnmarshalJSON) that TestRoleWireNamesRoundTrip above never
// exercised directly (it only round-trips through AsStr/ParseRole, not
// through encoding/json): a Role decodes correctly from its wire string,
// and an unrecognized value fails closed rather than defaulting to
// RoleViewer's zero value (deploy-time T12 review follow-up).
func TestRoleUnmarshalJSONRoundTrip(t *testing.T) {
	var r Role
	if err := json.Unmarshal([]byte(`"operator"`), &r); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if r != RoleOperator {
		t.Fatalf("role = %v, want RoleOperator", r)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(encoded) != `"operator"` {
		t.Fatalf("encoded = %s, want %q", encoded, `"operator"`)
	}

	var bad Role
	if err := json.Unmarshal([]byte(`"superuser"`), &bad); err == nil {
		t.Fatal("expected an error for an unrecognized role string (fail closed, not RoleViewer's zero value)")
	}
}

// TestRoleScopeUnmarshalJSONRoundTrip pins down that RoleScope needs no
// UnmarshalJSON of its own (T11's report flagged this as unverified):
// encoding/json already dispatches to the embedded Role field's
// UnmarshalJSON, so an invalid role string inside a RoleScope fails
// closed exactly like a bare Role does.
func TestRoleScopeUnmarshalJSONRoundTrip(t *testing.T) {
	var rs RoleScope
	if err := json.Unmarshal([]byte(`{"role":"admin","scope":"project:demo"}`), &rs); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if rs != (RoleScope{Role: RoleAdmin, Scope: "project:demo"}) {
		t.Fatalf("role scope = %+v, want {admin project:demo}", rs)
	}
	encoded, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var rs2 RoleScope
	if err := json.Unmarshal(encoded, &rs2); err != nil || rs2 != rs {
		t.Fatalf("round trip failed: encoded=%s err=%v got=%+v", encoded, err, rs2)
	}

	var bad RoleScope
	if err := json.Unmarshal([]byte(`{"role":"superuser","scope":"*"}`), &bad); err == nil {
		t.Fatal("expected an error for an unrecognized role string embedded in a RoleScope")
	}
}

// Nit (fix round 1): pin ParseRole's zero-value-on-failure behavior so a
// caller that (incorrectly) drops ok can't regress this silently. Unlike
// Rust's Option<Role>, an unchecked ParseRole result is RoleViewer's zero
// value, not a safe "no role" sentinel — see the ParseRole doc comment.
func TestParseRoleGarbageIsZeroValueNotOk(t *testing.T) {
	role, ok := ParseRole("garbage")
	if ok {
		t.Fatal("expected ok=false for an unrecognized role string")
	}
	if role != 0 {
		t.Fatalf("expected the zero Role value on failure, got %v", role)
	}
	if role != RoleViewer {
		t.Fatal("sanity: Role's zero value is RoleViewer — the whole point of this test")
	}
}

// Nit (fix round 1): AsStr() on an out-of-range Role value must return an
// honest sentinel, not a value that looks like it could be a real role.
func TestRoleAsStrOutOfRangeIsHonestSentinel(t *testing.T) {
	got := Role(99).AsStr()
	if got == "role" || got == "" {
		t.Fatalf("expected an honest out-of-range sentinel, got %q", got)
	}
	want := "invalid-role(99)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Port of scope_grammar_is_star_or_project_name (lib.rs:843-859).
func TestScopeGrammarIsStarOrProjectName(t *testing.T) {
	if !ValidScope("*") {
		t.Fatal("expected * to be valid")
	}
	if !ValidScope("project:ml-team") {
		t.Fatal("expected project:ml-team to be valid")
	}
	if !ValidScope("project:team/sub_ns.1") {
		t.Fatal("expected project:team/sub_ns.1 to be valid")
	}
	for _, bad := range []string{
		"", "project:", "cluster:c1", "project:has space", "project:semi;colon", "**", "*x",
	} {
		if ValidScope(bad) {
			t.Fatalf("expected %q to be invalid", bad)
		}
	}
}

// Port of permits_scoped_matrix (lib.rs:861-928).
func TestPermitsScopedMatrix(t *testing.T) {
	dev := Identity{Subject: "u", Roles: []Role{RoleDeveloper}}
	opML := []RoleScope{{RoleOperator, "project:ml-team"}}

	admin := Identity{Subject: "root", Roles: []Role{RoleAdmin}}
	if !admin.PermitsScoped(Delete, TargetCluster, nil, "anywhere") {
		t.Fatal("admin fast path should cover everything")
	}

	if !dev.PermitsScoped(Read, TargetCluster, opML, "ml-team") {
		t.Fatal("expected scoped read")
	}
	if !dev.PermitsScoped(Write, TargetCluster, opML, "ml-team") {
		t.Fatal("expected scoped write")
	}
	if !dev.PermitsScoped(Delete, TargetCluster, opML, "ml-team") {
		t.Fatal("expected scoped delete")
	}
	if dev.PermitsScoped(Write, TargetCluster, opML, "other") {
		t.Fatal("scoped grant must not leak to other project")
	}

	nobody := Identity{Subject: "n"}
	if nobody.PermitsScoped(Write, TargetJob, opML, "ml-team") {
		t.Fatal("operator-on-ml-team must not grant job writes")
	}
	if dev.Permits(Write, TargetCluster) {
		t.Fatal("non-scoped fast path must be unchanged")
	}

	opStar := []RoleScope{{RoleOperator, "*"}}
	if !dev.PermitsScoped(Write, TargetCluster, opStar, "other") {
		t.Fatal("a * assignment should be a global grant")
	}

	if dev.PermitsScoped(Write, TargetCluster, nil, "ml-team") {
		t.Fatal("no assignments should mean exactly the flat mapping")
	}
	if nobody.PermitsScoped(Read, TargetCluster, nil, "ml-team") {
		t.Fatal("nobody with no assignments should have no access")
	}
	if !nobody.PermitsScoped(Read, TargetCluster, opML, "ml-team") {
		t.Fatal("nobody with matching assignment should have access")
	}

	op := Identity{Subject: "o", Roles: []Role{RoleOperator}}
	scopedViewer := []RoleScope{{RoleViewer, "project:ml-team"}}
	if !op.PermitsScoped(Write, TargetCluster, scopedViewer, "ml-team") {
		t.Fatal("assignments are additive only — scoped viewer must not strip global operator write")
	}
}

// Port of sanitize_strips_control_chars (lib.rs:930-934).
func TestSanitizeStripsControlChars(t *testing.T) {
	if got := sanitizeClaim("normal-sub"); got != "normal-sub" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeClaim("evil\nsub\r\tx"); got != "evil?sub??x" {
		t.Fatalf("got %q", got)
	}
}

// Port of no_wildcard_means_deny_by_default (lib.rs:936-943).
func TestNoWildcardMeansDenyByDefault(t *testing.T) {
	m := RoleMappings{Viewer: []string{"/readers"}}
	if len(m.Resolve([]string{"/unrelated"})) != 0 {
		t.Fatal("expected deny by default")
	}
}

// Port of project_roles_derive_scoped_grant_per_group (lib.rs:945-957).
func TestProjectRolesDeriveScopedGrantPerGroup(t *testing.T) {
	m := ProjectRoleMappings{Operator: []string{"*"}}
	grants := m.Resolve([]string{"team-a", "team-b"})
	if !containsRoleScope(grants, RoleScope{RoleOperator, "project:team-a"}) {
		t.Fatalf("expected team-a grant, got %v", grants)
	}
	if !containsRoleScope(grants, RoleScope{RoleOperator, "project:team-b"}) {
		t.Fatalf("expected team-b grant, got %v", grants)
	}
	if len(m.Resolve(nil)) != 0 {
		t.Fatal("expected no groups -> no grants")
	}
}

// Port of project_roles_explicit_group_list_scopes_to_that_group (lib.rs:960-976).
func TestProjectRolesExplicitGroupListScopesToThatGroup(t *testing.T) {
	m := ProjectRoleMappings{
		Operator:  []string{"team-a"},
		Developer: []string{"team-b"},
	}
	a := m.Resolve([]string{"team-a"})
	want := []RoleScope{{RoleOperator, "project:team-a"}}
	if !equalRoleScopes(a, want) {
		t.Fatalf("got %v, want %v", a, want)
	}
	b := m.Resolve([]string{"team-b"})
	wantB := []RoleScope{{RoleDeveloper, "project:team-b"}}
	if !equalRoleScopes(b, wantB) {
		t.Fatalf("got %v, want %v", b, wantB)
	}
	if len(m.Resolve([]string{"team-c"})) != 0 {
		t.Fatal("expected unmapped group to resolve to nothing")
	}
}

// Port of project_roles_strip_prefix_and_grammar (lib.rs:979-996).
func TestProjectRolesStripPrefixAndGrammar(t *testing.T) {
	m := ProjectRoleMappings{Operator: []string{"*"}, StripPrefix: "/"}
	g := m.Resolve([]string{"/team-a"})
	want := []RoleScope{{RoleOperator, "project:team-a"}}
	if !equalRoleScopes(g, want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	if len(m.Resolve([]string{"/"})) != 0 {
		t.Fatal("expected empty project name to be skipped")
	}
	bad := ProjectRoleMappings{Operator: []string{"*"}}
	if len(bad.Resolve([]string{"has space"})) != 0 {
		t.Fatal("expected invalid scope grammar to be skipped")
	}
}

// Port of project_roles_empty_and_wildcard_flags (lib.rs:998-1014).
func TestProjectRolesEmptyAndWildcardFlags(t *testing.T) {
	var d ProjectRoleMappings
	if !d.IsEmpty() {
		t.Fatal("expected empty")
	}
	if d.HasWildcard() {
		t.Fatal("expected no wildcard")
	}
	m := ProjectRoleMappings{Operator: []string{"*"}}
	if m.IsEmpty() {
		t.Fatal("expected non-empty")
	}
	if !m.HasWildcard() {
		t.Fatal("expected wildcard")
	}
	explicit := ProjectRoleMappings{Operator: []string{"team-a"}}
	if explicit.IsEmpty() {
		t.Fatal("expected non-empty")
	}
	if explicit.HasWildcard() {
		t.Fatal("expected no wildcard")
	}
}

// Port of auth_config_parses_with_defaults (lib.rs:1016-1030).
func TestAuthConfigParsesWithDefaults(t *testing.T) {
	var cfg AuthConfig
	raw := []byte(`{
		"issuer": "https://kc.example.com/realms/nebari",
		"audience": "bifrost",
		"roles": {"developer": ["/ml-eng"]}
	}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GroupsClaim != "groups" {
		t.Fatalf("got %q", cfg.GroupsClaim)
	}
	if len(cfg.Roles.Admin) != 0 {
		t.Fatal("expected empty admin mapping")
	}
	if !cfg.ProjectRoles.IsEmpty() {
		t.Fatal("expected empty project_roles by default")
	}
}

// Port of auth_config_parses_project_roles (lib.rs:1032-1048).
func TestAuthConfigParsesProjectRoles(t *testing.T) {
	var cfg AuthConfig
	raw := []byte(`{
		"issuer": "https://kc.example.com/realms/nebari",
		"audience": "bifrost",
		"roles": {"viewer": ["*"]},
		"project_roles": {"operator": ["*"], "strip_prefix": "/"}
	}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoles.IsEmpty() {
		t.Fatal("expected non-empty project_roles")
	}
	if cfg.ProjectRoles.StripPrefix != "/" {
		t.Fatalf("got %q", cfg.ProjectRoles.StripPrefix)
	}
	want := []RoleScope{{RoleOperator, "project:team-a"}}
	got := cfg.ProjectRoles.Resolve([]string{"/team-a"})
	if !equalRoleScopes(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Fix round 1, #5 — marshal guards. The report's original Rust analogy was
// inverted: Rust's "no Serialize derive" on Identity/Role/PermissionType/
// Target is a COMPILE error at every marshal site (mobula-auth/src/lib.rs
// never derives Serialize for any of them); Go has no such compile-time
// guard, so an unguarded Identity/Role/etc. would marshal SILENTLY —
// Identity as a raw struct dump (including Email, and via ProjectRoles the
// caller's full scoped-grant set) and Role/PermissionType/Target as bare
// ordinals ("operator" -> 2). internal/core/auth.go:103-109 documents the
// convention this follows: credential/identity-bearing types either refuse
// to marshal outright, or marshal through an explicit wire-safe form.

// Identity must never marshal directly.
func TestIdentityMarshalJSONAlwaysFails(t *testing.T) {
	id := Identity{Subject: "u", Email: strPtr("u@example.com")}
	if _, err := json.Marshal(id); err == nil {
		t.Fatal("expected Identity to refuse to marshal")
	}
	// Also via a pointer receiver call site (json.Marshal(&id)).
	if _, err := json.Marshal(&id); err == nil {
		t.Fatal("expected *Identity to refuse to marshal")
	}
}

// Role, PermissionType, and Target must marshal as their AsStr() string,
// never as the bare underlying int — a handler emitting a Role in a
// response body must produce "operator", not 2.
func TestEnumsMarshalAsWireStrings(t *testing.T) {
	if got, err := json.Marshal(RoleOperator); err != nil || string(got) != `"operator"` {
		t.Fatalf("Role: got %s, err %v", got, err)
	}
	if got, err := json.Marshal(Write); err != nil || string(got) != `"write"` {
		t.Fatalf("PermissionType: got %s, err %v", got, err)
	}
	if got, err := json.Marshal(TargetCluster); err != nil || string(got) != `"cluster"` {
		t.Fatalf("Target: got %s, err %v", got, err)
	}
}

// RoleScope gets snake_case json tags (it's the wire shape for
// project_roles entries and scoped assignments).
func TestRoleScopeSnakeCaseTags(t *testing.T) {
	rs := RoleScope{Role: RoleOperator, Scope: "project:team-a"}
	got, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"operator","scope":"project:team-a"}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func strPtr(s string) *string { return &s }

func containsRole(roles []Role, r Role) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

func containsRoleScope(rs []RoleScope, want RoleScope) bool {
	for _, x := range rs {
		if x == want {
			return true
		}
	}
	return false
}

func equalRoleScopes(a, b []RoleScope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
