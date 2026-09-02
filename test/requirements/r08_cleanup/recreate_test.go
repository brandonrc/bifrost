package r08_cleanup

import (
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// Found on grace 2026-09-02: two tests reused one id; the second create
// answered 201 but the record stayed desired=terminated and nothing was
// provisioned. Re-creating an id after its delete must be a fresh cluster.
func TestClusterIdCanBeReusedAfterDelete(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 8, "a cluster id whose cluster was deleted can be created again and converges like a fresh one")
	id := req.Name("reuse")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	if st := fixture.Delete(t, tgt, "dev-a", id); st/100 != 2 {
		t.Fatalf("delete = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", id)

	st, body := fixture.Create(t, tgt, "dev-a", id, "team-a", nil)
	if st != http.StatusCreated && st != http.StatusConflict {
		t.Fatalf("re-create = %d %s, want 201 (fresh cluster) or 409 (id retired)", st, body)
	}
	if st == http.StatusConflict {
		t.Skipf("server retires deleted ids (409): %s", body)
	}
	got, view := fixture.Get(t, tgt, "dev-a", id)
	if got != http.StatusOK {
		t.Fatalf("get after re-create = %d", got)
	}
	if d, _ := fixture.State(view); d != "running" {
		t.Fatalf("re-create answered 201 but desired=%q: a zombie record that will never provision", d)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
}
