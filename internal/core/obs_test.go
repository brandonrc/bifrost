package core

import (
	"encoding/json"
	"testing"
)

// mobula-core/src/obs.rs has no #[cfg(test)] module. This is an added
// smoke test (not ported) characterizing the JSON shape against the Rust
// serde attributes, since there is no Rust test to drive this from.

func TestClusterEventShape(t *testing.T) {
	e := ClusterEvent{EventType: "Warning", Count: 3}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if v["type"] != "Warning" {
		t.Fatalf("type = %v, want Warning (event_type renames to \"type\")", v["type"])
	}
	for _, omitted := range []string{"reason", "message", "first_seen", "last_seen", "object"} {
		if _, ok := v[omitted]; ok {
			t.Fatalf("%s must be omitted when nil", omitted)
		}
	}
	if _, ok := v["count"]; !ok {
		t.Fatal("count must always be present")
	}
}

func TestResourceStatOmitsUsedWhenNil(t *testing.T) {
	s := ResourceStat{Total: 32}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := v["used"]; ok {
		t.Fatal("used must be omitted when nil")
	}
	if v["total"] != float64(32) {
		t.Fatalf("total = %v, want 32", v["total"])
	}
}

func TestClusterLogsRoundTrip(t *testing.T) {
	l := ClusterLogs{
		ClusterId: "demo",
		Pods:      []string{"demo-head-abc"},
		Pod:       "demo-head-abc",
		Tail:      100,
		Lines:     []string{"line 1", "line 2"},
		Truncated: true,
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round ClusterLogs
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if round.ClusterId != l.ClusterId || round.Tail != l.Tail || round.Truncated != l.Truncated || len(round.Lines) != 2 {
		t.Fatalf("round trip mismatch: got %#v", round)
	}
}
