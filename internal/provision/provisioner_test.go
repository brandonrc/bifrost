package provision

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// fakeProvisioner embeds BaseProvisioner to inherit the Rust trait's
// default (no-op) methods, overriding only what a real backend must
// implement — this is the shape Task 6's live client (and any future
// engine backend) will follow.
type fakeProvisioner struct {
	BaseProvisioner
}

func (fakeProvisioner) Apply(context.Context, core.ClusterId, *core.ClusterSpec, uint64, string, *QueueAssignment) (ApplyResponse, error) {
	return ApplyResponse{}, nil
}
func (fakeProvisioner) EnsureNamespacePosture(context.Context) error    { return nil }
func (fakeProvisioner) Terminate(context.Context, core.ClusterId) error { return nil }
func (fakeProvisioner) Suspend(context.Context, core.ClusterId) error   { return nil }
func (fakeProvisioner) Resume(context.Context, core.ClusterId) error    { return nil }
func (fakeProvisioner) Observe(context.Context, core.ClusterId) (ObservedCluster, error) {
	return ObservedCluster{}, nil
}
func (fakeProvisioner) List(context.Context) ([]ObservedCluster, error) { return nil, nil }

// Compile-time assertion that fakeProvisioner satisfies Provisioner, and
// that BaseProvisioner supplies the Rust trait's default bodies.
var _ Provisioner = fakeProvisioner{}

func TestBaseProvisionerDefaults(t *testing.T) {
	var p fakeProvisioner
	if err := p.ReapNetworkPolicies(context.Background(), "demo"); err != nil {
		t.Fatalf("ReapNetworkPolicies default must be a no-op: %v", err)
	}
	if ep, ok := p.MetricsEndpoint("demo"); ok || ep != "" {
		t.Fatalf("MetricsEndpoint default must be (\"\", false), got (%q, %v)", ep, ok)
	}
	if base, ok := p.DashboardApiBase("demo"); ok || base != "" {
		t.Fatalf("DashboardApiBase default must be (\"\", false), got (%q, %v)", base, ok)
	}
	if nodes, err := p.ClusterNodes(context.Background(), "demo"); nodes != nil || err != nil {
		t.Fatalf("ClusterNodes default must be (nil, nil), got (%v, %v)", nodes, err)
	}
	if events, err := p.ClusterEvents(context.Background(), "demo"); events != nil || err != nil {
		t.Fatalf("ClusterEvents default must be (nil, nil), got (%v, %v)", events, err)
	}
	if logs, err := p.ClusterLogs(context.Background(), "demo", nil, 100); logs != nil || err != nil {
		t.Fatalf("ClusterLogs default must be (nil, nil), got (%v, %v)", logs, err)
	}
}

// lib.rs: ProvisionError variants (NotFound(ClusterId), Backend(String)).
func TestProvisionErrorMessages(t *testing.T) {
	notFound := ProvisionError{Kind: ProvisionErrNotFound, ClusterID: "demo"}
	if got, want := notFound.Error(), "cluster not found: demo"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	backend := ProvisionError{Kind: ProvisionErrBackend, Message: "connection refused"}
	if got, want := backend.Error(), "backend error: connection refused"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// PoolObservation persists as opaque JSON on the pool row (lib.rs:241-265):
// nil maps must marshal as `{}`, never `null`, matching the established
// core/policy pattern, and an absent queues_usage key must deserialize as
// an empty map (Rust's #[serde(default)], added in Slice 4 for
// backward-compat with observations persisted by an older build).
func TestPoolObservationMarshalsEmptyMapsNotNull(t *testing.T) {
	var obs PoolObservation
	b, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["flavors_usage"].(map[string]any); !ok {
		t.Fatalf("flavors_usage = %#v, want {}", m["flavors_usage"])
	}
	if _, ok := m["queues_usage"].(map[string]any); !ok {
		t.Fatalf("queues_usage = %#v, want {}", m["queues_usage"])
	}

	// Backward-compat: an older-build observation without queues_usage at
	// all still deserializes as an empty map, not a nil one that would
	// then re-marshal as null.
	var old PoolObservation
	if err := json.Unmarshal([]byte(`{"admitted_workloads":1,"reserving_workloads":0,"pending_workloads":0,"flavors_usage":{}}`), &old); err != nil {
		t.Fatalf("unmarshal old-shape observation: %v", err)
	}
	b2, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(b2, &m2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m2["queues_usage"].(map[string]any); !ok {
		t.Fatalf("queues_usage = %#v, want {}", m2["queues_usage"])
	}
}
