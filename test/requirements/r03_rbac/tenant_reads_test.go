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

// The red-team finding, at L3: a GLOBAL role with zero project ties does
// not cross tenant boundaries on the read surface. The seeded "operator"
// principal holds a global operator role and no project-scoped
// assignments; a team-a cluster owned by dev-a must be invisible to them:
// absent from their list (filtering, never a 403 that leaks the list
// exists) and 404 — never 403 — by name, so the read leaks nothing about
// the cluster's existence. Admin still sees it (control row).
func TestGlobalRoleWithoutProjectTieCannotReadForeignCluster(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a global role with no project ties reads only what it owns: foreign clusters are filtered from the list and 404 by name, while Admin still reads")
	ctx := context.Background()
	id := req.Name("iso")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")

	// List: 200 with the foreign cluster filtered out (never a denial —
	// lists filter).
	list, err := tgt.As("operator").API().ListClustersWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("operator list: err=%v status=%v", err, codeOf(list))
	}
	if containsID(fixture.IDs(list.Body), id) {
		t.Errorf("operator's list shows dev-a's cluster %s: %s", id, list.Body)
	}
	// Control: admin sees it.
	alist, err := tgt.As("admin").API().ListClustersWithResponse(ctx)
	if err != nil || alist.StatusCode() != http.StatusOK || !containsID(fixture.IDs(alist.Body), id) {
		t.Fatalf("admin list: err=%v status=%v body=%s, want %s present", err, codeOf(alist), alist.Body, id)
	}

	// By name: 404, not 403 — the read must not leak existence.
	if st, _ := fixture.Get(t, tgt, "operator", id); st != http.StatusNotFound {
		t.Errorf("operator get dev-a's cluster = %d, want 404 (never 403: no existence leak)", st)
	}
	if st, _ := fixture.Get(t, tgt, "admin", id); st != http.StatusOK {
		t.Errorf("admin get = %d, want 200", st)
	}
}

// The other half of the finding, at L3: through the cluster's gateway
// host the same global-role-no-project principal is refused with 403 (the
// mutation convention gateway denials already used) — the anonymous 401
// and member-403 halves live in TestHeadJobsApiOnlyThroughGateway. The
// principal here is a freshly minted LOCAL developer holding no
// assignments at all: the exact "developer with zero project memberships"
// shape the red team exercised.
func TestGatewayDeveloperWithNoProjectTiesIsRefused(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a developer with zero project memberships gets 403 on another tenant's cluster gateway host — the Target::Job verb check alone is not sufficient")
	req.NeedsCapability(t, tgt, "gateway")
	ctx := context.Background()
	user := req.Name("gwdev")
	pw := "pw-" + req.RunID() + "-gwdev"
	var create client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":"developer"}`, user, pw)), &create)
	cr, err := tgt.As("admin").API().CreateUserWithResponse(ctx, create)
	if err != nil || cr.StatusCode()/100 != 2 {
		t.Fatalf("create developer: err=%v status=%v body=%s", err, codeOf(cr), cr.Body)
	}
	t.Cleanup(func() {
		var upd client.UpdateUserJSONRequestBody
		_ = json.Unmarshal([]byte(`{"disabled":true}`), &upd)
		_, _ = tgt.As("admin").API().UpdateUserWithResponse(context.Background(), user, upd)
	})

	id := req.Name("gwx")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	var host string
	req.Eventually(t, tgt, func() (bool, string) {
		_, v := fixture.Get(t, tgt, "dev-a", id)
		host = fixture.GatewayHost(v)
		return host != "", "gateway_url not set"
	})

	// The foreign developer may not even READ the jobs surface, and must
	// never reach the cluster: 403, matching the member-denial convention.
	// (GatewayRequest is principal-based and seeded-principals-only, so the
	// freshly minted user's token goes on the request directly.)
	tok, st := fixture.Login(t, tgt, user, pw)
	if tok == "" {
		t.Fatalf("developer login = %d", st)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+"/api/jobs/", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := fixture.HTTPClient(tgt).Do(r)
	if err != nil {
		t.Fatalf("gateway GET %s/api/jobs/: %v", host, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("developer with no project ties via gateway = %d, want 403", resp.StatusCode)
	}
}

// containsID reports whether xs holds x — a local helper (the r06
// package's contains is not shared across requirement packages).
func containsID(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
