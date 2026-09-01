package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// scriptedProvisioner is a provision.Provisioner with scriptable
// nodes/events/logs responses, for exercising cluster_obs.go's branches
// directly (nil/found/error/not-found).
type scriptedProvisioner struct {
	provision.BaseProvisioner
	nodes    *core.ClusterNodes
	nodesErr error
	events   *core.ClusterEvents
	logs     *core.ClusterLogs
	dashBase string
	dashOK   bool
}

func (p *scriptedProvisioner) ClusterNodes(context.Context, core.ClusterId) (*core.ClusterNodes, error) {
	return p.nodes, p.nodesErr
}
func (p *scriptedProvisioner) ClusterEvents(context.Context, core.ClusterId) (*core.ClusterEvents, error) {
	return p.events, nil
}
func (p *scriptedProvisioner) ClusterLogs(context.Context, core.ClusterId, *string, uint32) (*core.ClusterLogs, error) {
	return p.logs, nil
}
func (p *scriptedProvisioner) DashboardApiBase(core.ClusterId) (string, bool) {
	return p.dashBase, p.dashOK
}

// The remaining provision.Provisioner methods aren't exercised by
// cluster_obs.go (only ClusterNodes/ClusterEvents/ClusterLogs/
// DashboardApiBase are); BaseProvisioner doesn't default these, so they
// need trivial bodies purely to satisfy the interface.
func (p *scriptedProvisioner) Apply(context.Context, core.ClusterId, *core.ClusterSpec, uint64, string, *provision.QueueAssignment) (provision.ApplyResponse, error) {
	return provision.ApplyResponse{}, nil
}
func (p *scriptedProvisioner) Terminate(context.Context, core.ClusterId) error { return nil }
func (p *scriptedProvisioner) Suspend(context.Context, core.ClusterId) error   { return nil }
func (p *scriptedProvisioner) Resume(context.Context, core.ClusterId) error    { return nil }
func (p *scriptedProvisioner) Observe(context.Context, core.ClusterId) (provision.ObservedCluster, error) {
	return provision.ObservedCluster{}, nil
}
func (p *scriptedProvisioner) List(context.Context) ([]provision.ObservedCluster, error) {
	return nil, nil
}

func seedRunningCluster(t *testing.T, s *Server, id, project string) {
	t.Helper()
	if _, err := s.Store.UpsertDesired(context.Background(), core.ClusterId(id), core.ClusterSpec{
		Name: id, Project: project, RayVersion: "2.9.0", Image: "x", HeadCpu: "1", HeadMemory: "1Gi",
	}); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

// --- normalizeJobs: ported 1:1 from cluster_obs.rs's normalize_jobs* tests ---

func TestNormalizeJobs_FromArray(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(`[
		{"job_id":"01000000","submission_id":"raysubmit_abc","status":"SUCCEEDED",
		 "entrypoint":"python train.py","start_time":1755280010000,"end_time":1755281900000,
		 "message":"Job finished successfully."},
		{"submission_id":"raysubmit_def","status":"RUNNING","entrypoint":"serve run app:main","start_time":1755282000000}
	]`), &raw); err != nil {
		t.Fatal(err)
	}
	jobs := normalizeJobs(raw)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].JobId == nil || *jobs[0].JobId != "01000000" {
		t.Errorf("job_id = %v", jobs[0].JobId)
	}
	if jobs[0].SubmissionId == nil || *jobs[0].SubmissionId != "raysubmit_abc" {
		t.Errorf("submission_id = %v", jobs[0].SubmissionId)
	}
	if jobs[0].Status == nil || *jobs[0].Status != "SUCCEEDED" {
		t.Errorf("status = %v", jobs[0].Status)
	}
	if jobs[0].EndTime == nil || *jobs[0].EndTime != 1755281900000 {
		t.Errorf("end_time = %v", jobs[0].EndTime)
	}
	// Running job: no end_time, no job_id yet.
	if jobs[1].JobId != nil {
		t.Errorf("job_id = %v, want nil", jobs[1].JobId)
	}
	if jobs[1].Status == nil || *jobs[1].Status != "RUNNING" {
		t.Errorf("status = %v", jobs[1].Status)
	}
	if jobs[1].EndTime != nil {
		t.Errorf("end_time = %v, want nil", jobs[1].EndTime)
	}
}

func TestNormalizeJobs_FromObjectMap(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(`{"raysubmit_abc":{"submission_id":"raysubmit_abc","status":"FAILED"}}`), &raw); err != nil {
		t.Fatal(err)
	}
	jobs := normalizeJobs(raw)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Status == nil || *jobs[0].Status != "FAILED" {
		t.Errorf("status = %v", jobs[0].Status)
	}
}

func TestNormalizeJobs_ToleratesGarbage(t *testing.T) {
	for _, raw := range []string{`"nope"`, `42`, `[]`} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if jobs := normalizeJobs(v); len(jobs) != 0 {
			t.Errorf("normalizeJobs(%s) = %+v, want empty", raw, jobs)
		}
	}
}

// --- summarizeMetrics: ported from cluster_obs.rs's
// metrics_capacity_from_state_api_no_autoscaler /
// metrics_used_enriched_from_autoscaler_report /
// metrics_counts_dead_nodes_and_tolerates_garbage ---

func stateNodesSample() any {
	var v any
	_ = json.Unmarshal([]byte(`{
		"result": true,
		"data": { "result": { "total": 2, "result": [
			{"state":"ALIVE","is_head_node":false,"resources_total":{"CPU":1.0,"memory":3221225472.0,"object_store_memory":927966412.0,"node:10.1.213.197":1.0}},
			{"state":"ALIVE","is_head_node":true,"resources_total":{"CPU":1.0,"memory":3221225472.0,"object_store_memory":682360012.0,"node:__internal_head__":1.0,"node:10.1.213.216":1.0}}
		]}}
	}`), &v)
	return v
}

func TestSummarizeMetrics_CapacityFromStateAPINoAutoscaler(t *testing.T) {
	m := summarizeMetrics("team-b-scoring", stateNodesSample(), nil)
	if m.ClusterId != "team-b-scoring" {
		t.Errorf("cluster_id = %q", m.ClusterId)
	}
	if m.Cpu == nil || m.Cpu.Used != nil || m.Cpu.Total != 2.0 {
		t.Errorf("cpu = %+v, want total=2 used=nil", m.Cpu)
	}
	if m.Memory == nil || m.Memory.Total != 6442450944.0 {
		t.Errorf("memory = %+v", m.Memory)
	}
	if m.ObjectStoreMemory == nil {
		t.Error("object_store_memory should be present")
	}
	if m.Gpu != nil {
		t.Errorf("gpu = %+v, want nil (no GPU resource reported)", m.Gpu)
	}
	if m.ActiveNodes == nil || *m.ActiveNodes != 2 {
		t.Errorf("active_nodes = %v, want 2", m.ActiveNodes)
	}
	if m.FailedNodes != nil {
		t.Errorf("failed_nodes = %v, want nil", m.FailedNodes)
	}
}

func TestSummarizeMetrics_UsedEnrichedFromAutoscalerReport(t *testing.T) {
	var status any
	if err := json.Unmarshal([]byte(`{"data":{"clusterStatus":{"loadMetricsReport":{"usage":{
		"CPU":[1.5,2.0],"memory":[1000.0,6442450944.0]
	}}}}}`), &status); err != nil {
		t.Fatal(err)
	}
	m := summarizeMetrics("c", stateNodesSample(), status)
	if m.Cpu == nil || m.Cpu.Used == nil || *m.Cpu.Used != 1.5 || m.Cpu.Total != 2.0 {
		t.Errorf("cpu = %+v", m.Cpu)
	}
	if m.Memory == nil || m.Memory.Used == nil || *m.Memory.Used != 1000.0 {
		t.Errorf("memory = %+v", m.Memory)
	}
	if m.ObjectStoreMemory == nil || m.ObjectStoreMemory.Used != nil {
		t.Errorf("object_store_memory = %+v, want used=nil (capacity has no usage entry)", m.ObjectStoreMemory)
	}
}

func TestSummarizeMetrics_CountsDeadNodesAndToleratesGarbage(t *testing.T) {
	var withDead any
	if err := json.Unmarshal([]byte(`{"data":{"result":{"result":[
		{"state":"ALIVE","resources_total":{"CPU":4.0}},
		{"state":"DEAD","resources_total":{"CPU":4.0}}
	]}}}`), &withDead); err != nil {
		t.Fatal(err)
	}
	m := summarizeMetrics("c", withDead, nil)
	if m.Cpu == nil || m.Cpu.Total != 4.0 {
		t.Errorf("cpu = %+v, want total=4 (only ALIVE contributes)", m.Cpu)
	}
	if m.ActiveNodes == nil || *m.ActiveNodes != 1 {
		t.Errorf("active_nodes = %v, want 1", m.ActiveNodes)
	}
	if m.FailedNodes == nil || *m.FailedNodes != 1 {
		t.Errorf("failed_nodes = %v, want 1", m.FailedNodes)
	}

	var empty any
	_ = json.Unmarshal([]byte(`{"result":false}`), &empty)
	m2 := summarizeMetrics("c", empty, nil)
	if m2.Cpu != nil {
		t.Errorf("cpu = %+v, want nil for a well-formed empty summary", m2.Cpu)
	}
	if m2.ActiveNodes == nil || *m2.ActiveNodes != 0 {
		t.Errorf("active_nodes = %v, want 0, never a panic", m2.ActiveNodes)
	}
}

// --- Handler-level branch coverage ---

func TestClusterNodes_NoSuchCluster(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.ClusterNodes(ctxWithIdentity(admin()), ClusterNodesRequestObject{Id: "ghost"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterNodes_ScopedDenialIsNotFound(t *testing.T) {
	s := &Server{Store: newMemStore(t), Provisioner: &fakeProvisioner{}}
	seedRunningCluster(t, s, "c1", "proj-a")
	dev := &auth.Identity{Subject: "dev", ProjectRoles: []auth.RoleScope{{Role: auth.RoleViewer, Scope: "project:proj-b"}}}
	_, err := s.ClusterNodes(ctxWithIdentity(dev), ClusterNodesRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 404 (out-of-scope cluster must not leak existence)")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterNodes_NilProvisionerIsUnavailable(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	seedRunningCluster(t, s, "c1", "proj-a")
	_, err := s.ClusterNodes(ctxWithIdentity(admin()), ClusterNodesRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 404 nodes unavailable")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterNodes_ProvisionErrorNotFound(t *testing.T) {
	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{nodesErr: provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: "c1"}}}
	seedRunningCluster(t, s, "c1", "proj-a")
	_, err := s.ClusterNodes(ctxWithIdentity(admin()), ClusterNodesRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterNodes_Success(t *testing.T) {
	nodes := &core.ClusterNodes{
		ClusterId: "c1",
		Head:      &core.NodeView{PodName: "c1-head", IsHead: true, Phase: "Running", Ready: true},
		WorkerGroups: []core.WorkerGroupNodes{
			{Name: "cpu", Desired: 2, Ready: 1, Nodes: []core.NodeView{{PodName: "c1-cpu-1", Phase: "Running", Ready: true}}},
		},
	}
	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{nodes: nodes}}
	seedRunningCluster(t, s, "c1", "proj-a")
	resp, err := s.ClusterNodes(ctxWithIdentity(admin()), ClusterNodesRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[ClusterNodes200JSONResponse](t, resp)
	if view.ClusterId != "c1" || view.Head == nil || view.Head.PodName != "c1-head" {
		t.Errorf("view = %+v", view)
	}
	if len(view.WorkerGroups) != 1 || view.WorkerGroups[0].Name != "cpu" || view.WorkerGroups[0].Nodes[0].PodName != "c1-cpu-1" {
		t.Errorf("worker_groups = %+v", view.WorkerGroups)
	}
}

func TestClusterEvents_Success(t *testing.T) {
	events := &core.ClusterEvents{ClusterId: "c1", Events: []core.ClusterEvent{{EventType: "Warning", Count: 3}}}
	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{events: events}}
	seedRunningCluster(t, s, "c1", "proj-a")
	resp, err := s.ClusterEvents(ctxWithIdentity(admin()), ClusterEventsRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[ClusterEvents200JSONResponse](t, resp)
	if len(view.Events) != 1 || view.Events[0].Type != "Warning" || view.Events[0].Count != 3 {
		t.Errorf("view = %+v", view)
	}
}

func TestClusterLogs_DefaultAndClampedTail(t *testing.T) {
	logs := &core.ClusterLogs{ClusterId: "c1", Pod: "c1-head", Pods: []string{"c1-head"}, Lines: []string{"a", "b"}, Tail: 2}
	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{logs: logs}}
	seedRunningCluster(t, s, "c1", "proj-a")

	resp, err := s.ClusterLogs(ctxWithIdentity(admin()), ClusterLogsRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[ClusterLogs200JSONResponse](t, resp)

	huge := 999999
	resp2, err := s.ClusterLogs(ctxWithIdentity(admin()), ClusterLogsRequestObject{Id: "c1", Params: ClusterLogsParams{Tail: &huge}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustResponse[ClusterLogs200JSONResponse](t, resp2)
}

func TestClusterLogs_NoSuchPod(t *testing.T) {
	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{logs: nil}}
	seedRunningCluster(t, s, "c1", "proj-a")
	_, err := s.ClusterLogs(ctxWithIdentity(admin()), ClusterLogsRequestObject{Id: "c1", Params: ClusterLogsParams{Node: strPtr("nope")}})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterJobs_DaskEngineRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	dask := core.EngineDask
	if _, err := s.Store.UpsertDesired(context.Background(), "c1", core.ClusterSpec{
		Name: "c1", Project: "proj-a", Engine: dask, RayVersion: "x", Image: "x", HeadCpu: "1", HeadMemory: "1Gi",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := s.ClusterJobs(ctxWithIdentity(admin()), ClusterJobsRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestClusterJobs_UnavailableWithNoBackend(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	seedRunningCluster(t, s, "c1", "proj-a")
	_, err := s.ClusterJobs(ctxWithIdentity(admin()), ClusterJobsRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}

func TestClusterJobs_SuccessNormalizesUpstreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/jobs/" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"submission_id":"raysubmit_x","status":"RUNNING"}]`))
	}))
	defer upstream.Close()

	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{dashBase: upstream.URL, dashOK: true}}
	seedRunningCluster(t, s, "c1", "proj-a")
	resp, err := s.ClusterJobs(ctxWithIdentity(admin()), ClusterJobsRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	jobs := mustResponse[ClusterJobs200JSONResponse](t, resp)
	if len(jobs) != 1 || jobs[0].Status == nil || *jobs[0].Status != "RUNNING" {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestClusterJobs_UpstreamErrorPassesThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer upstream.Close()

	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{dashBase: upstream.URL, dashOK: true}}
	seedRunningCluster(t, s, "c1", "proj-a")
	resp, err := s.ClusterJobs(ctxWithIdentity(admin()), ClusterJobsRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error (should pass through, not error): %v", err)
	}
	passthrough := mustResponse[clusterJobsUpstreamResponse](t, resp)
	if passthrough.status != http.StatusBadGateway || string(passthrough.body) != "upstream exploded" {
		t.Errorf("passthrough = %+v", passthrough)
	}
}

func TestClusterMetrics_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/nodes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"result":{"result":[{"state":"ALIVE","resources_total":{"CPU":2.0}}]}}}`))
		case "/api/cluster_status":
			w.WriteHeader(http.StatusNotFound) // best-effort; failure here is not fatal
		}
	}))
	defer upstream.Close()

	s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{dashBase: upstream.URL, dashOK: true}}
	seedRunningCluster(t, s, "c1", "proj-a")
	resp, err := s.ClusterMetrics(ctxWithIdentity(admin()), ClusterMetricsRequestObject{Id: "c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	view := mustResponse[ClusterMetrics200JSONResponse](t, resp)
	if view.Cpu == nil || view.Cpu.Total != 2.0 {
		t.Errorf("cpu = %+v", view.Cpu)
	}
}

// TestClusterJobs_UpstreamErrorPassesThroughOverHTTP is an end-to-end
// round trip through a real http.Handler so
// clusterJobsUpstreamResponse.VisitClusterJobsResponse actually runs and
// writes the response (status + raw body) — TestClusterJobs_UpstreamErrorPassesThrough
// above only inspects the returned Go value, never a real ResponseWriter.
func TestClusterJobs_UpstreamErrorPassesThroughOverHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer upstream.Close()

	store := newMemStore(t)
	s := &Server{Store: store, Provisioner: &scriptedProvisioner{dashBase: upstream.URL, dashOK: true}}
	seedRunningCluster(t, s, "c1", "proj-a")
	srv := httptest.NewServer(NewHandler(s, HandlerOptions{AllowUnauthenticated: true}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/clusters/c1/jobs")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (passed through verbatim)", resp.StatusCode)
	}
}

func TestClusterMetrics_UnavailableWithNoBackend(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	seedRunningCluster(t, s, "c1", "proj-a")
	_, err := s.ClusterMetrics(ctxWithIdentity(admin()), ClusterMetricsRequestObject{Id: "c1"})
	if err == nil {
		t.Fatal("expected 404")
	}
	mustHTTPError(t, err, 404)
}
