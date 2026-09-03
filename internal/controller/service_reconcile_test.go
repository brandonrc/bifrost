package controller

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// scriptedServiceProvisioner is the Kubernetes edge for the service loop,
// faked: Deploy records the applied generation, Get reports the scripted
// state at that generation, Delete removes.
type scriptedServiceProvisioner struct {
	mu        sync.Mutex
	deployed  map[string]uint64
	queues    map[string]*provision.QueueAssignment
	state     map[string]core.ClusterState
	deploys   int
	deletes   int
	deployErr error
	getErr    error
}

func newScriptedServiceProvisioner() *scriptedServiceProvisioner {
	return &scriptedServiceProvisioner{deployed: map[string]uint64{}, queues: map[string]*provision.QueueAssignment{}, state: map[string]core.ClusterState{}}
}

func (p *scriptedServiceProvisioner) Deploy(_ context.Context, name string, _ *core.ServiceSpec, generation uint64, queue *provision.QueueAssignment) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deployErr != nil {
		return p.deployErr
	}
	p.deploys++
	p.deployed[name] = generation
	p.queues[name] = queue
	if _, ok := p.state[name]; !ok {
		p.state[name] = core.ClusterStateProvisioning
	}
	return nil
}

func (p *scriptedServiceProvisioner) Get(_ context.Context, name string) (*provision.ObservedService, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getErr != nil {
		return nil, p.getErr
	}
	gen, ok := p.deployed[name]
	if !ok {
		return nil, nil //nolint:nilnil // not-found is (nil, nil) per the interface
	}
	url := "http://" + name + "-serve-svc.ns.svc:8000"
	return &provision.ObservedService{Name: name, State: p.state[name], Url: &url, Project: "team-a", Generation: &gen}, nil
}

func (p *scriptedServiceProvisioner) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletes++
	delete(p.deployed, name)
	delete(p.state, name)
	return nil
}

func (p *scriptedServiceProvisioner) List(context.Context) ([]provision.ObservedService, error) {
	return nil, nil
}

func (p *scriptedServiceProvisioner) setState(name string, s core.ClusterState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state[name] = s
}

func serviceSpec(name string, replicas uint32) core.ServiceSpec {
	return core.ServiceSpec{
		Name: name, Project: "team-a", RayVersion: "2.56.0", Image: "rayproject/ray:2.56.0",
		ServeConfigV2: "applications: []\n", HeadCpu: "1", HeadMemory: "2Gi",
		WorkerReplicas: replicas, WorkerCpu: "1", WorkerMemory: "2Gi", Upgrade: core.UpgradeStrategyInPlace,
	}
}

func newServiceHarness(t *testing.T) (*ServiceReconciler, Store, *scriptedServiceProvisioner, *core.ClusterRegistry) {
	t.Helper()
	store := NewMemoryStore()
	prov := newScriptedServiceProvisioner()
	reg := &core.ClusterRegistry{}
	r := newServiceReconcilerFromOptions(store, prov, ServiceOptions{
		Registrar:       reg,
		GatewayHostname: func(id core.ClusterId) string { return string(id) + ".ray.test" },
	})
	return r, store, prov, reg
}

func onlyResult(t *testing.T, results []ServiceReconcileResult) ServiceReconcileResult {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly one", results)
	}
	if results[0].Err != nil {
		t.Fatalf("reconcile error: %v", results[0].Err)
	}
	return results[0]
}

// A fresh row is deployed at its generation; the view is provisioning
// until the backend says running, at which point the Serve endpoint is
// registered with the gateway as a `serve` target in the row's project.
func TestServiceReconcileDeploysThenRegistersWhenRunning(t *testing.T) {
	r, store, prov, reg := newServiceHarness(t)
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}

	if res := onlyResult(t, r.ReconcileAllAt(ctx, 1000)); res.Action != ServiceActionDeploy {
		t.Fatalf("first pass action = %s, want deploy", res.Action)
	}
	row, _ := store.GetService(ctx, "svc")
	if row.ObservedState == nil || *row.ObservedState != core.ClusterStateProvisioning {
		t.Fatalf("observed after deploy = %v, want provisioning", row.ObservedState)
	}
	if prov.deployed["svc"] != 1 {
		t.Fatalf("deployed generation = %d, want 1", prov.deployed["svc"])
	}
	if _, ok := reg.ByID("svc"); ok {
		t.Fatal("a provisioning service must not be registered")
	}

	// Same generation, still provisioning: no redeploy.
	if res := onlyResult(t, r.ReconcileAllAt(ctx, 1001)); res.Action != ServiceActionNone {
		t.Fatalf("second pass action = %s, want none", res.Action)
	}
	if prov.deploys != 1 {
		t.Fatalf("deploys = %d, want 1 (no redeploy at the same generation)", prov.deploys)
	}

	prov.setState("svc", core.ClusterStateRunning)
	onlyResult(t, r.ReconcileAllAt(ctx, 1002))
	row, _ = store.GetService(ctx, "svc")
	if row.ObservedState == nil || *row.ObservedState != core.ClusterStateRunning || row.ObservedURL == nil {
		t.Fatalf("observed after running = %v %v", row.ObservedState, row.ObservedURL)
	}
	ep, ok := reg.ByID("svc")
	if !ok {
		t.Fatal("running service not registered")
	}
	if ep.Hostname != "svc.ray.test" || ep.Target != core.RegistryTargetServe || ep.Source != core.RegistrySourceDynamic ||
		ep.Project != "team-a" || ep.ApiBaseUrl != *row.ObservedURL {
		t.Fatalf("registered endpoint = %+v", ep)
	}
}

// A spec change bumps the stored generation; the next pass sees the
// RayService behind, records updating, deregisters the stale endpoint and
// redeploys at the new generation.
func TestServiceReconcileRedeploysWhenGenerationBehind(t *testing.T) {
	r, store, prov, reg := newServiceHarness(t)
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}
	onlyResult(t, r.ReconcileAllAt(ctx, 1000))
	prov.setState("svc", core.ClusterStateRunning)
	onlyResult(t, r.ReconcileAllAt(ctx, 1001))
	if _, ok := reg.ByID("svc"); !ok {
		t.Fatal("precondition: registered")
	}

	gen, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 3), nil)
	if err != nil || gen != 2 {
		t.Fatalf("upsert changed spec: gen=%d err=%v", gen, err)
	}
	if res := onlyResult(t, r.ReconcileAllAt(ctx, 1002)); res.Action != ServiceActionDeploy {
		t.Fatalf("action after spec change = %s, want deploy", res.Action)
	}
	if prov.deployed["svc"] != 2 {
		t.Fatalf("redeployed generation = %d, want 2", prov.deployed["svc"])
	}
	row, _ := store.GetService(ctx, "svc")
	if row.ObservedState == nil || *row.ObservedState != core.ClusterStateUpdating {
		t.Fatalf("observed during redeploy = %v, want updating", row.ObservedState)
	}
	if _, ok := reg.ByID("svc"); ok {
		t.Fatal("an updating service must be deregistered")
	}
	// The backend now reports the new generation as running: re-registered.
	onlyResult(t, r.ReconcileAllAt(ctx, 1003))
	if _, ok := reg.ByID("svc"); !ok {
		t.Fatal("running at the new generation must re-register")
	}
}

// A RayService that carries no generation annotation (created before the
// annotation existed) is treated as behind and re-applied once.
func TestServiceReconcileRedeploysWhenGenerationUnknown(t *testing.T) {
	r, store, prov, _ := newServiceHarness(t)
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}
	onlyResult(t, r.ReconcileAllAt(ctx, 1000))
	prov.mu.Lock()
	prov.deployed["svc"] = 0 // pretend the annotation was stripped
	prov.mu.Unlock()
	res := onlyResult(t, r.ReconcileAllAt(ctx, 1001))
	if res.Action != ServiceActionDeploy || prov.deployed["svc"] != 1 {
		t.Fatalf("action=%s deployed=%d, want deploy at 1", res.Action, prov.deployed["svc"])
	}
}

// Desired terminated: delete the RayService, deregister, show terminating
// until it is gone, then hold a terminated tombstone for the retention
// window and purge it.
func TestServiceReconcileDeletesTombstonesAndReaps(t *testing.T) {
	r, store, prov, reg := newServiceHarness(t)
	retention := uint64(100)
	r.terminatedRetentionSecs = retention
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}
	onlyResult(t, r.ReconcileAllAt(ctx, 1000))
	prov.setState("svc", core.ClusterStateRunning)
	onlyResult(t, r.ReconcileAllAt(ctx, 1001))

	if err := store.SetServiceDesired(ctx, "svc", DesiredTerminated); err != nil {
		t.Fatal(err)
	}
	row, _ := store.GetService(ctx, "svc")
	terminatedAt := *row.TerminatedAt

	if res := onlyResult(t, r.ReconcileAllAt(ctx, terminatedAt+1)); res.Action != ServiceActionDelete {
		t.Fatalf("action = %s, want delete", res.Action)
	}
	if _, ok := reg.ByID("svc"); ok {
		t.Fatal("a terminating service must be deregistered")
	}
	if prov.deletes != 1 {
		t.Fatalf("deletes = %d, want 1", prov.deletes)
	}
	// Gone from the backend: tombstone, not yet purged.
	if res := onlyResult(t, r.ReconcileAllAt(ctx, terminatedAt+2)); res.Action != ServiceActionNone {
		t.Fatalf("action = %s, want none (tombstone)", res.Action)
	}
	row, _ = store.GetService(ctx, "svc")
	if row == nil || row.ObservedState == nil || *row.ObservedState != core.ClusterStateTerminated {
		t.Fatalf("tombstone row = %+v", row)
	}
	// Retention elapsed: purged.
	if res := onlyResult(t, r.ReconcileAllAt(ctx, terminatedAt+retention)); res.Action != ServiceActionReap {
		t.Fatalf("action = %s, want reap", res.Action)
	}
	if row, _ := store.GetService(ctx, "svc"); row != nil {
		t.Fatalf("row survives reap: %+v", row)
	}
}

// A backend failure surfaces as the pass's error and leaves the row's
// observation honest (provisioning) so the next tick retries the deploy.
func TestServiceReconcileDeployErrorIsRetried(t *testing.T) {
	r, store, prov, _ := newServiceHarness(t)
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}
	prov.deployErr = errors.New("apiserver down")
	results := r.ReconcileAllAt(ctx, 1000)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one error", results)
	}
	row, _ := store.GetService(ctx, "svc")
	if row.ObservedState == nil || *row.ObservedState != core.ClusterStateProvisioning {
		t.Fatalf("observed after failed deploy = %v, want provisioning", row.ObservedState)
	}
	prov.deployErr = nil
	if res := onlyResult(t, r.ReconcileAllAt(ctx, 1001)); res.Action != ServiceActionDeploy {
		t.Fatalf("retry action = %s, want deploy", res.Action)
	}
}

// Without the gateway seam the loop still converges; it just registers
// nothing.
func TestServiceReconcileWithoutRegistrar(t *testing.T) {
	store := NewMemoryStore()
	prov := newScriptedServiceProvisioner()
	r := newServiceReconcilerFromOptions(store, prov, ServiceOptions{})
	ctx := context.Background()
	if _, err := store.UpsertService(ctx, "svc", serviceSpec("svc", 1), nil); err != nil {
		t.Fatal(err)
	}
	onlyResult(t, r.ReconcileAllAt(ctx, 1))
	prov.setState("svc", core.ClusterStateRunning)
	onlyResult(t, r.ReconcileAllAt(ctx, 2))
	row, _ := store.GetService(ctx, "svc")
	if row.ObservedState == nil || *row.ObservedState != core.ClusterStateRunning {
		t.Fatalf("observed = %v, want running", row.ObservedState)
	}
	if len(r.registered) != 0 {
		t.Fatalf("registered = %v, want none", r.registered)
	}
}

func TestServiceActionStrings(t *testing.T) {
	for a, want := range map[ServiceAction]string{
		ServiceActionNone: "none", ServiceActionDeploy: "deploy", ServiceActionDelete: "delete",
		ServiceActionReap: "reap", ServiceAction(99): "unknown",
	} {
		if a.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(a), a.String(), want)
		}
	}
}

// Requirement 4: the reconciler hands Deploy the project's serving-pool
// queue — resolved from the store at apply time — and nil when the project
// has only a compute allocation (services never land in compute pools).
func TestServiceReconcileDeploysThroughServingQueue(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	pool := func(name string, purpose core.PoolPurpose) core.PoolSpec {
		return core.PoolSpec{Name: name, Cohort: "c", FairSharingWeight: 1,
			Flavors: []core.FlavorSpec{{Name: "f", Resources: map[string]string{"cpu": "8"}}}, Purpose: purpose}
	}
	if _, err := store.UpsertPool(ctx, "cpu", pool("cpu", core.PoolPurposeCompute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "cpu", Project: "team-a", Namespace: "ns"}); err != nil {
		t.Fatal(err)
	}
	prov := newScriptedServiceProvisioner()
	r := NewServiceReconciler(store, prov)

	spec := core.ServiceSpec{Name: "svc", Project: "team-a", HeadCpu: "1", HeadMemory: "1Gi", WorkerCpu: "1", WorkerMemory: "1Gi", Upgrade: core.UpgradeStrategyInPlace}
	if _, err := store.UpsertService(ctx, "svc", spec, nil); err != nil {
		t.Fatal(err)
	}
	r.ReconcileAllAt(ctx, 1)
	if q := prov.queues["svc"]; q != nil {
		t.Fatalf("compute-only project: Deploy got queue %+v, want nil", q)
	}

	// A serving allocation appears; the next roll carries its queue.
	if _, err := store.UpsertPool(ctx, "serve", pool("serve", core.PoolPurposeServing)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "serve", Project: "team-a", Namespace: "ns"}); err != nil {
		t.Fatal(err)
	}
	spec.WorkerCpu = "2"
	if _, err := store.UpsertService(ctx, "svc", spec, nil); err != nil {
		t.Fatal(err)
	}
	r.ReconcileAllAt(ctx, 2)
	q := prov.queues["svc"]
	if q == nil || q.QueueName != "team-a-serving" {
		t.Fatalf("Deploy queue = %+v, want team-a-serving", q)
	}
}
