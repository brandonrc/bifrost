package req

import (
	"context"
	"net/http"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
)

// FakeClock is controllable time. nil on every target in P0 (spec's
// Global Constraints: deferred to P1).
type FakeClock interface {
	Advance(d time.Duration)
	Now() time.Time
}

// Target is the seam every requirement test speaks through. Two
// implementations: inproc (L2) and cluster (L3, P2). Tests never learn which.
type Target interface {
	// Name is the REQ_TARGET value: inproc, kind, grace.
	Name() string
	// API is a client authenticated as the current principal.
	API() *client.ClientWithResponses
	// As returns a Target bound to another seeded principal:
	// admin, operator, dev-a (project team-a), dev-b (project team-b), anon.
	As(principal string) Target
	// K8s is a controller-runtime client scoped to Namespace(); ok=false on inproc.
	K8s() (c ctrlclient.Client, ok bool)
	Namespace() string
	Clock() FakeClock
	// Has reports a declared capability: keycloak, artifact-keeper, gateway, calico, consumers.
	Has(capability string) bool
	// BaseURL is the API's root (no trailing slash), for raw-HTTP tests such
	// as the generated contract tests that must send malformed bodies the
	// typed client cannot express.
	BaseURL() string
	// Authorize sets the current principal's bearer header on r (no-op for anon).
	Authorize(r *http.Request)
	// Cleanup deletes every cluster whose id carries RunID(). Registered by
	// target.Get via t.Cleanup; exposed so a test can force it early.
	Cleanup(ctx context.Context) error
}

// RunLabel marks every Kubernetes object a run creates outside the API
// (probe pods, CNI-check policies) so postflight can find and reap them
// by run, exactly as bifrost.dev/cluster-id does for API-created clusters.
const RunLabel = "req.bifrost.dev/run"

// Restarter is implemented by targets that can kill and restart the control
// plane under test (the cluster target). Tests gate on
// NeedsCapability(t, tgt, "restart") and then type-assert; inproc has no
// process to kill.
type Restarter interface {
	RestartControlPlane(ctx context.Context) error
}

// PodSpec describes a probe pod a PodRunner runs on the target's cluster.
// With Detach=false the pod runs to completion (Succeeded/Failed); with
// Detach=true RunPod returns as soon as the pod is Running, with its IP.
type PodSpec struct {
	// Namespace defaults to the target's ProbeNamespace (the consumer
	// namespace — "jupyter" on grace and kind).
	Namespace string
	// Labels are added to the RunLabel the runner always stamps.
	Labels  map[string]string
	Image   string
	Command []string
	Detach  bool
	// Timeout defaults to the lane's convergence budget.
	Timeout time.Duration
}

// PodResult is what a probe pod reported.
type PodResult struct {
	Name      string
	IP        string
	Succeeded bool
	Logs      string
}

// PodRunner is implemented by targets with a Kubernetes cluster where tests
// may run probe pods (capability "probes"). Probe pods stand in for the
// consumers — a notebook pod, a checkmaite pod — that the requirements are
// written for.
type PodRunner interface {
	RunPod(ctx context.Context, spec PodSpec) (PodResult, error)
	ProbeNamespace() string
	// RayImage is the Ray image the target provisions clusters with; probe
	// pods that speak Ray Client must run the same Ray version.
	RayImage() string
}
