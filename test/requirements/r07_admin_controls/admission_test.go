// Requirement 7 — group administrators control which profiles, images, CPU,
// memory, GPU and maximum worker counts are allowed.
//
// CPU/memory/GPU are governed by the quota and budget policy; this file is
// about the two the policy could not express until now: which images a
// cluster may run and how many workers it may ask for. The deployment-wide
// rule is control-plane configuration (`serve --allowed-images`,
// `--max-workers`, the policy's "*" admission rule); per-project rules are
// set through PUT /settings/policy (profiles_test.go's helpers).
//
// On inproc the test builds its own restricted control plane; on a cluster
// target it reads what the deployment was configured with from
// REQ_ADMISSION_DISALLOWED_IMAGE / REQ_ADMISSION_MAX_WORKERS (the kind lane
// sets both) and skips with a recorded reason when they are absent.
package r07_admin_controls

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
	"github.com/brandonrc/bifrost/test/requirements/target/inproc"
)

const disallowedInproc = "docker.io/library/nginx:1.27"

// restricted returns a target whose control plane forbids some image and
// caps workers, plus the image and cap to test against.
func restricted(t *testing.T) (tgt req.Target, disallowed string, cap int) {
	t.Helper()
	tgt = target.Get(t)
	if tgt.Name() == "inproc" {
		return inproc.New(t, inproc.WithAdmission([]string{"rayproject/"}, 2)), disallowedInproc, 2
	}
	disallowed = os.Getenv("REQ_ADMISSION_DISALLOWED_IMAGE")
	capStr := os.Getenv("REQ_ADMISSION_MAX_WORKERS")
	if disallowed == "" || capStr == "" {
		reason := "target " + tgt.Name() + " does not declare its admission configuration (REQ_ADMISSION_DISALLOWED_IMAGE / REQ_ADMISSION_MAX_WORKERS)"
		t.Log(req.Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
	n, err := strconv.Atoi(capStr)
	if err != nil {
		t.Fatalf("REQ_ADMISSION_MAX_WORKERS=%q: %v", capStr, err)
	}
	return tgt, disallowed, n
}

func TestDisallowedImageIsRefused(t *testing.T) {
	req.Covers(t, 7, "an image outside the administrator's allowlist is refused with 400 and nothing is created")
	tgt, disallowed, _ := restricted(t)
	id := req.Name("img")
	resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(t.Context(), fixture.ClusterBodyWithImage(id, "team-a", disallowed, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("create with disallowed image = %d %s, want 400", resp.StatusCode(), resp.Body)
	}
	if st, _ := fixture.Get(t, tgt, "admin", id); st != http.StatusNotFound {
		t.Fatalf("a refused create must persist nothing; get = %d", st)
	}
	// The allowlisted image is still fine.
	fixture.MustCreate(t, tgt, "dev-a", req.Name("okimg"), "team-a")
}

func TestWorkerCapIsEnforced(t *testing.T) {
	req.Covers(t, 7, "a cluster asking for more workers than the administrator's cap is refused with 400")
	tgt, _, cap := restricted(t)
	id := req.Name("cap")
	body := fixture.ClusterBody(id, "team-a", nil)
	over := int32(cap + 1)
	body.Spec.WorkerGroups[0].MaxReplicas = over
	body.Spec.WorkerGroups[0].MinReplicas = 0
	body.Spec.WorkerGroups[0].Replicas = 0
	resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("create over the worker cap = %d %s, want 400", resp.StatusCode(), resp.Body)
	}
	body.Spec.WorkerGroups[0].MaxReplicas = int32(cap)
	resp, err = tgt.As("dev-a").API().CreateClusterWithResponse(t.Context(), body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create at the cap = %d %s, want 201", resp.StatusCode(), resp.Body)
	}
}

func TestDeveloperCannotChangeAdmissionPolicy(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 7, "only an administrator changes governance policy; a project operator's update is 403")
	resp, err := tgt.As("dev-a").API().UpdatePolicyWithResponse(t.Context(), fixture.EmptyPolicyUpdate())
	if err != nil || resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("project operator update_policy: err=%v status=%v, want 403", err, resp.StatusCode())
	}
}

func TestPerProjectImageAllowlist(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 7, "an administrator's per-project image allowlist refuses that project's create with 400 while other projects are unaffected")
	ctx := context.Background()
	setPolicySections(t, tgt, `{"admission":{"team-b":{"allowed_images":["registry.example/"]}}}`)

	id := req.Name("pimg")
	resp, err := tgt.As("dev-b").API().CreateClusterWithResponse(ctx, fixture.ClusterBody(id, "team-b", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("team-b create outside its allowlist = %d %s, want 400", resp.StatusCode(), resp.Body)
	}
	if st, _ := fixture.Get(t, tgt, "admin", id); st != http.StatusNotFound {
		t.Fatalf("a refused create must persist nothing; get = %d", st)
	}
	fixture.MustCreate(t, tgt, "dev-a", req.Name("pimga"), "team-a")
}
