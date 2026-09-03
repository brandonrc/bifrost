package inproc

import (
	"context"
	"sync"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// fakeServiceProvisioner is the RayService edge, faked (requirement 1). It
// converges one step per Get — provisioning on the first read after a
// deploy, running with a Serve URL from the second — and validates names
// the way the API server would. A redeploy at a higher generation starts
// the convergence over, as a real RayService rollout would.
type fakeServiceProvisioner struct {
	mu       sync.Mutex
	services map[string]*fakeService
}

type fakeService struct {
	generation uint64
	project    string
	gets       int
}

func newFakeServiceProvisioner() *fakeServiceProvisioner {
	return &fakeServiceProvisioner{services: map[string]*fakeService{}}
}

var _ provision.ServiceProvisioner = (*fakeServiceProvisioner)(nil)

func (p *fakeServiceProvisioner) Deploy(_ context.Context, name string, spec *core.ServiceSpec, generation uint64, _ *provision.QueueAssignment) error {
	if !core.IsK8sName(name) {
		return provision.ProvisionError{Kind: provision.ProvisionErrBackend, Message: "resource name may not be empty or invalid: " + name}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.services[name]
	if !ok {
		s = &fakeService{}
		p.services[name] = s
	}
	if s.generation != generation {
		s.gets = 0
	}
	s.generation = generation
	s.project = spec.Project
	return nil
}

func (p *fakeServiceProvisioner) observed(name string, s *fakeService) provision.ObservedService {
	gen := s.generation
	o := provision.ObservedService{Name: name, State: core.ClusterStateProvisioning, Project: s.project, Generation: &gen}
	if s.gets > 1 {
		o.State = core.ClusterStateRunning
		url := "http://" + name + "-serve-svc.inproc.svc:8000"
		o.Url = &url
	}
	return o
}

func (p *fakeServiceProvisioner) Get(_ context.Context, name string) (*provision.ObservedService, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.services[name]
	if !ok {
		return nil, nil //nolint:nilnil // not-found is (nil, nil) per the interface
	}
	s.gets++
	o := p.observed(name, s)
	return &o, nil
}

func (p *fakeServiceProvisioner) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.services, name)
	return nil
}

func (p *fakeServiceProvisioner) List(_ context.Context) ([]provision.ObservedService, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provision.ObservedService, 0, len(p.services))
	for name, s := range p.services {
		out = append(out, p.observed(name, s))
	}
	return out, nil
}
