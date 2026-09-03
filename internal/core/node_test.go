package core

import (
	"encoding/json"
	"testing"
)

// The predecessor's core crate, src/node.rs has no #[cfg(test)] module. This is an added
// smoke test (not ported) characterizing the JSON shape against the Rust
// serde attributes and the frozen OpenAPI schema, since there is no Rust
// test to drive this from.

func TestNodeViewOptionalFieldsOmittedWhenAbsent(t *testing.T) {
	n := NodeView{
		PodName: "head-abc",
		IsHead:  true,
		Phase:   "Running",
		Ready:   true,
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, omitted := range []string{"group", "node_ip", "host", "cpu", "memory_bytes", "gpu"} {
		if _, ok := v[omitted]; ok {
			t.Fatalf("%s must be omitted when nil, got %v", omitted, v[omitted])
		}
	}
	for _, required := range []string{"pod_name", "is_head", "phase", "ready"} {
		if _, ok := v[required]; !ok {
			t.Fatalf("%s must always be present", required)
		}
	}
}

func TestClusterNodesShape(t *testing.T) {
	cn := ClusterNodes{
		ClusterId: "demo",
		Head:      nil,
		WorkerGroups: []WorkerGroupNodes{
			{Name: "cpu", Desired: 2, Ready: 1, Nodes: []NodeView{}},
		},
	}
	b, err := json.Marshal(cn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := v["head"]; ok {
		t.Fatal("head must be omitted when nil")
	}
	if _, ok := v["worker_groups"]; !ok {
		t.Fatal("worker_groups must always be present")
	}

	var round ClusterNodes
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if round.ClusterId != cn.ClusterId || round.Head != nil || len(round.WorkerGroups) != 1 {
		t.Fatalf("round trip mismatch: got %#v", round)
	}
}

// Added (not ported from Rust): fix round 1 (review finding M3). A
// zero-value WorkerGroupNodes/ClusterNodes (nil Nodes/WorkerGroups) must
// still marshal each as `[]`, not the Go zero value `null`, matching
// Rust's Vec::default() serde behavior.
func TestWorkerGroupNodesMarshalsNilNodesAsEmpty(t *testing.T) {
	var w WorkerGroupNodes
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if nodes, ok := v["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Fatalf("nodes = %v, want []", v["nodes"])
	}
}

func TestClusterNodesMarshalsNilWorkerGroupsAsEmpty(t *testing.T) {
	var cn ClusterNodes
	b, err := json.Marshal(cn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if wg, ok := v["worker_groups"].([]any); !ok || len(wg) != 0 {
		t.Fatalf("worker_groups = %v, want []", v["worker_groups"])
	}
}
