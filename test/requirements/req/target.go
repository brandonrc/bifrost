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
