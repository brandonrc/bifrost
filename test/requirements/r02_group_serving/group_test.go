// Requirement 2 — groups share models privately: one RayService per group,
// every request authenticated, the caller must belong to the owning group.
//
// The API half (project-scoped reads, one-service-per-project) is proved on
// L2 against the in-process target. The serve-path half (401/403 on the
// gateway host before anything is proxied) needs the gateway's `serve`
// authorization branch, which package B owns; that test is gated on the
// gateway-serve capability until B lands.
package r02_group_serving

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// A team-a service is invisible to team-b: get answers 404 (never 403 —
// a non-member must not even learn the name exists) and 200 to a member.
func TestNonMemberCannotReadAnotherGroupsService(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 2, "a service deployed by team-a is 403/404 to team-b and 200 to team-a")
	name := req.Name("gsvc")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy_service: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })

	if st, _ := fixture.GetService(t, tgt, "dev-b", name); !fixture.Denied(st) {
		t.Fatalf("dev-b get team-a's service = %d, want 403/404", st)
	}
	st, view := fixture.GetService(t, tgt, "dev-a", name)
	if st != http.StatusOK {
		t.Fatalf("dev-a get own service = %d, want 200", st)
	}
	if view["project"] != "team-a" {
		t.Errorf("project = %v, want team-a", view["project"])
	}
	list, err := tgt.As("dev-b").API().ListServicesWithResponse(t.Context())
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list_services as dev-b: err=%v status=%v", err, list.StatusCode())
	}
	for _, n := range serviceNames(t, list.Body) {
		if n == name {
			t.Fatalf("dev-b's list_services includes team-a's %s", name)
		}
	}
}

// One shared RayService per group (plan ruling D8): a second name in the
// same project is 409 until the first is deleted, then the redeploy is
// accepted. Deleting frees the slot as soon as the row is headed for
// termination, so a member need not wait for the tombstone.
func TestSecondServiceInSameProjectIsRefusedUntilFirstIsGone(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 2, "a project has one service: a second name is 409 until the first is deleted")
	first := req.Name("gsvc-one")
	second := req.Name("gsvc-two")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(first, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy first: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() {
		fixture.DeleteService(t, tgt, "dev-a", first)
		fixture.DeleteService(t, tgt, "dev-a", second)
	})

	st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(second, "team-a"))
	if st != http.StatusConflict {
		t.Fatalf("deploy second name in team-a: status=%d body=%s, want 409", st, raw)
	}
	if !strings.Contains(string(raw), first) {
		t.Errorf("409 body must name the service that holds the slot (%s): %s", first, raw)
	}
	if st, _ := fixture.GetService(t, tgt, "dev-a", second); st != http.StatusNotFound {
		t.Fatalf("refused service must not exist, get = %d", st)
	}

	if st := fixture.DeleteService(t, tgt, "dev-a", first); st != http.StatusAccepted {
		t.Fatalf("delete first: status=%d, want 202", st)
	}
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.GetService(t, tgt, "dev-a", first)
		if st == http.StatusNotFound {
			return true, "404"
		}
		state, _ := v["state"].(string)
		return state == "terminated" || state == "terminating", "state=" + state
	})
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(second, "team-a")); st != http.StatusAccepted {
		t.Fatalf("redeploy second after delete: status=%d body=%s, want 202", st, raw)
	}
	fixture.WaitService(t, tgt, "dev-a", second, "running")
}

// Redeploying under the same name is an update, never a conflict: 202
// again, and the project still has exactly one live entry in list_services.
func TestRedeploySameNameIsAnUpdate(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 2, "redeploying the group's service under its name updates it rather than tripping the one-per-group rule")
	name := req.Name("gsvc-upd")
	body := fixture.ServiceBody(name, "team-a")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", body); st != http.StatusAccepted {
		t.Fatalf("deploy: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })
	fixture.WaitService(t, tgt, "dev-a", name, "running")

	body.Spec.WorkerCpu = "2"
	if st, raw := fixture.Deploy(t, tgt, "dev-a", body); st != http.StatusAccepted {
		t.Fatalf("redeploy same name: status=%d body=%s, want 202", st, raw)
	}
	list, err := tgt.As("dev-a").API().ListServicesWithResponse(t.Context())
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list_services: err=%v status=%v", err, list.StatusCode())
	}
	var live []string
	for _, it := range serviceItems(t, list.Body) {
		if it.Project == "team-a" && it.State != "terminating" && it.State != "terminated" {
			live = append(live, it.Name)
		}
	}
	if len(live) != 1 || live[0] != name {
		t.Fatalf("team-a live services after redeploy = %v, want exactly [%s]", live, name)
	}
}

// Every serve request is authenticated and group-private: on the
// service's gateway host, anonymous is 401 and a non-member is 403 — both
// decided before anything is proxied, so L2 can prove them without a Serve
// backend. Gated on gateway-serve: the gateway's `serve` authorization
// branch (package B) is not on main yet, and until it is a member's
// request would be refused too. No target declares the capability yet.
func TestAnonymousServeRequestIs401AndNonMemberIs403(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 2, "on the service's gateway host an anonymous request is 401 and a non-member's is 403")
	req.NeedsCapability(t, tgt, "gateway-serve")
	name := req.Name("gsvc-gw")
	if st, raw := fixture.Deploy(t, tgt, "dev-a", fixture.ServiceBody(name, "team-a")); st != http.StatusAccepted {
		t.Fatalf("deploy: status=%d body=%s, want 202", st, raw)
	}
	t.Cleanup(func() { fixture.DeleteService(t, tgt, "dev-a", name) })
	view := fixture.WaitService(t, tgt, "dev-a", name, "running")
	gatewayURL, _ := view["gateway_url"].(string)
	if gatewayURL == "" {
		t.Fatalf("running service has no gateway_url: %v", view)
	}
	host := strings.TrimSuffix(strings.TrimPrefix(gatewayURL, "https://"), "/")

	do := func(principal string) int {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tgt.BaseURL()+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = host
		tgt.As(principal).Authorize(r)
		resp, err := fixture.HTTPClient(tgt).Do(r)
		if err != nil {
			t.Fatalf("GET / as %s via %s: %v", principal, host, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	if st := do("anon"); st != http.StatusUnauthorized {
		t.Errorf("anonymous GET / via gateway = %d, want 401", st)
	}
	if st := do("dev-b"); st != http.StatusForbidden {
		t.Errorf("dev-b (non-member) GET / via gateway = %d, want 403", st)
	}
	// A member gets past authorization; whether the proxied hop answers
	// 200 is requirement 1's serve-fixture test (L3).
	if st := do("dev-a"); st == http.StatusUnauthorized || st == http.StatusForbidden {
		t.Errorf("dev-a (member) GET / via gateway = %d, must not be refused", st)
	}
}

type serviceItem struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	State   string `json:"state"`
}

func serviceItems(t *testing.T, body []byte) []serviceItem {
	t.Helper()
	var items []serviceItem
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("list_services: unmarshal: %v", err)
	}
	return items
}

func serviceNames(t *testing.T, body []byte) []string {
	t.Helper()
	var names []string
	for _, it := range serviceItems(t, body) {
		names = append(names, it.Name)
	}
	return names
}
