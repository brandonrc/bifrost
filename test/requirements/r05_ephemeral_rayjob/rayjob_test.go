// Requirement 5 — the UI runs jobs on an ephemeral RayJob: a submit creates
// a cluster for the job and removes it when the job finishes. The contract
// has list_jobs only; there is no submit operation yet.
package r05_ephemeral_rayjob

import (
	"context"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestSubmitCreatesAnEphemeralCluster(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 5, "POST /api/v1/jobs creates an ephemeral cluster for the job and removes it when the job finishes", func(b *req.B) {
		st, body := fixture.Do(b, tgt, adminBearer(b, tgt), http.MethodPost, "/api/v1/jobs",
			`{"project":"team-a","entrypoint":"python -c 1","image":"rayproject/ray:2.56.0"}`)
		if st != http.StatusCreated && st != http.StatusAccepted {
			b.Fatalf("submit = %d %s (the contract has no job submission operation)", st, body)
		}
	})
}

func adminBearer(b *req.B, tgt req.Target) string {
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tgt.BaseURL(), nil)
	if err != nil {
		b.Fatal(err)
	}
	tgt.As("admin").Authorize(r)
	return r.Header.Get("Authorization")[len("Bearer "):]
}
