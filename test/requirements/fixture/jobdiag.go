package fixture

import (
	"context"
	"fmt"
	"strings"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sort"

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
func DiagnoseJobHead(t req.T, tgt req.Target, jobID, cluster string) string {
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
			fmt.Fprintf(&b, "head rayStartParams: %v\n", rc.Spec.HeadGroupSpec.RayStartParams)
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
			if p.Labels["ray.io/node-type"] == "head" {
				keys := make([]string, 0, len(p.Labels))
				for k := range p.Labels {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				b.WriteString("head labels:")
				for _, k := range keys {
					fmt.Fprintf(&b, " %s=%s", k, p.Labels[k])
				}
				b.WriteString("\n")
				var nps networkingv1.NetworkPolicyList
				if err := k.List(ctx, &nps, ctrlclient.InNamespace(tgt.Namespace())); err == nil {
					for _, np := range nps.Items {
						sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
						if err != nil || !sel.Matches(labels.Set(p.Labels)) {
							continue
						}
						fmt.Fprintf(&b, "policy %s selects the head: types=%v", np.Name, np.Spec.PolicyTypes)
						for _, r := range np.Spec.Ingress {
							b.WriteString(" ingress{")
							for _, from := range r.From {
								if from.NamespaceSelector != nil {
									fmt.Fprintf(&b, " ns%v", from.NamespaceSelector.MatchLabels)
								}
								if from.PodSelector != nil {
									fmt.Fprintf(&b, " pod%v%v", from.PodSelector.MatchLabels, from.PodSelector.MatchExpressions)
								}
								b.WriteString(";")
							}
							b.WriteString(" ports")
							for _, pt := range r.Ports {
								if pt.Port != nil {
									fmt.Fprintf(&b, " %s", pt.Port.String())
								}
							}
							b.WriteString("}")
						}
						b.WriteString("\n")
					}
				}
			}
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
t0 = time.time()
try:
    with urllib.request.urlopen("http://%s/api/version", timeout=15) as r:
        print("GET /api/version", r.status, "%%.2fs" %% (time.time() - t0), r.read(200))
except Exception as e:
    print("GET /api/version ERROR", "%%.2fs" %% (time.time() - t0), type(e).__name__, str(e)[:160])
`, head, head)
		// Four peer identities, one per ingress rule that should admit them:
		// the operator (from kuberay and from the probe namespace), a pod of
		// the same cluster (the submitter's shape), and the control plane.
		peers := []struct {
			name string
			ns   string
			lbl  map[string]string
		}{
			{"operator@kuberay", "kuberay", map[string]string{"app.kubernetes.io/name": "kuberay-operator"}},
			{"operator@probe-ns", "", map[string]string{"app.kubernetes.io/name": "kuberay-operator"}},
			{"same-cluster@" + tgt.Namespace(), tgt.Namespace(), map[string]string{"bifrost.dev/cluster-id": jobID}},
			{"control-plane@" + tgt.Namespace(), tgt.Namespace(), map[string]string{"bifrost.dev/control-plane": "true"}},
		}
		for _, peer := range peers {
			res, err := pr.RunPod(ctx, req.PodSpec{
				Namespace: peer.ns,
				Labels:    peer.lbl,
				Image:     pr.RayImage(),
				Command:   []string{"python", "-c", script},
				Timeout:   2 * time.Minute,
			})
			if err != nil {
				fmt.Fprintf(&b, "probe %s -> %s: failed to run: %v\n", peer.name, head, err)
				continue
			}
			fmt.Fprintf(&b, "probe %s -> %s:\n%s", peer.name, head, res.Logs)
		}
	}
	return b.String()
}
