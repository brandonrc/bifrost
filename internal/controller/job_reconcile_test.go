package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// scriptedJobProvisioner is a scripted JobProvisioner: ApplyJob records the
// call and creates the job; ObserveJob answers the scripted observation
// for a created job (NotFound otherwise); DeleteJob removes it.
type scriptedJobProvisioner struct {
	mu       sync.Mutex
	created  map[core.ClusterId]bool
	observed map[core.ClusterId]provision.ObservedJob
	applies  []core.ClusterId
	deletes  []core.ClusterId
	applyErr error
}

var _ provision.JobProvisioner = (*scriptedJobProvisioner)(nil)

func newScriptedJobProvisioner() *scriptedJobProvisioner {
	return &scriptedJobProvisioner{created: map[core.ClusterId]bool{}, observed: map[core.ClusterId]provision.ObservedJob{}}
}

func (p *scriptedJobProvisioner) ApplyJob(_ context.Context, id core.ClusterId, _ *core.RayJobSpec, _ uint64, _ *provision.QueueAssignment) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applies = append(p.applies, id)
	if p.applyErr != nil {
		return p.applyErr
	}
	p.created[id] = true
	return nil
}

func (p *scriptedJobProvisioner) ObserveJob(_ context.Context, id core.ClusterId) (provision.ObservedJob, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.created[id] {
		return provision.ObservedJob{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
	}
	obs, ok := p.observed[id]
	if !ok {
		obs = provision.ObservedJob{ID: id, DeploymentStatus: "Initializing"}
	}
	return obs, nil
}

func (p *scriptedJobProvisioner) DeleteJob(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes = append(p.deletes, id)
	delete(p.created, id)
	delete(p.observed, id)
	return nil
}

func (p *scriptedJobProvisioner) ListJobs(context.Context) ([]provision.ObservedJob, error) {
	return nil, nil
}

func (p *scriptedJobProvisioner) set(id core.ClusterId, obs provision.ObservedJob) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created[id] = true
	p.observed[id] = obs
}

// fakeRegistrar records registrations and deregistrations.
type fakeRegistrar struct {
	mu      sync.Mutex
	entries map[core.ClusterId]core.ClusterEndpoint
	err     error
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{entries: map[core.ClusterId]core.ClusterEndpoint{}}
}

func (f *fakeRegistrar) Register(e core.ClusterEndpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries[e.Id] = e
	return nil
}

func (f *fakeRegistrar) Deregister(id core.ClusterId) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, id)
}

func (f *fakeRegistrar) get(id core.ClusterId) (core.ClusterEndpoint, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	return e, ok
}

func hostnameFor(id core.ClusterId) string { return id.String() + ".gw.test" }

func jobSpec() core.RayJobSpec {
	return core.RayJobSpec{Project: "team-a", Entrypoint: "python -c 1", Image: "rayproject/ray:2.56.0",
		RayVersion: "2.56.0", HeadCpu: "1", HeadMemory: "2Gi"}
}

func newJobHarness(t *testing.T) (*JobReconciler, Store, *scriptedJobProvisioner, *fakeRegistrar) {
	t.Helper()
	store := NewMemoryStore()
	jobs := newScriptedJobProvisioner()
	reg := newFakeRegistrar()
	r := NewJobReconciler(store, jobs).WithRegistrar(reg, hostnameFor)
	return r, store, jobs, reg
}

func mustOneOK(t *testing.T, results []ReconcileResult) Action {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one", results)
	}
	if results[0].Err != nil {
		t.Fatalf("reconcile error: %v", results[0].Err)
	}
	return results[0].Action
}

func TestJobReconcileAppliesOnceThenObserves(t *testing.T) {
	r, store, jobs, _ := newJobHarness(t)
	ctx := context.Background()
	owner := "alice"
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), &owner); err != nil {
		t.Fatal(err)
	}
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1000)); a != ActionApplied {
		t.Fatalf("first pass = %s, want applied", a)
	}
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1001)); a != ActionNoOp {
		t.Fatalf("second pass = %s, want no_op", a)
	}
	if len(jobs.applies) != 1 {
		t.Fatalf("applies = %v, want exactly one", jobs.applies)
	}
	j, err := store.GetRayJob(ctx, "job-1")
	if err != nil || j == nil {
		t.Fatalf("get: %v %v", j, err)
	}
	if j.DeploymentStatus != "Initializing" {
		t.Fatalf("deployment status = %q, want the observation recorded", j.DeploymentStatus)
	}
	rec, err := store.GetIntent(ctx, jobIntentKey("job-1"))
	if err != nil || rec == nil || rec.Status != IntentStatusApplied {
		t.Fatalf("intent = %+v %v, want applied", rec, err)
	}
}

func TestJobReconcileRegistersWhileRunningAndDeregistersWhenDone(t *testing.T) {
	r, store, jobs, reg := newJobHarness(t)
	ctx := context.Background()
	owner := "alice"
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), &owner); err != nil {
		t.Fatal(err)
	}
	mustOneOK(t, r.ReconcileAllAt(ctx, 1000))
	if _, ok := reg.get("job-1"); ok {
		t.Fatal("initializing job registered before it has a dashboard")
	}

	url := "http://job-1-raycluster-x-head-svc.ns.svc.cluster.local:8265"
	cluster := "job-1-raycluster-x"
	start := uint64(1100)
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", JobStatus: "RUNNING", DeploymentStatus: "Running",
		ClusterName: &cluster, DashboardURL: &url, StartTime: &start})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1101))
	e, ok := reg.get("job-1")
	if !ok {
		t.Fatal("running job not registered")
	}
	if e.Hostname != "job-1.gw.test" || e.ApiBaseUrl != url || e.Project != "team-a" ||
		e.Target != core.RegistryTargetJobs || e.Source != core.RegistrySourceDynamic {
		t.Fatalf("endpoint = %+v", e)
	}

	end := uint64(1190)
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", JobStatus: "SUCCEEDED", DeploymentStatus: "Complete",
		ClusterName: &cluster, StartTime: &start, EndTime: &end})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1200))
	if _, ok := reg.get("job-1"); ok {
		t.Fatal("finished job still registered")
	}
	hist, err := store.ListJobs(ctx)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %+v %v, want one record", hist, err)
	}
	h := hist[0]
	if h.Id != "job-1" || h.Cluster != cluster || h.Submitter != "alice" || h.Status != "SUCCEEDED" ||
		h.DurationSecs == nil || *h.DurationSecs != 90 {
		t.Fatalf("history record = %+v", h)
	}
	// A later pass sees the row already terminal: no second history write
	// churn, still no registration, no re-apply.
	mustOneOK(t, r.ReconcileAllAt(ctx, 1300))
	if len(jobs.applies) != 1 {
		t.Fatalf("applies = %v", jobs.applies)
	}
}

func TestJobReconcileFinishedJobWhoseCRVanishedIsNotRerun(t *testing.T) {
	r, store, jobs, _ := newJobHarness(t)
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	mustOneOK(t, r.ReconcileAllAt(ctx, 1000))
	end := uint64(1050)
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", JobStatus: "FAILED", DeploymentStatus: "Failed", EndTime: &end})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1060))
	// The RayJob is swept out of band.
	_ = jobs.DeleteJob(ctx, "job-1")
	jobs.deletes = nil
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1070)); a != ActionNoOp {
		t.Fatalf("action = %s, want no_op", a)
	}
	if len(jobs.applies) != 1 {
		t.Fatalf("a finished job was re-applied: %v", jobs.applies)
	}
	hist, _ := store.ListJobs(ctx)
	if len(hist) != 1 || hist[0].Status != "FAILED" || hist[0].Submitter != "-" {
		t.Fatalf("history = %+v", hist)
	}
}

func TestJobReconcileDeleteTearsDownTombstonesAndReaps(t *testing.T) {
	r, store, jobs, reg := newJobHarness(t)
	r.WithTerminatedRetention(100)
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	mustOneOK(t, r.ReconcileAllAt(ctx, 1000))
	url := "http://h:8265"
	cluster := "job-1-raycluster-x"
	start := uint64(1000)
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", JobStatus: "RUNNING", DeploymentStatus: "Running",
		ClusterName: &cluster, DashboardURL: &url, StartTime: &start})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1001))
	if _, ok := reg.get("job-1"); !ok {
		t.Fatal("not registered while running")
	}

	if err := store.SetRayJobDesired(ctx, "job-1", DesiredTerminated); err != nil {
		t.Fatal(err)
	}
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1500)); a != ActionTerminated {
		t.Fatalf("action = %s, want terminated", a)
	}
	if len(jobs.deletes) != 1 {
		t.Fatalf("deletes = %v", jobs.deletes)
	}
	if _, ok := reg.get("job-1"); ok {
		t.Fatal("deleted job still registered")
	}
	j, _ := store.GetRayJob(ctx, "job-1")
	if j == nil || !RayJobObservedGone(j) || j.Status != "STOPPED" || j.FinishedAt == nil || *j.FinishedAt != 1500 {
		t.Fatalf("tombstone = %+v", j)
	}
	hist, _ := store.ListJobs(ctx)
	if len(hist) != 1 || hist[0].Status != "STOPPED" || hist[0].Cluster != cluster {
		t.Fatalf("history = %+v", hist)
	}

	// Inside the retention window the tombstone stays; past it, it goes.
	if ids, err := r.ReapTerminatedJobs(ctx, 1550); err != nil || len(ids) != 0 {
		t.Fatalf("early reap = %v %v", ids, err)
	}
	ids, err := r.ReapTerminatedJobs(ctx, 1600)
	if err != nil || len(ids) != 1 {
		t.Fatalf("reap = %v %v", ids, err)
	}
	if j, _ := store.GetRayJob(ctx, "job-1"); j != nil {
		t.Fatal("row survived the reap")
	}
}

func TestJobReconcileApplyFailureBacksOff(t *testing.T) {
	r, store, jobs, _ := newJobHarness(t)
	jobs.applyErr = provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "boom"}
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	res := r.ReconcileAllAt(ctx, 1000)
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("results = %+v, want an error", res)
	}
	var rerr ReconcileError
	if !errors.As(res[0].Err, &rerr) || rerr.Kind != ReconcileErrProvision {
		t.Fatalf("err = %v, want a provision error", res[0].Err)
	}
	j, _ := store.GetRayJob(ctx, "job-1")
	if j.FailureCount != 1 || j.NextAttemptAt != 1000+backoffSecs(1) {
		t.Fatalf("backoff state = %d/%d", j.FailureCount, j.NextAttemptAt)
	}
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1001)); a != ActionBackoff {
		t.Fatalf("inside the window = %s, want backoff", a)
	}
	jobs.applyErr = nil
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1000+backoffSecs(1))); a != ActionApplied {
		t.Fatalf("after the window = %s, want applied", a)
	}
	j, _ = store.GetRayJob(ctx, "job-1")
	if j.FailureCount != 0 || j.NextAttemptAt != 0 {
		t.Fatalf("backoff not cleared: %d/%d", j.FailureCount, j.NextAttemptAt)
	}
}

func TestJobReconcileRegistrationRefusalIsNotFatal(t *testing.T) {
	r, store, jobs, reg := newJobHarness(t)
	reg.err = errors.New("static hostname collision")
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	url := "http://h:8265"
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", JobStatus: "RUNNING", DeploymentStatus: "Running", DashboardURL: &url})
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1000)); a != ActionNoOp {
		t.Fatalf("action = %s", a)
	}
}

func TestJobIntentKeyAndFingerprint(t *testing.T) {
	if k := jobIntentKey("job-1"); !strings.HasPrefix(k, "job/job-1/") {
		t.Fatalf("key = %q", k)
	}
	a, b := jobSpec(), jobSpec()
	b.Entrypoint = "python -c 2"
	if jobParamsFingerprint(&a) == jobParamsFingerprint(&b) {
		t.Fatal("fingerprint ignores the entrypoint")
	}
}

// --- cluster reconciler registrar hook ---

func TestClusterReconcileRegistersRoutableClustersAndDeregistersDeadOnes(t *testing.T) {
	store := NewMemoryStore()
	prov := &fakeProvisioner{}
	reg := newFakeRegistrar()
	r := newReconcilerFromOptions(store, prov, Options{Registrar: reg, GatewayHostname: hostnameFor})
	ctx := context.Background()
	spec := core.ClusterSpec{Name: "c1", Project: "team-a", Engine: core.EngineRay, RayVersion: "2.56.0",
		Image: "rayproject/ray:2.56.0", HeadCpu: "1", HeadMemory: "2Gi"}
	if _, err := store.UpsertDesired(ctx, "c1", spec); err != nil {
		t.Fatal(err)
	}
	url := "http://c1-head-svc.ns.svc:8265"
	state := core.ClusterStateRunning
	gen := uint64(1)
	prov.observeFn = func(int) (provision.ObservedCluster, error) {
		return provision.ObservedCluster{ID: "c1", State: state, ObservedGeneration: &gen, ApiBaseUrl: &url}, nil
	}
	if res := r.ReconcileAllAt(ctx, 1000); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	e, ok := reg.get("c1")
	if !ok || e.Hostname != "c1.gw.test" || e.ApiBaseUrl != url || e.Project != "team-a" || e.Target != core.RegistryTargetJobs {
		t.Fatalf("registration = %+v %v", e, ok)
	}

	state = core.ClusterStateTerminated
	if res := r.ReconcileAllAt(ctx, 1001); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if _, ok := reg.get("c1"); ok {
		t.Fatal("terminated cluster still registered")
	}
}

func TestClusterIsRoutableMatrix(t *testing.T) {
	want := map[core.ClusterState]bool{
		core.ClusterStatePending: false, core.ClusterStateProvisioning: true, core.ClusterStateRunning: true,
		core.ClusterStateDegraded: true, core.ClusterStateUpdating: true, core.ClusterStateSuspending: false,
		core.ClusterStateSuspended: false, core.ClusterStateTerminating: false, core.ClusterStateTerminated: false,
	}
	for s, w := range want {
		if got := clusterIsRoutable(s); got != w {
			t.Errorf("%s = %v, want %v", s, got, w)
		}
	}
}

func TestJobReconcileQuarantineObservesOnly(t *testing.T) {
	r, store, jobs, _ := newJobHarness(t)
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetQuarantine(ctx, true); err != nil {
		t.Fatal(err)
	}
	if a := mustOneOK(t, r.ReconcileAllAt(ctx, 1000)); a != ActionNoOp || len(jobs.applies) != 0 {
		t.Fatalf("quarantined pass = %s applies=%v, want no actuation", a, jobs.applies)
	}
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", DeploymentStatus: "Initializing"})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1001))
	if j, _ := store.GetRayJob(ctx, "job-1"); j.DeploymentStatus != "Initializing" {
		t.Fatalf("quarantine must still record observations: %+v", j)
	}
}

func TestJobReconcileRecordsAFailureKubeRayDeclaredWithoutRay(t *testing.T) {
	r, store, jobs, _ := newJobHarness(t)
	ctx := context.Background()
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	msg := "cluster never became ready"
	jobs.set("job-1", provision.ObservedJob{ID: "job-1", DeploymentStatus: "Failed", Message: &msg})
	mustOneOK(t, r.ReconcileAllAt(ctx, 1000))
	hist, _ := store.ListJobs(ctx)
	if len(hist) != 1 || hist[0].Status != "FAILED" || hist[0].DurationSecs != nil {
		t.Fatalf("history = %+v, want FAILED derived from the deployment status", hist)
	}
	for _, c := range []struct {
		obs  provision.ObservedJob
		want string
	}{
		{provision.ObservedJob{JobStatus: "STOPPED"}, "STOPPED"},
		{provision.ObservedJob{DeploymentStatus: "Complete"}, "SUCCEEDED"},
		{provision.ObservedJob{DeploymentStatus: "ValidationFailed"}, "FAILED"},
		{provision.ObservedJob{DeploymentStatus: "Running"}, ""},
	} {
		if got := historyStatus(&c.obs); got != c.want {
			t.Errorf("historyStatus(%+v) = %q, want %q", c.obs, got, c.want)
		}
	}
}

func TestRunJobReconcilerRunsAPassAndStopsOnCancel(t *testing.T) {
	store := NewMemoryStore()
	jobs := newScriptedJobProvisioner()
	reg := newFakeRegistrar()
	ctx, cancel := context.WithCancel(context.Background())
	if err := store.UpsertRayJob(ctx, "job-1", jobSpec(), nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		retention := uint64(10)
		done <- RunJobReconciler(ctx, store, jobs, Options{Interval: time.Millisecond, Registrar: reg, GatewayHostname: hostnameFor, TerminatedRetentionSecs: &retention})
	}()
	deadline := time.After(5 * time.Second)
	for {
		jobs.mu.Lock()
		n := len(jobs.applies)
		jobs.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the loop never applied the job")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
	if r := NewJobReconciler(store, jobs); len(r.ReconcileAll(context.Background())) != 1 {
		t.Fatal("ReconcileAll did not visit the job")
	}
}
