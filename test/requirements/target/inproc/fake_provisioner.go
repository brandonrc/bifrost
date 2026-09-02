package inproc

import (
	"context"
	"sync"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// fakeProvisioner is the Kubernetes edge, faked. It converges one step per
// Observe (provisioning -> running) and validates names the way a real API
// server would, so an empty or invalid id fails here on the first
// reconcile tick.
type fakeProvisioner struct {
	provision.BaseProvisioner
	mu       sync.Mutex
	clusters map[core.ClusterId]*fakeCluster
}

type fakeCluster struct {
	generation uint64
	observes   int
	suspended  bool
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{clusters: map[core.ClusterId]*fakeCluster{}}
}

var _ provision.Provisioner = (*fakeProvisioner)(nil)

func (p *fakeProvisioner) Apply(_ context.Context, id core.ClusterId, _ *core.ClusterSpec, generation uint64, _ string, _ *provision.QueueAssignment) (provision.ApplyResponse, error) {
	if !core.IsK8sName(string(id)) {
		return provision.ApplyResponse{}, provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "resource name may not be empty or invalid: " + string(id)}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[id]
	if !ok {
		c = &fakeCluster{}
		p.clusters[id] = c
	}
	c.generation = generation
	url := "http://" + string(id) + "-head-svc:8265"
	return provision.ApplyResponse{Generation: generation, ApiBaseUrl: &url}, nil
}

func (p *fakeProvisioner) Observe(_ context.Context, id core.ClusterId) (provision.ObservedCluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[id]
	if !ok {
		return provision.ObservedCluster{}, provision.ProvisionError{Kind: provision.ProvisionErrNotFound, ClusterID: id}
	}
	c.observes++
	state := core.ClusterStateRunning
	if c.observes == 1 {
		state = core.ClusterStateProvisioning
	}
	if c.suspended {
		state = core.ClusterStateSuspended
	}
	gen := c.generation
	url := "http://" + string(id) + "-head-svc:8265"
	return provision.ObservedCluster{ID: id, State: state, ObservedGeneration: &gen, ApiBaseUrl: &url}, nil
}

func (p *fakeProvisioner) List(_ context.Context) ([]provision.ObservedCluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provision.ObservedCluster, 0, len(p.clusters))
	for id, c := range p.clusters {
		gen := c.generation
		out = append(out, provision.ObservedCluster{ID: id, State: core.ClusterStateRunning, ObservedGeneration: &gen})
	}
	return out, nil
}

func (p *fakeProvisioner) Terminate(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clusters, id)
	return nil
}

func (p *fakeProvisioner) Suspend(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusters[id]; ok {
		c.suspended = true
	}
	return nil
}

func (p *fakeProvisioner) Resume(_ context.Context, id core.ClusterId) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusters[id]; ok {
		c.suspended = false
	}
	return nil
}

func (p *fakeProvisioner) DashboardApiBase(id core.ClusterId) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clusters[id]; !ok {
		return "", false
	}
	return "http://" + string(id) + "-head-svc:8265", true
}

func (p *fakeProvisioner) ClusterNodes(context.Context, core.ClusterId) (*core.ClusterNodes, error) {
	return &core.ClusterNodes{}, nil
}
func (p *fakeProvisioner) ClusterEvents(context.Context, core.ClusterId) (*core.ClusterEvents, error) {
	return &core.ClusterEvents{}, nil
}
func (p *fakeProvisioner) ClusterLogs(context.Context, core.ClusterId, *string, uint32) (*core.ClusterLogs, error) {
	return &core.ClusterLogs{}, nil
}
