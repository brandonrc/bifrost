package r06_self_serve

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// The consumer path, as run by hand on grace 2026-09-02: a pod in the
// notebook namespace carrying the owner's label submits a Ray job to the
// cluster's head over :8265 and opens a Ray Client session on :10001 — the
// two things bifrost-jupyter's connect cell does. Probe pods stand in for
// the notebook pod; the label is what KubeSpawner will have to stamp.
func TestOwnerNotebookPodConnectsToItsCluster(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "the owner's notebook pod submits a Ray job and opens a Ray Client session against its private cluster")
	req.NeedsCapability(t, tgt, "probes")
	pr, ok := tgt.(req.PodRunner)
	if !ok {
		t.Fatalf("target %s declares capability probes but is not a req.PodRunner", tgt.Name())
	}
	ctx := context.Background()
	k, _ := tgt.K8s()

	id := req.Name("own")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	var rc rayv1.RayCluster
	if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, &rc); err != nil {
		t.Fatal(err)
	}
	owner := rc.Labels[ownerLabel]
	if owner == "" {
		t.Fatalf("RayCluster has no %s label", ownerLabel)
	}

	head := fmt.Sprintf("%s-head-svc.%s.svc", id, tgt.Namespace())
	// The job is retried up to three times when Ray reports FAILED with no
	// output of its own: on a 4-vCPU CI runner the head's job agent can
	// still be warming up when the first submission lands. A job that runs
	// and prints the wrong thing is never retried — that would be a bug.
	script := fmt.Sprintf(`
import sys, time
from ray.job_submission import JobSubmissionClient
c = JobSubmissionClient("http://%[1]s:8265")
status, logs = None, ""
for attempt in range(3):
    jid = c.submit_job(entrypoint="python -c 'import ray; ray.init(); print(\"REQ-JOB-OK\", ray.cluster_resources().get(\"CPU\"))'")
    for _ in range(120):
        status = str(c.get_job_status(jid))
        if status in ("SUCCEEDED", "FAILED", "STOPPED"):
            break
        time.sleep(2)
    logs = c.get_job_logs(jid)
    info = c.get_job_info(jid)
    print("attempt", attempt, "job", status, "message:", getattr(info, "message", None)); print(logs[-300:])
    if status == "SUCCEEDED" or logs.strip():
        break
    time.sleep(5)
if status != "SUCCEEDED" or "REQ-JOB-OK" not in logs:
    sys.exit(2)
import ray
ray.init("ray://%[1]s:10001")
@ray.remote
def double(x): return x * 2
total = sum(ray.get([double.remote(i) for i in range(10)]))
print("REQ-CLIENT-OK", total)
sys.exit(0 if total == 90 else 3)
`, head)

	res, err := pr.RunPod(ctx, req.PodSpec{
		Labels:  map[string]string{ownerLabel: owner},
		Image:   pr.RayImage(),
		Command: []string{"python", "-c", script},
		Timeout: 8 * time.Minute,
	})
	if err != nil {
		t.Fatalf("owner probe pod: %v", err)
	}
	if !res.Succeeded || !strings.Contains(res.Logs, "REQ-JOB-OK") || !strings.Contains(res.Logs, "REQ-CLIENT-OK 90") {
		t.Fatalf("owner pod could not use its cluster (succeeded=%v):\n%s", res.Succeeded, res.Logs)
	}
}
