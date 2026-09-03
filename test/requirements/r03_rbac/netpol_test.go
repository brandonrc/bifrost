package r03_rbac

import (
	"context"
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// agnhost is the Kubernetes e2e probe image: tiny, has `netexec` (an HTTP
// server) and `connect` (a TCP probe with a timeout).
const agnhost = "registry.k8s.io/e2e-test-images/agnhost:2.45"

// TestCNIEnforcesNetworkPolicy is the L3 lane's precondition (spec §3): if a
// deny-all policy does not block a probe, the CNI (kindnet, say) ignores
// NetworkPolicy and every isolation result below it is vacuous. The lane is
// invalid, and this says so loudly. A positive control first proves the
// probe itself can connect.
func TestCNIEnforcesNetworkPolicy(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "the cluster's CNI enforces NetworkPolicy, so isolation results mean something")
	req.NeedsCapability(t, tgt, "probes")
	pr, ok := tgt.(req.PodRunner)
	if !ok {
		t.Fatalf("target %s declares capability probes but is not a req.PodRunner", tgt.Name())
	}
	k, _ := tgt.K8s()
	ctx := context.Background()

	server, err := pr.RunPod(ctx, req.PodSpec{
		Labels:  map[string]string{"req.bifrost.dev/role": "cni-server"},
		Image:   agnhost,
		Command: []string{"/agnhost", "netexec", "--http-port=8080"},
		Detach:  true,
	})
	if err != nil {
		t.Fatalf("server pod: %v", err)
	}
	connect := func() (bool, string) {
		res, err := pr.RunPod(ctx, req.PodSpec{
			Image:   agnhost,
			Command: []string{"/agnhost", "connect", fmt.Sprintf("%s:8080", server.IP), "--timeout=5s"},
			Timeout: 2 * time.Minute,
		})
		if err != nil {
			return false, err.Error()
		}
		return res.Succeeded, res.Logs
	}
	if ok, logs := connect(); !ok {
		t.Fatalf("positive control: probe could not reach the server before any policy existed: %s", logs)
	}

	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name("deny-all"),
			Namespace: pr.ProbeNamespace(),
			Labels:    map[string]string{req.RunLabel: req.RunID()},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{req.RunLabel: req.RunID(), "req.bifrost.dev/role": "cni-server"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if err := k.Create(ctx, deny); err != nil {
		t.Fatalf("create deny-all policy: %v", err)
	}
	// Calico's dataplane can lag a policy by a few seconds; the probe is
	// re-run until it is blocked or the lane budget says the CNI does not
	// enforce.
	req.Eventually(t, tgt, func() (bool, string) {
		ok, logs := connect()
		if ok {
			return false, "probe still connects under a deny-all policy — the CNI does not enforce NetworkPolicy"
		}
		return true, logs
	})
}

// The cross-owner half of the grace run: a pod in the notebook namespace
// labelled as someone else cannot reach the head's Ray Client, dashboard/
// Jobs, or GCS ports at all. The owner half (the connect succeeds) lives in
// r06 — together they are the requirement.
func TestCrossOwnerHeadPortsAreUnreachable(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "another owner's pod cannot open :8265, :10001 or :6379 on a private cluster's head")
	req.NeedsCapability(t, tgt, "calico")
	req.NeedsCapability(t, tgt, "probes")
	pr, ok := tgt.(req.PodRunner)
	if !ok {
		t.Fatalf("target %s declares capability probes but is not a req.PodRunner", tgt.Name())
	}
	ctx := context.Background()

	id := req.Name("xo")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	head := fmt.Sprintf("%s-head-svc.%s.svc", id, tgt.Namespace())
	script := fmt.Sprintf(`
import socket, sys
open_ports = []
for port in (8265, 10001, 6379):
    s = socket.socket(); s.settimeout(5)
    try:
        s.connect((%q, port)); open_ports.append(port)
    except Exception as e:
        print("blocked", port, type(e).__name__)
    finally:
        s.close()
print("OPEN", open_ports)
sys.exit(1 if open_ports else 0)
`, head)
	res, err := pr.RunPod(ctx, req.PodSpec{
		Labels:  map[string]string{"bifrost.dev/owner": "req-not-the-owner"},
		Image:   pr.RayImage(),
		Command: []string{"python", "-c", script},
		Timeout: 4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("probe pod: %v", err)
	}
	if !res.Succeeded {
		t.Fatalf("a non-owner pod reached the head:\n%s", res.Logs)
	}
}

// TestKubeRayOperatorPeerReachesTheHeadDashboard: RayJob status and
// RayService readiness both depend on the KubeRay operator polling the
// head's dashboard (:8265) from its own namespace, which the tenant policy
// admits by pod label from any namespace. On kind (runs 33802820554 and
// 33808304909) the operator timed out on every poll while the submitter
// and Bifrost's proxy reached the same dashboard, so this pins the
// operator-shaped path down: an operator-labelled probe from the probe
// namespace and from "kuberay" must connect; an unlabelled one must not.
func TestKubeRayOperatorPeerReachesTheHeadDashboard(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "the tenant policy admits KubeRay's operator, by pod label from any namespace, to the head dashboard that RayJob and RayService status depend on")
	req.NeedsCapability(t, tgt, "probes")
	pr, ok := tgt.(req.PodRunner)
	if !ok {
		t.Fatalf("target %s declares capability probes but is not a req.PodRunner", tgt.Name())
	}
	ctx := context.Background()
	id := req.Name("oppeer")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	head := fmt.Sprintf("%s-head-svc.%s.svc:8265", id, tgt.Namespace())
	operator := map[string]string{"app.kubernetes.io/name": "kuberay-operator"}
	for _, ns := range []string{"", "kuberay"} {
		res, err := pr.RunPod(ctx, req.PodSpec{
			Namespace: ns,
			Labels:    operator,
			Image:     agnhost,
			Command:   []string{"/agnhost", "connect", head, "--timeout=5s"},
			Timeout:   2 * time.Minute,
		})
		if err != nil || !res.Succeeded {
			t.Errorf("operator-labelled probe from namespace %q could not reach %s: err=%v logs=%s", ns, head, err, res.Logs)
		}
	}
	res, err := pr.RunPod(ctx, req.PodSpec{
		Image:   agnhost,
		Command: []string{"/agnhost", "connect", head, "--timeout=5s"},
		Timeout: 2 * time.Minute,
	})
	if err == nil && res.Succeeded {
		t.Errorf("negative control: an unlabelled probe reached %s; the tenant policy is not enforcing", head)
	}
}
