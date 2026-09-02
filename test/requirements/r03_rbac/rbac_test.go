// Requirement 3 — RBAC for cluster access; direct Ray access blocked.
package r03_rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// The permission model (internal/auth/rbac.go, ported from mobula): a
// developer is read-only on clusters; cluster lifecycle needs operator,
// globally or on the project. Found the hard way on grace 2026-09-02 when
// the first e2e pass seeded developers and every create was 403.
func TestDeveloperCannotCreateCluster(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a developer with no operator grant may list clusters but not create one")
	ctx := context.Background()
	user := req.Name("dev")
	pw := "pw-" + req.RunID() + "-developer"

	var create client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":"developer"}`, user, pw)), &create)
	cr, err := tgt.As("admin").API().CreateUserWithResponse(ctx, create)
	if err != nil || cr.StatusCode()/100 != 2 {
		t.Fatalf("create developer: err=%v status=%v body=%s", err, cr.StatusCode(), cr.Body)
	}
	t.Cleanup(func() {
		var upd client.UpdateUserJSONRequestBody
		_ = json.Unmarshal([]byte(`{"disabled":true}`), &upd)
		_, _ = tgt.As("admin").API().UpdateUserWithResponse(context.Background(), user, upd)
	})
	tok, st := fixture.Login(t, tgt, user, pw)
	if tok == "" {
		t.Fatalf("developer login = %d", st)
	}
	if st, _ := fixture.Do(t, tgt, tok, http.MethodGet, "/api/v1/clusters", ""); st != http.StatusOK {
		t.Errorf("developer list_clusters = %d, want 200 (developers read clusters)", st)
	}
	body, _ := json.Marshal(fixture.ClusterBody(req.Name("devc"), "team-a", nil))
	st, b := fixture.Do(t, tgt, tok, http.MethodPost, "/api/v1/clusters", string(body))
	if st != http.StatusForbidden {
		t.Fatalf("developer create_cluster = %d %s, want 403", st, b)
	}
}

func TestProjectOperatorCannotEscalateOrReadPlatformState(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a project operator cannot grant themselves admin, read or edit platform policy, list users, or read the audit trail")
	ctx := context.Background()
	me := fixture.Subject(t, tgt, "dev-a")
	api := tgt.As("dev-a").API()

	var esc client.UpsertAssignmentJSONRequestBody
	_ = json.Unmarshal([]byte(`{"role":"admin","scope":"*"}`), &esc)
	if r, err := api.UpsertAssignmentWithResponse(ctx, me, esc); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("self-escalation to global admin: err=%v status=%v, want 403", err, r.StatusCode())
	}
	if r, err := api.GetPolicyWithResponse(ctx); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("get_policy: err=%v status=%v, want 403", err, r.StatusCode())
	}
	var upd client.UpdatePolicyJSONRequestBody
	_ = json.Unmarshal([]byte(`{}`), &upd)
	if r, err := api.UpdatePolicyWithResponse(ctx, upd); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("update_policy: err=%v status=%v, want 403", err, r.StatusCode())
	}
	if r, err := api.ListUsersWithResponse(ctx); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("list_users: err=%v status=%v, want 403", err, r.StatusCode())
	}
	if r, err := api.ListAuditEventsWithResponse(ctx, nil); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Errorf("list_audit_events: err=%v status=%v, want 403", err, r.StatusCode())
	}
}

func TestCreateInAnotherProjectIsRefused(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a project operator's grant is scoped: creating in another project is 403")
	st, body := fixture.Create(t, tgt, "dev-a", req.Name("xp"), "team-b", nil)
	if st != http.StatusForbidden {
		t.Fatalf("dev-a create in team-b = %d %s, want 403", st, body)
	}
}
