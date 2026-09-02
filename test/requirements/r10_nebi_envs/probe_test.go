// Requirement 10 — nebi environments on the cluster. The first thing a
// custom environment image hit was defect 2: the provisioner set no probes,
// KubeRay's defaults shelled out to `wget`, and an image without it — the
// checkmaite Ray image on grace, any slim environment — ran Ray perfectly
// and was killed by the liveness probe every ~10 minutes
// (docs/defects/2026-09-02-health-probes-assume-wget.md). Fixed 2026-09-02:
// the provisioner sets python-based probes; verified on grace with the
// checkmaite image reaching running in 80 s with zero restarts.
package r10_nebi_envs

import (
	"context"
	"net/http"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// REQ_NOWGET_RAY_IMAGE names a Ray image that ships neither wget nor curl
// (on grace: localhost:32000/checkmaite-api:2.56.0-r1). Without it the test
// skips with a recorded reason; the public rayproject/ray images all carry
// wget, so there is no portable stand-in — but the probe shape itself is
// asserted on every Kubernetes target regardless.
func TestImageWithoutWgetReachesRunning(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 10, "a Ray image without wget reaches running and stays there: the probes use Ray's own health endpoints through python")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	k, _ := tgt.K8s()

	image := os.Getenv("REQ_NOWGET_RAY_IMAGE")
	if image == "" {
		image = fixture.RayImage()
		t.Log(req.Line{Kind: "skip", Req: 0, Reason: "REQ_NOWGET_RAY_IMAGE not set: probe shape asserted with the default image only"}.Format())
	}
	id := req.Name("nowget")
	resp, err := tgt.As("admin").API().CreateClusterWithResponse(ctx, fixture.ClusterBodyWithImage(id, "team-a", image, nil))
	if err != nil || resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
	}
	fixture.WaitObserved(t, tgt, "admin", id, "running")

	var pods corev1.PodList
	if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{"ray.io/cluster": id}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("no ray pods for a running cluster")
	}
	for _, p := range pods.Items {
		c := p.Spec.Containers[0]
		for name, probe := range map[string]*corev1.Probe{"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe} {
			if probe == nil || probe.Exec == nil || len(probe.Exec.Command) == 0 {
				t.Fatalf("pod %s: %s probe missing or not exec", p.Name, name)
			}
			if probe.Exec.Command[0] != "python" {
				t.Fatalf("pod %s: %s probe runs %q; an image without wget would be restarted forever", p.Name, name, probe.Exec.Command[0])
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount != 0 {
				t.Errorf("pod %s restarted %d times while reaching running", p.Name, cs.RestartCount)
			}
		}
	}
}
