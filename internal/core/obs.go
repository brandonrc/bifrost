package core

// Provider-agnostic wire types for the cluster drill-down observability
// tabs (api-v1.md §5.3/§5.6/§5.8, Milestone C) that are NOT the node
// breakdown (that lives in node.go):
//
//   - ClusterEvents — Kubernetes Events for the cluster's objects, the body
//     of GET /api/v1/clusters/{id}/events. Sourced from the K8s API (never
//     the Ray dashboard), so it answers even when Ray is down — the
//     highest-value signal for "why won't this cluster come up" (image
//     pulls, probe failures, scheduling).
//   - ClusterMetrics — a normalized cluster resource-usage summary (used
//     vs total CPU/GPU/memory + node counts), the body of
//     GET /api/v1/clusters/{id}/metrics. Distilled from the Ray
//     dashboard's autoscaler status so the UI can render stat tiles
//     against one schema.
//   - ClusterLogs — a non-streaming, tail-capped pod log view, the body of
//     GET /api/v1/clusters/{id}/logs. The WS streaming upgrade is future
//     work (api-v1.md §5.6); this is the pragmatic first cut.
//
// These are pure domain types: this package never depends on a Kubernetes
// client (ADR-0002). The backends outside this package produce them.

// ---------------------------------------------------------------------------
// Events (§5.8)
// ---------------------------------------------------------------------------

// ClusterEvent is one normalized Kubernetes Event about a cluster object
// (api-v1.md §5.8). Optional fields are nil because the two Event schemas
// (core/v1 and events.k8s.io/v1) name them differently and any of them may
// be absent.
type ClusterEvent struct {
	// EventType is the event severity: Normal or Warning (verbatim from
	// `type`).
	EventType string `json:"type"`
	// Reason is a short machine reason (FailedScheduling, Pulled,
	// BackOff, …).
	Reason *string `json:"reason,omitempty"`
	// Message is a human-readable message.
	Message *string `json:"message,omitempty"`
	// Count is how many times this event has fired (K8s collapses
	// repeats). Defaults to 1 when the source records no count.
	Count uint32 `json:"count"`
	// FirstSeen is the first occurrence (RFC3339); nil when the source
	// omits it.
	FirstSeen *string `json:"first_seen,omitempty"`
	// LastSeen is the most recent occurrence (RFC3339); the field the
	// list is sorted by.
	LastSeen *string `json:"last_seen,omitempty"`
	// Object is the object the event is about, as Kind/name (e.g.
	// Pod/foo-head-abc).
	Object *string `json:"object,omitempty"`
}

// ClusterEvents is the Kubernetes Events for one cluster's objects
// (api-v1.md §5.8), newest first and capped — the body of
// GET /api/v1/clusters/{id}/events.
type ClusterEvents struct {
	// ClusterId is the cluster id (RayCluster name).
	ClusterId string `json:"cluster_id"`
	// Events are the normalized events, most-recent-first, capped (see
	// the endpoint's cap).
	Events []ClusterEvent `json:"events"`
}

// ---------------------------------------------------------------------------
// Metrics (§5.x resource summary)
// ---------------------------------------------------------------------------

// ResourceStat is a single resource's capacity, and its used amount when
// known. Used/Total are in the resource's natural unit: CPU in cores, GPU
// in device count, memory in bytes. Used is nil when the cluster does not
// report live utilization (e.g. a non-autoscaling cluster whose Ray
// dashboard has no load-metrics report) — the tile then shows capacity
// only.
type ResourceStat struct {
	Used  *float64 `json:"used,omitempty"`
	Total float64  `json:"total"`
}

// ClusterMetrics is the normalized cluster resource-usage summary (the
// body of GET /api/v1/clusters/{id}/metrics), distilled from the Ray
// dashboard's autoscaler / load-metrics report. Every stat is optional
// because a cluster may not report GPUs, and an older/mismatched Ray may
// omit a field — the UI renders a tile only for the stats present.
type ClusterMetrics struct {
	// ClusterId is the cluster id (RayCluster name).
	ClusterId string `json:"cluster_id"`
	// Cpu is CPU cores used vs total across the Ray cluster.
	Cpu *ResourceStat `json:"cpu,omitempty"`
	// Gpu is GPU devices used vs total.
	Gpu *ResourceStat `json:"gpu,omitempty"`
	// Memory is memory bytes used vs total.
	Memory *ResourceStat `json:"memory,omitempty"`
	// ObjectStoreMemory is object-store memory bytes used vs total.
	ObjectStoreMemory *ResourceStat `json:"object_store_memory,omitempty"`
	// ActiveNodes is active Ray nodes the autoscaler reports; nil when
	// unavailable.
	ActiveNodes *uint64 `json:"active_nodes,omitempty"`
	// PendingNodes is nodes pending launch; nil when unavailable.
	PendingNodes *uint64 `json:"pending_nodes,omitempty"`
	// FailedNodes is nodes the autoscaler marked failed; nil when
	// unavailable.
	FailedNodes *uint64 `json:"failed_nodes,omitempty"`
}

// ---------------------------------------------------------------------------
// Logs (§5.6, non-streaming first cut)
// ---------------------------------------------------------------------------

// ClusterLogs is a tail-capped pod log view for one cluster (api-v1.md
// §5.6, non-streaming first cut) — the body of
// GET /api/v1/clusters/{id}/logs. WS streaming is the eventual design
// (Milestone C); this GET-tail form removes the pending-backend stub now.
type ClusterLogs struct {
	// ClusterId is the cluster id (RayCluster name).
	ClusterId string `json:"cluster_id"`
	// Pods are the names of the cluster's pods the caller may tail (head
	// first), so the UI can offer a pod selector.
	Pods []string `json:"pods"`
	// Pod is the pod these Lines are from (the requested pod, or the head
	// pod when none was requested). Empty only when the cluster has no
	// pods yet.
	Pod string `json:"pod"`
	// Tail is the tail line count that was requested.
	Tail uint32 `json:"tail"`
	// Lines are the most recent log lines (up to Tail), oldest first.
	Lines []string `json:"lines"`
	// Truncated is true when the tail was filled (there may be older
	// lines beyond it).
	Truncated bool `json:"truncated"`
}
