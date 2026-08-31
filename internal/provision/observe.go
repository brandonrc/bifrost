package provision

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	"github.com/brandonrc/bifrost/internal/core"
)

// Live-observation mappers (api-v1.md §5.3/§5.8/§5.6): pure functions from
// already-fetched Kubernetes objects to Bifrost's domain observation
// types. Deliberately outside internal/provision/live (which is in
// COVERAGE_EXCLUDE and kept I/O-only, per the wave plan): these functions
// carry the entire filtering/normalization/parsing logic, so they live
// where they are unit-testable and covered. The live client
// (internal/provision/live) does only the I/O (get RayCluster, list
// pods/events, fetch pod logs) and hands the raw typed results here.
//
// Ported from mobula-provision/src/kuberay.rs:869-1063 (node_breakdown,
// events_from_k8s, tail_lines and their quantity/label helpers). See each
// function's doc comment for the typed-API divergences from the Rust
// reference oracle, which read untyped serde_json::Value against a
// kube-rs dynamic client — deliberately deferred to this task by Task 5
// (task-5-report.md's "Scope decision" section, impedance item 6's
// sibling note).

// ---------------------------------------------------------------------------
// Node breakdown (api-v1.md §5.3, GET /api/v1/clusters/{id}/nodes)
// ---------------------------------------------------------------------------

const (
	// RayClusterLabel is the KubeRay pod label carrying the owning
	// RayCluster's name — the selector the live client lists pods by
	// (`ray.io/cluster=<name>`). Ported from kuberay.rs:724.
	RayClusterLabel = "ray.io/cluster"
	// RayGroupLabel is the KubeRay pod label carrying the worker-group
	// name; head pods carry the head group's name and are distinguished by
	// [RayNodeTypeLabel]. Ported from kuberay.rs:727.
	RayGroupLabel = "ray.io/group"
	// RayNodeTypeLabel is the KubeRay pod label carrying the node type:
	// "head" or "worker". Ported from kuberay.rs:729.
	RayNodeTypeLabel = "ray.io/node-type"
)

// NodeBreakdown builds the head + per-worker-group node breakdown
// (api-v1.md §5.3) from a RayCluster object and the pods KubeRay owns for
// it. pods must already be scoped to the cluster (the live client lists by
// [RayClusterLabel]=id before calling this) — NodeBreakdown does no
// cluster-membership filtering of its own, matching the Rust reference.
// Worker groups follow the RayCluster spec's order; a group seen only on
// pods (e.g. mid-rename) is appended so nothing a pod belongs to is
// silently dropped. desired comes from the spec (Replicas, else
// MinReplicas) since per-group desired counts are not in the status; ready
// is counted from the pods (Running + Ready). rc may be nil (treated as a
// RayCluster with no worker groups), matching a defensive nil-spec guard.
//
// Pure — no Kubernetes client — so it is exhaustively unit-tested against
// constructed Pod/RayCluster fixtures. Ported from
// kuberay.rs:876-954 (node_breakdown). The Rust reference also hand-rolled
// parse_cpu/parse_memory/parse_gpu (kuberay.rs:739-788) to turn quantity
// *strings* into numbers, because it read pods as untyped
// serde_json::Value; that parsing has no equivalent here because typed
// corev1.Pod container requests are already resource.Quantity values —
// [sumRequests] just reads them.
func NodeBreakdown(clusterID string, rc *rayv1.RayCluster, pods []corev1.Pod) core.ClusterNodes {
	var head *core.NodeView
	for i := range pods {
		if pods[i].Labels[RayNodeTypeLabel] == "head" {
			nv := podToNodeView(&pods[i], true)
			head = &nv
			break
		}
	}

	isWorker := func(p *corev1.Pod) bool { return p.Labels[RayNodeTypeLabel] != "head" }

	var workerGroups []core.WorkerGroupNodes
	seen := map[string]bool{}

	var specGroups []rayv1.WorkerGroupSpec
	if rc != nil {
		specGroups = rc.Spec.WorkerGroupSpecs
	}
	for _, g := range specGroups {
		name := g.GroupName
		desired, ok := nonNegativeU32(g.Replicas)
		if !ok {
			desired, _ = nonNegativeU32(g.MinReplicas)
		}
		nodes := nodesForGroup(pods, isWorker, name)
		seen[name] = true
		workerGroups = append(workerGroups, core.WorkerGroupNodes{
			Name: name, Desired: desired, Ready: countReady(nodes), Nodes: nodes,
		})
	}

	// Groups present on pods but absent from the spec (mid-rename / scaled
	// by something else): append them so their nodes are still reported.
	// Desired is unknown here, so it falls back to the observed pod count.
	for i := range pods {
		p := &pods[i]
		if !isWorker(p) {
			continue
		}
		name, ok := p.Labels[RayGroupLabel]
		if !ok || seen[name] {
			continue
		}
		nodes := nodesForGroup(pods, isWorker, name)
		seen[name] = true
		workerGroups = append(workerGroups, core.WorkerGroupNodes{
			Name: name, Desired: uint32(len(nodes)), Ready: countReady(nodes), Nodes: nodes,
		})
	}

	return core.ClusterNodes{ClusterId: clusterID, Head: head, WorkerGroups: workerGroups}
}

// nonNegativeU32 reads an optional int32 replica count as a uint32,
// mirroring the Rust reference's `.and_then(|v| v.as_u64())` (which yields
// None for a missing OR negative value, falling through to the next
// `.or_else`). Returns ok=false for nil or negative.
func nonNegativeU32(v *int32) (n uint32, ok bool) {
	if v == nil || *v < 0 {
		return 0, false
	}
	return uint32(*v), true
}

func nodesForGroup(pods []corev1.Pod, isWorker func(*corev1.Pod) bool, group string) []core.NodeView {
	var nodes []core.NodeView
	for i := range pods {
		p := &pods[i]
		if !isWorker(p) || p.Labels[RayGroupLabel] != group {
			continue
		}
		nodes = append(nodes, podToNodeView(p, false))
	}
	return nodes
}

func countReady(nodes []core.NodeView) uint32 {
	var n uint32
	for _, node := range nodes {
		if node.Phase == "Running" && node.Ready {
			n++
		}
	}
	return n
}

// podToNodeView maps one pod to a [core.NodeView]. Ported from
// kuberay.rs:822-867 (pod_to_node_view).
func podToNodeView(pod *corev1.Pod, isHead bool) core.NodeView {
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}
	phase := string(pod.Status.Phase)
	if phase == "" {
		phase = "Unknown"
	}
	nv := core.NodeView{
		PodName: pod.Name,
		IsHead:  isHead,
		Phase:   phase,
		Ready:   ready,
	}
	if !isHead {
		if g, ok := pod.Labels[RayGroupLabel]; ok {
			nv.Group = &g
		}
	}
	if pod.Status.PodIP != "" {
		ip := pod.Status.PodIP
		nv.NodeIp = &ip
	}
	if pod.Spec.NodeName != "" {
		host := pod.Spec.NodeName
		nv.Host = &host
	}
	if v, ok := sumRequests(pod, corev1.ResourceCPU, func(q resource.Quantity) float64 { return q.AsApproximateFloat64() }, func(a, b float64) float64 { return a + b }); ok {
		nv.Cpu = &v
	}
	if v, ok := sumRequests(pod, corev1.ResourceMemory, func(q resource.Quantity) int64 { return q.Value() }, func(a, b int64) int64 { return a + b }); ok {
		u := uint64(v)
		nv.MemoryBytes = &u
	}
	if v, ok := sumRequests(pod, corev1.ResourceName("nvidia.com/gpu"), func(q resource.Quantity) float64 { return q.AsApproximateFloat64() }, func(a, b float64) float64 { return a + b }); ok {
		nv.Gpu = &v
	}
	return nv
}

// sumRequests sums one named resource request across every container in a
// pod, returning ok=false when no container declares it (distinct from a
// declared-but-zero quantity). Ported from kuberay.rs:793-819
// (sum_requests); extract replaces the Rust reference's quantity-string
// parse function, since a typed corev1.Pod's requests are already
// resource.Quantity values.
func sumRequests[T any](pod *corev1.Pod, name corev1.ResourceName, extract func(resource.Quantity) T, add func(T, T) T) (T, bool) {
	var acc T
	found := false
	for _, c := range pod.Spec.Containers {
		q, ok := c.Resources.Requests[name]
		if !ok {
			continue
		}
		v := extract(q)
		if found {
			acc = add(acc, v)
		} else {
			acc = v
			found = true
		}
	}
	return acc, found
}

// ---------------------------------------------------------------------------
// Events (api-v1.md §5.8, GET /api/v1/clusters/{id}/events)
// ---------------------------------------------------------------------------

// MaxEvents caps the events [EventsFromK8s] returns: a busy namespace can
// hold thousands; the newest window is what the tab shows. Ported from
// kuberay.rs:962.
const MaxEvents = 200

// objectBelongsToCluster reports whether a Kubernetes object name belongs
// to the cluster id: true for the RayCluster itself (exact match) and for
// everything KubeRay names under it (head/worker pods, the head service,
// …), which all carry the `<id>-` prefix. Ported from kuberay.rs:969-971.
func objectBelongsToCluster(id, objectName string) bool {
	return objectName == id || strings.HasPrefix(objectName, id+"-")
}

// EventsFromK8s normalizes a list of Kubernetes Events into the cluster's
// events (api-v1.md §5.8): keeps only events about clusterID's objects,
// sorts newest-first by last-seen (nil last-seen sinks to the bottom), and
// caps at [MaxEvents]. Pure, so the filtering/normalization is
// unit-tested without a cluster; the live client (internal/provision/live)
// does the I/O (list Events) and hands the result here. Ported from
// kuberay.rs:989-1043 (events_from_k8s).
//
// The Rust reference oracle read Events through kube-rs's dynamic API and
// so defensively normalized both the legacy core/v1 Event schema
// (involvedObject/firstTimestamp/count) and the events.k8s.io/v1 schema
// (regarding/eventTime/deprecatedCount/series) — it could not know at
// compile time which GVK a given dynamic watch would surface. This port's
// live client only ever lists typed corev1.Event objects — kuberay_client.rs's
// own event_resource() already pins {group: "", version: "v1"} (the
// legacy/core schema, never events.k8s.io), so the dual-schema branches
// were defensive, not load-bearing — and the API server normalizes an
// events.k8s.io/v1-sourced Event onto the legacy corev1.Event shape
// (InvolvedObject, Message, Count, FirstTimestamp/LastTimestamp, EventTime,
// Series) before it ever reaches client code. This port therefore reads
// only the legacy corev1.Event fields; Series is still consulted (it is a
// real field on corev1.Event, carrying collapsed-event compat data
// regardless of source schema).
func EventsFromK8s(clusterID string, raw []corev1.Event) core.ClusterEvents {
	events := make([]core.ClusterEvent, 0, len(raw))
	for i := range raw {
		ev := &raw[i]
		name := ev.InvolvedObject.Name
		if name == "" || !objectBelongsToCluster(clusterID, name) {
			continue
		}
		kind := ev.InvolvedObject.Kind
		if kind == "" {
			kind = "Object"
		}
		object := kind + "/" + name

		count := uint32(ev.Count)
		if count == 0 {
			if ev.Series != nil && ev.Series.Count > 0 {
				count = uint32(ev.Series.Count)
			} else {
				count = 1
			}
		}

		firstSeen := metaTimePtr(ev.FirstTimestamp)
		if firstSeen == nil {
			firstSeen = microTimePtr(ev.EventTime)
		}
		lastSeen := metaTimePtr(ev.LastTimestamp)
		if lastSeen == nil && ev.Series != nil {
			lastSeen = microTimePtr(ev.Series.LastObservedTime)
		}
		if lastSeen == nil {
			lastSeen = microTimePtr(ev.EventTime)
		}
		if lastSeen == nil {
			lastSeen = firstSeen
		}

		eventType := ev.Type
		if eventType == "" {
			eventType = "Normal"
		}
		var reason, message *string
		if ev.Reason != "" {
			r := ev.Reason
			reason = &r
		}
		if ev.Message != "" {
			m := ev.Message
			message = &m
		}

		events = append(events, core.ClusterEvent{
			EventType: eventType,
			Reason:    reason,
			Message:   message,
			Count:     count,
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
			Object:    &object,
		})
	}

	// Newest first by last-seen (RFC3339 sorts lexicographically). Events
	// with no timestamp sink to the bottom.
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i].LastSeen, events[j].LastSeen
		switch {
		case a != nil && b != nil:
			return *a > *b
		case a == nil:
			return false
		default:
			return true
		}
	})
	if len(events) > MaxEvents {
		events = events[:MaxEvents]
	}

	return core.ClusterEvents{ClusterId: clusterID, Events: events}
}

// metaTimePtr formats t as RFC3339, or nil when t is unset.
func metaTimePtr(t metav1.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z07:00")
	return &s
}

// microTimePtr formats t as RFC3339, or nil when t is unset.
func microTimePtr(t metav1.MicroTime) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z07:00")
	return &s
}

// ---------------------------------------------------------------------------
// Logs (api-v1.md §5.6, GET /api/v1/clusters/{id}/logs, non-streaming tail)
// ---------------------------------------------------------------------------

// TailLines splits a raw pod-log blob into the last tail lines (oldest
// first) and a flag for whether the tail was filled (older lines may exist
// beyond it). The Kubernetes API already tail-caps server-side; this
// defends against a source that ignores the cap and computes the
// truncated hint. Pure and tested. Ported from kuberay.rs:1049-1063.
func TailLines(raw string, tail uint32) ([]string, bool) {
	var lines []string
	if raw != "" {
		lines = strings.Split(raw, "\n")
		// Drop a trailing empty line from a final newline so it is not
		// counted.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}
	total := uint32(len(lines))
	if tail > 0 && total > tail {
		lines = lines[total-tail:]
	}
	truncated := tail > 0 && total >= tail
	return lines, truncated
}
