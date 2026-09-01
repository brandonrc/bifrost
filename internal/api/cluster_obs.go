// Per-cluster observability endpoints for the cluster drill-down tabs
// (Milestone C):
//
//   - GET /api/v1/clusters/{id}/nodes (api-v1.md §5.3) — the head +
//     per-worker-group node breakdown, read from Kubernetes (the RayCluster
//     and the pods KubeRay owns), NOT the Ray dashboard, so it answers even
//     when the dashboard is unreachable.
//   - GET /api/v1/clusters/{id}/jobs (api-v1.md §5.6) — the browser-
//     consumable, path-based proxy to the cluster's Ray Job Submission API
//     (GET /api/jobs/). Same southbound discipline as the federating
//     gateway: the outbound request is built from scratch (no inbound
//     header, so the caller's JWT never leaks southbound) and the only
//     credential injected is the cluster's static Ray token.
//   - GET /api/v1/clusters/{id}/events (api-v1.md §5.8) — Kubernetes Events.
//   - GET /api/v1/clusters/{id}/metrics — a normalized resource-usage
//     summary distilled from the Ray dashboard's state API + autoscaler
//     status.
//   - GET /api/v1/clusters/{id}/logs (api-v1.md §5.6, non-streaming first
//     cut) — tail-capped pod logs.
//
// Every route requires the same read-scoped authorization as the other
// cluster reads (#49): a developer sees only their project's clusters,
// Admin sees all; an out-of-scope cluster is 404 (never leaks existence).
// Ported from mobula-api's cluster_obs.rs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// Southbound proxy limits (cluster_obs.rs's CONNECT_TIMEOUT/READ_TIMEOUT
// live on server.go; the rest mirror cluster_obs.rs's module constants).
const (
	maxJobsBodyBytes   = 8 * 1024 * 1024
	maxStatusBodyBytes = 4 * 1024 * 1024
	maxObsInflight     = 64
	defaultLogTail     = 200
	maxLogTail         = 5000
)

// obsHTTPClient returns s.ObsHTTPClient when set (tests point this at an
// httptest server), else a default client with cluster_obs.rs's timeouts
// and no redirect-following (a 3xx Location would carry internal service
// names/IPs — same posture as the gateway).
func (s *Server) obsHTTPClient() *http.Client {
	if s.ObsHTTPClient != nil {
		return s.ObsHTTPClient
	}
	dialer := &net.Dialer{Timeout: obsConnectTimeout}
	return &http.Client{
		Timeout: obsReadTimeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		// Redirects are never followed southbound (same posture as the
		// gateway): a 3xx Location would carry internal service names/IPs.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// acquireObsSlot bounds concurrent southbound jobs/metrics proxies
// (#30/#31): excess requests are refused with 503 rather than piling up.
// Returns a release func on success, or ok=false when the semaphore is
// full.
func (s *Server) acquireObsSlot() (release func(), ok bool) {
	s.obsMu.Lock()
	if s.obsInflight == nil {
		s.obsInflight = make(chan struct{}, maxObsInflight)
	}
	ch := s.obsInflight
	s.obsMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

var errGatewayBusy = HTTPError{Status: http.StatusServiceUnavailable, Code: "service_unavailable", Message: "gateway busy: too many inflight proxied requests"}

func serviceUnavailable(msg string) error {
	return HTTPError{Status: http.StatusServiceUnavailable, Code: "service_unavailable", Message: msg}
}

// clusterScope is the visibility a caller has to a cluster for a read:
// either a store-backed cluster with a project (scoping applies) or a
// registry-only cluster (no project — only global reads see it). Ported
// from cluster_obs.rs's ClusterScope.
type clusterScope struct {
	project    string
	registered bool
}

// scopeForRead resolves a cluster for a read, applying read-scoping (#49):
// a caller narrowed by project-scoped assignments gets 404 (not 403) for a
// cluster outside their projects — the list hides it, so a by-name read
// must not leak its existence. Ported from cluster_obs.rs's
// scope_for_read.
func (s *Server) scopeForRead(ctx context.Context, identity *auth.Identity, id core.ClusterId) (clusterScope, error) {
	c, err := s.Store.Get(ctx, id)
	if err != nil {
		return clusterScope{}, wrapStoreErr(err)
	}
	if c != nil {
		_, narrowed := readScope(ctx, s.Store, identity)
		if len(narrowed) > 0 && !containsString(narrowed, c.Spec.Project) {
			return clusterScope{}, notFound("no such cluster")
		}
		return clusterScope{project: c.Spec.Project}, nil
	}
	// Not in the store: only an externally-registered cluster can be read
	// here. A project-narrowed caller can't see a cluster with no project,
	// so it 404s exactly as a hidden one would.
	if s.Registry == nil {
		return clusterScope{}, notFound("no such cluster")
	}
	if _, ok := s.Registry.ByID(id); !ok {
		return clusterScope{}, notFound("no such cluster")
	}
	_, narrowed := readScope(ctx, s.Store, identity)
	if len(narrowed) > 0 {
		return clusterScope{}, notFound("no such cluster")
	}
	return clusterScope{registered: true}, nil
}

// authorizeClusterRead is the shared read-authorization step every route in
// this file needs after scopeForRead: scoped for a project-owned cluster,
// unscoped (global) for a registry-only one — ported from cluster_obs.rs's
// deny_cluster_read (folded into cluster_nodes/cluster_jobs there too).
func (s *Server) authorizeClusterRead(ctx context.Context, identity *auth.Identity, scope clusterScope, action auth.PermissionType, target auth.Target) error {
	if scope.registered {
		return Authorize(ctx, s.Store, identity, action, target)
	}
	return AuthorizeScoped(ctx, s.Store, identity, action, target, scope.project)
}

// provisionNotFound reports whether err is a provision.ProvisionError
// naming a missing cluster.
func provisionNotFound(err error) bool {
	var perr provision.ProvisionError
	return errors.As(err, &perr) && perr.Kind == provision.ProvisionErrNotFound
}

// --- core -> wire conversions ---
//
// internal/core's ClusterNodes/ClusterEvents/ClusterLogs (and their nested
// NodeView/WorkerGroupNodes/ClusterEvent) are structurally near-identical
// to the generated wire types of the same names, but Go only allows a
// blind struct conversion when every nested named field type is IDENTICAL
// across both sides, not merely structurally equal — and here it isn't
// (core.NodeView.MemoryBytes is *uint64 vs the wire's *int64,
// core.ClusterEvent.Count is uint32 vs the wire's int32, etc). These
// convert field-by-field instead.

func wireNodeView(n *core.NodeView) *NodeView {
	if n == nil {
		return nil
	}
	var mem *int64
	if n.MemoryBytes != nil {
		v := int64(*n.MemoryBytes)
		mem = &v
	}
	return &NodeView{
		PodName: n.PodName, Group: n.Group, IsHead: n.IsHead, Phase: n.Phase, Ready: n.Ready,
		NodeIp: n.NodeIp, Host: n.Host, Cpu: n.Cpu, MemoryBytes: mem, Gpu: n.Gpu,
	}
}

func wireWorkerGroupNodes(g *core.WorkerGroupNodes) WorkerGroupNodes {
	nodes := make([]NodeView, len(g.Nodes))
	for i := range g.Nodes {
		nodes[i] = *wireNodeView(&g.Nodes[i])
	}
	return WorkerGroupNodes{Name: g.Name, Desired: int32(g.Desired), Ready: int32(g.Ready), Nodes: nodes}
}

func wireClusterNodes(c *core.ClusterNodes) ClusterNodes {
	groups := make([]WorkerGroupNodes, len(c.WorkerGroups))
	for i := range c.WorkerGroups {
		groups[i] = wireWorkerGroupNodes(&c.WorkerGroups[i])
	}
	return ClusterNodes{ClusterId: c.ClusterId, Head: wireNodeView(c.Head), WorkerGroups: groups}
}

func wireClusterEvent(e *core.ClusterEvent) ClusterEvent {
	return ClusterEvent{
		Count: int32(e.Count), FirstSeen: e.FirstSeen, LastSeen: e.LastSeen,
		Message: e.Message, Object: e.Object, Reason: e.Reason, Type: e.EventType,
	}
}

func wireClusterEvents(c *core.ClusterEvents) ClusterEvents {
	events := make([]ClusterEvent, len(c.Events))
	for i := range c.Events {
		events[i] = wireClusterEvent(&c.Events[i])
	}
	return ClusterEvents{ClusterId: c.ClusterId, Events: events}
}

func wireClusterLogs(c *core.ClusterLogs) ClusterLogs {
	return ClusterLogs{
		ClusterId: c.ClusterId, Pods: c.Pods, Pod: c.Pod,
		Tail: int32(c.Tail), Lines: c.Lines, Truncated: c.Truncated,
	}
}

// ---------------------------------------------------------------------------
// Nodes (§5.3)
// ---------------------------------------------------------------------------

// ClusterNodes returns the head + per-worker-group node breakdown, sourced
// from Kubernetes. Read on Target::Cluster (Viewer+).
func (s *Server) ClusterNodes(ctx context.Context, req ClusterNodesRequestObject) (ClusterNodesResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	id := core.ClusterId(req.Id)
	scope, err := s.scopeForRead(ctx, identity, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeClusterRead(ctx, identity, scope, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}
	if s.Provisioner == nil {
		return nil, notFound("nodes unavailable")
	}
	nodes, err := s.Provisioner.ClusterNodes(ctx, id)
	switch {
	case err == nil && nodes != nil:
		return ClusterNodes200JSONResponse(wireClusterNodes(nodes)), nil
	case err == nil:
		return nil, notFound("nodes unavailable")
	case provisionNotFound(err):
		return nil, notFound("no such cluster")
	default:
		slog.Warn("api: node breakdown backend error", "cluster", id, "error", err)
		return nil, serviceUnavailable("node source unavailable")
	}
}

// ---------------------------------------------------------------------------
// Events (§5.8)
// ---------------------------------------------------------------------------

// ClusterEvents returns Kubernetes Events for the cluster's objects. Read
// on Target::Cluster.
func (s *Server) ClusterEvents(ctx context.Context, req ClusterEventsRequestObject) (ClusterEventsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	id := core.ClusterId(req.Id)
	scope, err := s.scopeForRead(ctx, identity, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeClusterRead(ctx, identity, scope, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}
	if s.Provisioner == nil {
		return nil, notFound("events unavailable")
	}
	events, err := s.Provisioner.ClusterEvents(ctx, id)
	switch {
	case err == nil && events != nil:
		return ClusterEvents200JSONResponse(wireClusterEvents(events)), nil
	case err == nil:
		return nil, notFound("events unavailable")
	case provisionNotFound(err):
		return nil, notFound("no such cluster")
	default:
		slog.Warn("api: events backend error", "cluster", id, "error", err)
		return nil, serviceUnavailable("event source unavailable")
	}
}

// ---------------------------------------------------------------------------
// Logs (§5.6, non-streaming first cut)
// ---------------------------------------------------------------------------

// ClusterLogs returns tail-capped pod logs for the cluster. Read on
// Target::Cluster.
func (s *Server) ClusterLogs(ctx context.Context, req ClusterLogsRequestObject) (ClusterLogsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	id := core.ClusterId(req.Id)
	scope, err := s.scopeForRead(ctx, identity, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeClusterRead(ctx, identity, scope, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}

	tail := defaultLogTail
	if req.Params.Tail != nil {
		tail = *req.Params.Tail
	}
	if tail < 1 {
		tail = 1
	}
	if tail > maxLogTail {
		tail = maxLogTail
	}

	if s.Provisioner == nil {
		return nil, notFound("logs unavailable")
	}
	logs, err := s.Provisioner.ClusterLogs(ctx, id, req.Params.Node, uint32(tail))
	switch {
	case err == nil && logs != nil:
		return ClusterLogs200JSONResponse(wireClusterLogs(logs)), nil
	case err == nil:
		// A named pod that is not part of this cluster (or no log source).
		return nil, notFound("no such pod for this cluster")
	case provisionNotFound(err):
		return nil, notFound("no such cluster")
	default:
		slog.Warn("api: logs backend error", "cluster", id, "error", err)
		return nil, serviceUnavailable("log source unavailable")
	}
}

// ---------------------------------------------------------------------------
// Jobs proxy (§5.6)
// ---------------------------------------------------------------------------

// dashboardBaseAndToken resolves the southbound base URL + token exactly
// as cluster_obs.rs's jobs/metrics handlers do: a registered cluster's
// from the registry (explicit token), else a lifecycle-managed cluster's
// head-service dashboard from the provisioner (no token — the in-cluster
// Ray dashboard is reached over the tenant network).
func (s *Server) dashboardBaseAndToken(id core.ClusterId) (base string, token *string, ok bool) {
	if s.Registry != nil {
		if ep, found := s.Registry.ByID(id); found {
			return ep.ApiBaseUrl, ep.AuthToken, true
		}
	}
	if s.Provisioner != nil {
		if b, found := s.Provisioner.DashboardApiBase(id); found {
			return b, nil, true
		}
	}
	return "", nil, false
}

// clusterJobsUpstreamResponse passes a non-success upstream Ray Job API
// response through verbatim (status + body) — Ray's own error is not
// re-wrapped (api-v1.md §2.6). A custom ResponseObject implementation
// (rather than one of the generated 401/403/404/503 variants) is the only
// way to carry an arbitrary upstream status/body through the strict-server
// layer without touching the generated file.
type clusterJobsUpstreamResponse struct {
	status int
	body   []byte
}

func (r clusterJobsUpstreamResponse) VisitClusterJobsResponse(w http.ResponseWriter) error {
	w.WriteHeader(r.status)
	_, err := w.Write(r.body)
	return err
}

// RayJobSummary field readers (normalizeJobs): pull an optional string or
// unix-millis integer out of a generically-decoded JSON object.
func jobStr(m map[string]any, key string) *string {
	if v, ok := m[key].(string); ok {
		return &v
	}
	return nil
}

func jobMillis(m map[string]any, key string) *int64 {
	if v, ok := m[key].(float64); ok {
		iv := int64(v)
		return &iv
	}
	return nil
}

// normalizeJobs normalizes the Ray GET /api/jobs/ body into a stable list.
// Ray returns a JSON array of job records; some older versions return an
// object keyed by submission id. Both are accepted; anything else yields
// an empty list. Ported from cluster_obs.rs's normalize_jobs.
func normalizeJobs(raw any) []RayJobSummary {
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case map[string]any:
		for _, val := range v {
			items = append(items, val)
		}
	}
	out := make([]RayJobSummary, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		out = append(out, RayJobSummary{
			JobId:        jobStr(m, "job_id"),
			SubmissionId: jobStr(m, "submission_id"),
			Status:       jobStr(m, "status"),
			Entrypoint:   jobStr(m, "entrypoint"),
			StartTime:    jobMillis(m, "start_time"),
			EndTime:      jobMillis(m, "end_time"),
			Message:      jobStr(m, "message"),
		})
	}
	return out
}

// readCapped reads r up to maxBytes+1, reporting ok=false when the cap
// would be (or was) exceeded.
func readCapped(r io.Reader, maxBytes int64) (body []byte, ok bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > maxBytes {
		return body[:maxBytes], false, nil
	}
	return body, true, nil
}

// ClusterJobs proxies the cluster's Ray Job Submission API
// (GET /api/jobs/), normalized to a stable shape. Read on Target::Job.
func (s *Server) ClusterJobs(ctx context.Context, req ClusterJobsRequestObject) (ClusterJobsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	id := core.ClusterId(req.Id)
	scope, err := s.scopeForRead(ctx, identity, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeClusterRead(ctx, identity, scope, auth.Read, auth.TargetJob); err != nil {
		return nil, err
	}

	// Multi-engine: the batch job gateway is a Ray-only surface.
	if c, gerr := s.Store.Get(ctx, id); gerr == nil && c != nil && c.Spec.Engine == core.EngineDask {
		return nil, badRequest("job submission is not supported for engine=dask (batch is a Ray-only surface)")
	}

	base, token, ok := s.dashboardBaseAndToken(id)
	if !ok {
		return nil, notFound("jobs unavailable")
	}
	url := strings.TrimRight(base, "/") + "/api/jobs/"

	release, ok := s.acquireObsSlot()
	if !ok {
		return nil, errGatewayBusy
	}
	defer release()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if token != nil {
		httpReq.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := s.obsHTTPClient().Do(httpReq)
	if err != nil {
		slog.Warn("api: jobs upstream error", "cluster", id, "error", err)
		return nil, serviceUnavailable("cluster unreachable")
	}
	defer func() { _ = resp.Body.Close() }()

	body, within, err := readCapped(resp.Body, maxJobsBodyBytes)
	if err != nil {
		slog.Warn("api: jobs stream error", "cluster", id, "error", err)
		return nil, serviceUnavailable("cluster unreachable")
	}
	if !within {
		slog.Warn("api: jobs response exceeded the size cap", "cluster", id, "cap", maxJobsBodyBytes)
		return nil, serviceUnavailable("jobs response too large")
	}

	// A non-success upstream status is Ray's own answer — pass it through
	// with its body, rather than pretending an empty list.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return clusterJobsUpstreamResponse{status: resp.StatusCode, body: body}, nil
	}

	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		slog.Warn("api: jobs response was not valid JSON", "cluster", id, "error", err)
		return nil, serviceUnavailable("invalid jobs response")
	}
	return ClusterJobs200JSONResponse(normalizeJobs(raw)), nil
}

// ---------------------------------------------------------------------------
// Metrics (§5.x resource summary)
// ---------------------------------------------------------------------------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func numOr(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// stateNodesSummary is one resource-capacity summary computed from the Ray
// state API's /api/v0/nodes response. Ported from cluster_obs.rs's
// StateNodesSummary/summarize_state_nodes.
type stateNodesSummary struct {
	cpu, gpu, memory, objectStoreMemory float64
	haveCPU, haveGPU, haveMem, haveOSS  bool
	activeNodes, deadNodes              uint64
}

func stateNodeRows(raw any) []any {
	m := asMap(raw)
	data := asMap(m["data"])
	result := asMap(data["result"])
	return asSlice(result["result"])
}

func summarizeStateNodes(raw any) stateNodesSummary {
	var sum stateNodesSummary
	for _, n := range stateNodeRows(raw) {
		node := asMap(n)
		alive, _ := node["state"].(string)
		if alive == "ALIVE" {
			sum.activeNodes++
		} else {
			sum.deadNodes++
			continue // only ALIVE nodes contribute capacity
		}
		res := asMap(node["resources_total"])
		if v, ok := numOr(res["CPU"]); ok {
			sum.cpu += v
			sum.haveCPU = true
		}
		if v, ok := numOr(res["GPU"]); ok {
			sum.gpu += v
			sum.haveGPU = true
		}
		if v, ok := numOr(res["memory"]); ok {
			sum.memory += v
			sum.haveMem = true
		}
		if v, ok := numOr(res["object_store_memory"]); ok {
			sum.objectStoreMemory += v
			sum.haveOSS = true
		}
	}
	return sum
}

// usageUsed reads the used amount for key from an autoscaler
// loadMetricsReport.usage map ({ key: [used, total] }). nil when absent —
// the common case on a non-autoscaling cluster.
func usageUsed(usage map[string]any, key string) *float64 {
	if usage == nil {
		return nil
	}
	arr := asSlice(usage[key])
	if len(arr) == 0 {
		return nil
	}
	f, ok := numOr(arr[0])
	if !ok {
		return nil
	}
	return &f
}

// summarizeMetrics builds the normalized resource summary from the
// state-API node list (capacity + node counts, always available on a live
// Ray) and, when present, the autoscaler's load-metrics usage map (for the
// used half of each stat). Ported from cluster_obs.rs's summarize_metrics.
func summarizeMetrics(clusterID string, nodesRaw, statusRaw any) ClusterMetrics {
	sum := summarizeStateNodes(nodesRaw)

	var usage map[string]any
	if statusRaw != nil {
		status := asMap(statusRaw)
		cs := asMap(status["data"])
		clusterStatus := asMap(cs["clusterStatus"])
		if len(clusterStatus) == 0 {
			clusterStatus = asMap(status["clusterStatus"])
		}
		report := asMap(clusterStatus["loadMetricsReport"])
		usage = asMap(report["usage"])
	}
	stat := func(total float64, have bool, key string) *ResourceStat {
		if !have {
			return nil
		}
		return &ResourceStat{Used: usageUsed(usage, key), Total: total}
	}
	active := int64(sum.activeNodes)
	var failed *int64
	if sum.deadNodes > 0 {
		v := int64(sum.deadNodes)
		failed = &v
	}
	return ClusterMetrics{
		ClusterId:         clusterID,
		Cpu:               stat(sum.cpu, sum.haveCPU, "CPU"),
		Gpu:               stat(sum.gpu, sum.haveGPU, "GPU"),
		Memory:            stat(sum.memory, sum.haveMem, "memory"),
		ObjectStoreMemory: stat(sum.objectStoreMemory, sum.haveOSS, "object_store_memory"),
		ActiveNodes:       &active,
		FailedNodes:       failed,
		PendingNodes:      nil, // the state API does not surface pending pods
	}
}

// fetchDashboardJSON fetches and JSON-decodes a southbound dashboard
// endpoint with the shared body cap. Returns an error on any transport /
// status / parse failure — the caller decides whether that is a hard 503
// (the primary source) or a soft skip (the best-effort usage enrichment).
func (s *Server) fetchDashboardJSON(ctx context.Context, url string, token *string, id core.ClusterId) (any, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != nil {
		httpReq.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := s.obsHTTPClient().Do(httpReq)
	if err != nil {
		slog.Warn("api: metrics upstream error", "cluster", id, "error", err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("api: metrics upstream non-success", "cluster", id, "status", resp.StatusCode)
		return nil, errUpstreamNonSuccess
	}
	body, within, err := readCapped(resp.Body, maxStatusBodyBytes)
	if err != nil {
		slog.Warn("api: metrics stream error", "cluster", id, "error", err)
		return nil, err
	}
	if !within {
		slog.Warn("api: metrics response exceeded the size cap", "cluster", id, "cap", maxStatusBodyBytes)
		return nil, errBodyTooLarge
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		slog.Warn("api: metrics response was not valid JSON", "cluster", id, "error", err)
		return nil, err
	}
	return v, nil
}

var (
	errUpstreamNonSuccess = errors.New("metrics upstream non-success")
	errBodyTooLarge       = errors.New("metrics response too large")
)

// ClusterMetrics returns a normalized cluster resource-usage summary,
// distilled from the Ray state API (primary, required) and the
// autoscaler's cluster_status (best-effort enrichment). Read on
// Target::Cluster.
func (s *Server) ClusterMetrics(ctx context.Context, req ClusterMetricsRequestObject) (ClusterMetricsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	id := core.ClusterId(req.Id)
	scope, err := s.scopeForRead(ctx, identity, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeClusterRead(ctx, identity, scope, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}

	base, token, ok := s.dashboardBaseAndToken(id)
	if !ok {
		return nil, notFound("metrics unavailable")
	}
	base = strings.TrimRight(base, "/")

	release, ok := s.acquireObsSlot()
	if !ok {
		return nil, errGatewayBusy
	}
	defer release()

	nodesRaw, err := s.fetchDashboardJSON(ctx, base+"/api/v0/nodes", token, id)
	if err != nil {
		return nil, serviceUnavailable("cluster unreachable")
	}

	// Best-effort enrichment: a failure here is NOT fatal.
	statusRaw, _ := s.fetchDashboardJSON(ctx, base+"/api/cluster_status", token, id)

	return ClusterMetrics200JSONResponse(summarizeMetrics(id.String(), nodesRaw, statusRaw)), nil
}
