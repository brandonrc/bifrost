package core

import (
	"encoding/json"
	"testing"
)

// The predecessor's core crate, src/job.rs has no #[cfg(test)] module. This is an added
// smoke test (not ported) characterizing the JSON shape against the Rust
// serde attributes, since there is no Rust test to drive this from.

func TestJobRecordDurationSecsPresentAsNull(t *testing.T) {
	j := JobRecord{
		Id:          "raysubmit_abc",
		Cluster:     "demo",
		Submitter:   "-",
		Status:      "RUNNING",
		SubmittedAt: 1_700_000_000,
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	raw, ok := v["duration_secs"]
	if !ok {
		t.Fatal("duration_secs must be present as null when unset")
	}
	if raw != nil {
		t.Fatalf("duration_secs = %v, want null", raw)
	}

	dur := uint64(42)
	j.DurationSecs = &dur
	b, err = json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round JobRecord
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if round.DurationSecs == nil || *round.DurationSecs != 42 {
		t.Fatalf("round trip mismatch: got %#v", round)
	}
}
