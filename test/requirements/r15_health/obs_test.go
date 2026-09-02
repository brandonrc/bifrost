// Requirement 15 — cluster health and pending reasons without direct
// Kubernetes access: the five observability operations.
package r15_health

import (
	"context"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

type obsOp struct {
	name string
	call func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error)
}

var obsOps = []obsOp{
	{"nodes", func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error) {
		r, err := api.ClusterNodesWithResponse(ctx, id)
		return code(r, err), body(r), err
	}},
	{"events", func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error) {
		r, err := api.ClusterEventsWithResponse(ctx, id)
		return code(r, err), body(r), err
	}},
	{"metrics", func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error) {
		r, err := api.ClusterMetricsWithResponse(ctx, id)
		return code(r, err), body(r), err
	}},
	{"logs", func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error) {
		r, err := api.ClusterLogsWithResponse(ctx, id, nil)
		return code(r, err), body(r), err
	}},
	{"jobs", func(ctx context.Context, api *client.ClientWithResponses, id string) (int, []byte, error) {
		r, err := api.ClusterJobsWithResponse(ctx, id)
		return code(r, err), body(r), err
	}},
}

type resp interface {
	StatusCode() int
}

func code(r resp, err error) int {
	if err != nil || r == nil {
		return 0
	}
	return r.StatusCode()
}

func body(r any) []byte {
	if b, ok := r.(interface{ GetBody() []byte }); ok {
		return b.GetBody()
	}
	return nil
}

// The owner sees their cluster's nodes, events, metrics, logs and jobs; a
// member of another project sees none of them. The 200 half needs a real
// head to observe and is asserted on Kubernetes targets; the denial half is
// authorization and holds everywhere.
func TestObservabilityOpsOwnVsOther(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 15, "the five observability operations answer the owner and refuse another project")
	ctx := context.Background()
	id := req.Name("obs")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	_, k8s := tgt.K8s()
	if k8s {
		fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	}
	for _, op := range obsOps {
		st, _, err := op.call(ctx, tgt.As("dev-b").API(), id)
		if err != nil || !fixture.Denied(st) {
			t.Errorf("%s as dev-b: err=%v status=%d, want 403 or 404", op.name, err, st)
		}
		if k8s {
			st, b, err := op.call(ctx, tgt.As("dev-a").API(), id)
			if err != nil || st != http.StatusOK {
				t.Errorf("%s as owner: err=%v status=%d body=%s, want 200", op.name, err, st, b)
			}
		}
	}
}
