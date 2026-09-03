package controller

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// The reconciler depends on Registrar, not on the registry type; this
// pins the contract that the real registry keeps satisfying it and that
// the controller-facing verbs reach the dynamic table.
func TestClusterRegistrySatisfiesRegistrar(t *testing.T) {
	var reg Registrar = &core.ClusterRegistry{}
	ep := core.ClusterEndpoint{Id: "job-1", Hostname: "job-1.ray.kind.invalid", ApiBaseUrl: "http://job-1-head-svc:8265"}
	if err := reg.Register(ep); err != nil {
		t.Fatalf("register: %v", err)
	}
	registry, ok := reg.(*core.ClusterRegistry)
	if !ok {
		t.Fatal("registrar is not the registry")
	}
	if got, ok := registry.ByID("job-1"); !ok || got.Source != core.RegistrySourceDynamic {
		t.Fatalf("registered entry not visible: %+v %v", got, ok)
	}
	reg.Deregister("job-1")
	reg.Deregister("job-1") // idempotent
	if _, ok := registry.ByID("job-1"); ok {
		t.Fatal("deregistered entry still resolves")
	}
}

// Options carries the requirement-5 seams through to the reconciler
// untouched (accepted and stored; nothing acts on them yet).
func TestOptionsSeamsAreCarriedByRunReconciler(t *testing.T) {
	reg := &core.ClusterRegistry{}
	host := func(id core.ClusterId) string { return string(id) + ".ray.test" }
	jobs := &fakeJobProvisioner{}
	r := newReconcilerFromOptions(NewMemoryStore(), &fakeProvisioner{}, Options{
		Registrar: reg, GatewayHostname: host, JobProvisioner: jobs,
	})
	if r.registrar != reg || r.gatewayHostname == nil || r.gatewayHostname("a") != "a.ray.test" || r.jobProvisioner != jobs {
		t.Fatalf("seams not carried: %+v", r)
	}
	bare := newReconcilerFromOptions(NewMemoryStore(), &fakeProvisioner{}, Options{})
	if bare.registrar != nil || bare.gatewayHostname != nil || bare.jobProvisioner != nil {
		t.Fatalf("zero Options must leave the seams nil: %+v", bare)
	}
}

// fakeJobProvisioner is the smallest JobProvisioner; it exists only so the
// interface has a compile-time implementer inside the controller package
// until the live client lands.
type fakeJobProvisioner struct{}

func (fakeJobProvisioner) ApplyJob(context.Context, core.ClusterId, *core.RayJobSpec, uint64, *provision.QueueAssignment) error {
	return nil
}

func (fakeJobProvisioner) ObserveJob(_ context.Context, id core.ClusterId) (provision.ObservedJob, error) {
	return provision.ObservedJob{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
}

func (fakeJobProvisioner) DeleteJob(context.Context, core.ClusterId) error { return nil }

func (fakeJobProvisioner) ListJobs(context.Context) ([]provision.ObservedJob, error) {
	return []provision.ObservedJob{}, nil
}
