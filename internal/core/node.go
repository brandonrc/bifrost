package core

// Kubernetes-sourced node view for the cluster nodes tab (api-v1.md §5.3).
//
// Observability only (decision D2): there is no per-node mutation anywhere
// in the API — scale is group-level. The breakdown is read from the
// RayCluster and the pods KubeRay owns (label ray.io/cluster=<name>), NOT
// from the Ray dashboard, so it is available even when the dashboard is
// unreachable. That is a deliberate refinement of the original §5.3 draft
// (which named the Ray node summary as the source): Kubernetes is the
// authority for "what pods exist and where", and it answers when Ray does
// not.

// NodeView is a single Ray node (head or one worker) as Kubernetes sees it:
// the pod KubeRay created for it, its scheduling/readiness, and the
// compute it requests. Optional fields are nil because a pod may not yet
// be scheduled (no IP/host) and a quantity may be unparseable or unset.
type NodeView struct {
	// PodName is the pod's name (metadata.name).
	PodName string `json:"pod_name"`
	// Group is the worker-group name (ray.io/group); nil for the head
	// node.
	Group *string `json:"group,omitempty"`
	// IsHead is whether this is the cluster's head node.
	IsHead bool `json:"is_head"`
	// Phase is the Kubernetes pod phase: Pending | Running | Succeeded |
	// Failed | Unknown (verbatim from status.phase).
	Phase string `json:"phase"`
	// Ready is whether the pod's Ready condition is true. Distinct from
	// Phase: a pod can be Running but not yet Ready.
	Ready bool `json:"ready"`
	// NodeIp is the pod IP once scheduled (status.podIP); nil before
	// scheduling.
	NodeIp *string `json:"node_ip,omitempty"`
	// Host is the Kubernetes node the pod landed on (spec.nodeName); nil
	// before scheduling.
	Host *string `json:"host,omitempty"`
	// Cpu is CPU cores requested, summed across the pod's containers and
	// parsed from the K8s quantity (500m -> 0.5). nil when
	// unset/unparseable.
	Cpu *float64 `json:"cpu,omitempty"`
	// MemoryBytes is memory bytes requested, summed across the pod's
	// containers (2Gi -> 2147483648). nil when unset/unparseable.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
	// Gpu is GPUs requested (nvidia.com/gpu), summed across containers.
	// nil when the pod requests no GPU.
	Gpu *float64 `json:"gpu,omitempty"`
}

// WorkerGroupNodes is one worker group and its nodes (api-v1.md §5.3).
type WorkerGroupNodes struct {
	// Name is the worker-group name (groupName in the RayCluster spec).
	Name string `json:"name"`
	// Desired replicas: the group's `replicas` field, or `min_replicas`
	// when autoscaling leaves `replicas` unmanaged (ADR-0007). Per-group
	// desired counts are not in the RayCluster status, so this is the
	// spec's answer.
	Desired uint32 `json:"desired"`
	// Ready replicas: pods in this group that are Running and Ready.
	Ready uint32 `json:"ready"`
	// Nodes are the group's nodes.
	Nodes []NodeView `json:"nodes"`
}

// ClusterNodes is the head + per-worker-group node breakdown for one
// cluster (api-v1.md §5.3), the body of GET /api/v1/clusters/{id}/nodes.
type ClusterNodes struct {
	// ClusterId is the cluster id (RayCluster name).
	ClusterId string `json:"cluster_id"`
	// Head is the head node; nil if KubeRay has not created the head pod
	// yet.
	Head *NodeView `json:"head,omitempty"`
	// WorkerGroups has one entry per worker group, in the RayCluster
	// spec's order.
	WorkerGroups []WorkerGroupNodes `json:"worker_groups"`
}
