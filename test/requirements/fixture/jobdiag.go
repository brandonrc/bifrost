package fixture

import (
	"context"
	"fmt"
	"strings"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// DiagnoseJobHead is the operator's view of a RayJob's head while KubeRay is
// (or was) polling it: the RayCluster's state, every pod of the cluster with
// phase, readiness, container state and restarts, the pods' recent events,
// and an HTTP probe from the kuberay namespace carrying the operator's pod
// label. On kind (runs 33802820554 … 33817115292) KubeRay's job status
// checks timed out for five minutes on every RayJob while the same probe
// reached an ordinary cluster's head, and a probe run after the failure
// only saw the cluster being torn down — so this runs while the job is
// still initializing, and again at failure.
func DiagnoseJobHead(t req.T, tgt req.Target, cluster string) string {
	t.Helper()
	if cluster == "" {
		return "(job view carries no cluster yet)"
	}
	var b strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if k, ok := tgt.K8s(); ok {
		var rc rayv1.RayCluster
		if err := k.Get(ctx, ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: cluster}, &rc); err != nil {
			fmt.Fprintf(&b, "raycluster %s: %v\n", cluster, err)
		} else {
			fmt.Fprintf(&b, "raycluster %s: ready=%d desired=%d", cluster, rc.Status.ReadyWorkerReplicas, rc.Status.DesiredWorkerReplicas)
			for _, c := range rc.Status.Conditions {
				fmt.Fprintf(&b, " %s=%s(%s)", c.Type, c.Status, c.Reason)
			}
			b.WriteString("\n")
		}
		var pods corev1.PodList
		if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabels{"ray.io/cluster": cluster}); err != nil {
			fmt.Fprintf(&b, "list pods: %v\n", err)
		}
		names := map[string]bool{}
		for _, p := range pods.Items {
			names[p.Name] = true
			ready := "unknown"
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady {
					ready = string(c.Status)
					if c.Reason != "" {
						ready += "(" + c.Reason + ")"
					}
				}
			}
			fmt.Fprintf(&b, "pod %s type=%s phase=%s ip=%s ready=%s", p.Name, p.Labels["ray.io/node-type"], p.Status.Phase, p.Status.PodIP, ready)
			for _, cs := range p.Status.ContainerStatuses {
				state := "running"
				if cs.State.Waiting != nil {
					state = "waiting:" + cs.State.Waiting.Reason
				} else if cs.State.Terminated != nil {
					state = fmt.Sprintf("terminated:%s/%d", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				}
				fmt.Fprintf(&b, " [%s ready=%v restarts=%d %s]", cs.Name, cs.Ready, cs.RestartCount, state)
			}
			b.WriteString("\n")
		}
		var events corev1.EventList
		if err := k.List(ctx, &events, ctrlclient.InNamespace(tgt.Namespace())); err == nil {
			for _, e := range events.Items {
				if names[e.InvolvedObject.Name] || e.InvolvedObject.Name == cluster {
					fmt.Fprintf(&b, "event %s %s/%s: %s x%d\n", e.InvolvedObject.Name, e.Type, e.Reason, strings.TrimSpace(e.Message), e.Count)
				}
			}
		}
	}
	if pr, ok := tgt.(req.PodRunner); ok {
		head := fmt.Sprintf("%s-head-svc.%s.svc:8265", cluster, tgt.Namespace())
		script := fmt.Sprintf(`
import socket, time, urllib.request
host = "%s".split(":")[0]
try:
    print("resolve", host, socket.gethostbyname(host))
except Exception as e:
    print("resolve", host, "ERROR", type(e).__name__, e)
for path in ("/api/version", "/api/jobs/"):
    t0 = time.time()
    try:
        with urllib.request.urlopen("http://%s" + path, timeout=20) as r:
            print("GET", path, r.status, "%%.2fs" %% (time.time() - t0), r.read(300))
    except Exception as e:
        print("GET", path, "ERROR", "%%.2fs" %% (time.time() - t0), type(e).__name__, str(e)[:200])
`, head, head)
		res, err := pr.RunPod(ctx, req.PodSpec{
			Namespace: "kuberay",
			Labels:    map[string]string{"app.kubernetes.io/name": "kuberay-operator"},
			Image:     pr.RayImage(),
			Command:   []string{"python", "-c", script},
			Timeout:   3 * time.Minute,
		})
		if err != nil {
			fmt.Fprintf(&b, "operator-shaped probe failed to run: %v\n", err)
		} else {
			fmt.Fprintf(&b, "operator-shaped probe from kuberay to %s:\n%s", head, res.Logs)
		}
	}
	return b.String()
}
