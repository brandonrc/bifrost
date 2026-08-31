package core

import (
	"encoding/json"
	"testing"
)

// Ported from mobula-core/src/cluster.rs #[cfg(test)] mod tests.

func TestHappyPathLifecycle(t *testing.T) {
	s := ClusterStatePending
	for _, next := range []ClusterState{
		ClusterStateProvisioning,
		ClusterStateRunning,
		ClusterStateSuspending,
		ClusterStateSuspended,
		ClusterStateProvisioning,
		ClusterStateRunning,
		ClusterStateTerminating,
		ClusterStateTerminated,
	} {
		got, err := s.Transition(next)
		if err != nil {
			t.Fatalf("transition %s -> %s: expected legal transition, got error: %v", s, next, err)
		}
		s = got
	}
	if !s.IsTerminal() {
		t.Fatalf("expected terminal state, got %s", s)
	}
}

func TestTerminatedIsTerminal(t *testing.T) {
	for _, target := range []ClusterState{
		ClusterStatePending,
		ClusterStateProvisioning,
		ClusterStateRunning,
		ClusterStateDegraded,
		ClusterStateUpdating,
		ClusterStateSuspending,
		ClusterStateSuspended,
		ClusterStateTerminating,
	} {
		_, err := ClusterStateTerminated.Transition(target)
		want := &TransitionError{From: ClusterStateTerminated, To: target}
		got, ok := err.(*TransitionError)
		if !ok || *got != *want {
			t.Fatalf("Terminated.Transition(%s) = %v, want %v", target, err, want)
		}
	}
}

func TestNoResumeWithoutReprovision(t *testing.T) {
	// Suspended clusters released their compute; they must re-enter
	// Provisioning rather than jumping straight to Running.
	if ClusterStateSuspended.CanTransitionTo(ClusterStateRunning) {
		t.Fatal("Suspended must not be able to transition directly to Running")
	}
	if !ClusterStateSuspended.CanTransitionTo(ClusterStateProvisioning) {
		t.Fatal("Suspended must be able to transition to Provisioning")
	}
}

// Added (not ported from Rust): mobula-core has no unit test in cluster.rs
// for Engine's #[serde(default)] behavior, but it is documented, load-bearing
// wire behavior (pre-multi-engine specs and clients that omit `engine` must
// still deserialize as Ray) and is part of the frozen OpenAPI contract
// (ClusterSpec.engine is not in the `required` list). Characterized here
// since Go's encoding/json has no built-in analogue to serde(default).
func TestClusterSpecEngineDefaultsToRayWhenOmitted(t *testing.T) {
	data := []byte(`{
		"name": "n", "project": "p", "ray_version": "2.9.0", "image": "img",
		"head_cpu": "1", "head_memory": "1Gi", "worker_groups": []
	}`)
	var spec ClusterSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Engine != EngineRay {
		t.Fatalf("engine = %q, want %q", spec.Engine, EngineRay)
	}
}

// Added (not ported from Rust): fix round 1 (review finding M2). A
// zero-value ClusterSpec (built as a Go struct literal without setting
// Engine — there is no Rust equivalent gap, since #[serde(default)] is a
// deserialize-only construct there too, but Rust struct literals always
// require every field explicitly) must still marshal Engine as the
// documented default, not the Go zero value "".
func TestClusterSpecMarshalsZeroValueEngineAsDefault(t *testing.T) {
	var spec ClusterSpec
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if v["engine"] != string(EngineRay) {
		t.Fatalf("engine = %v, want %q", v["engine"], EngineRay)
	}
	workerGroups, ok := v["worker_groups"].([]any)
	if !ok || len(workerGroups) != 0 {
		t.Fatalf("worker_groups = %v, want []", v["worker_groups"])
	}
}
