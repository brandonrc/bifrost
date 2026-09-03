package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRayJobSpecMarshalsNilSlicesAsEmpty(t *testing.T) {
	var spec RayJobSpec
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"worker_groups", "storage"} {
		arr, ok := v[key].([]any)
		if !ok || len(arr) != 0 {
			t.Fatalf("%s = %v, want []", key, v[key])
		}
	}
	if _, present := v["storage_resolved"]; present {
		t.Fatalf("storage_resolved must be omitted when empty: %s", b)
	}
	if v["ttl_seconds_after_finished"] != nil || v["profile"] != nil || v["owner"] != nil {
		t.Fatalf("nullable pointers must marshal as null: %s", b)
	}
}

func TestRayJobSpecRoundTripsStorageResolvedAndTtl(t *testing.T) {
	ttl := uint32(5)
	mount := "/mnt/data"
	in := RayJobSpec{
		Project: "team-a", Entrypoint: "python -c 1", Image: "rayproject/ray:2.57.0",
		Storage:                 []string{"data"},
		StorageResolved:         []ResolvedStorage{{Name: "data", SecretName: "data-creds", Mode: StorageModeFile, MountPath: &mount}},
		TtlSecondsAfterFinished: &ttl,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RayJobSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TtlSecondsAfterFinishedOrDefault() != 5 {
		t.Fatalf("ttl = %d, want 5", out.TtlSecondsAfterFinishedOrDefault())
	}
	if len(out.StorageResolved) != 1 || out.StorageResolved[0].Mode != StorageModeFile || *out.StorageResolved[0].MountPath != mount {
		t.Fatalf("storage_resolved did not round-trip: %+v", out.StorageResolved)
	}
	if (RayJobSpec{}).TtlSecondsAfterFinishedOrDefault() != DefaultRayJobTtlSecondsAfterFinished {
		t.Fatalf("nil ttl must default to %d", DefaultRayJobTtlSecondsAfterFinished)
	}
}

func TestRayJobStateStrictAndTerminal(t *testing.T) {
	var s RayJobState
	if err := json.Unmarshal([]byte(`"succeeded"`), &s); err != nil || s != RayJobStateSucceeded {
		t.Fatalf("unmarshal succeeded: %v %v", s, err)
	}
	if err := json.Unmarshal([]byte(`"SUCCEEDED"`), &s); err == nil || !strings.Contains(err.Error(), "invalid RayJobState") {
		t.Fatalf("upper-case Ray vocabulary must be rejected on the wire: %v", err)
	}
	for _, st := range []RayJobState{RayJobStateSucceeded, RayJobStateFailed, RayJobStateStopped} {
		if !st.IsTerminal() {
			t.Fatalf("%s should be terminal", st)
		}
	}
	for _, st := range []RayJobState{RayJobStatePending, RayJobStateRunning} {
		if st.IsTerminal() {
			t.Fatalf("%s should not be terminal", st)
		}
	}
}

func TestParseRayJobStatusMapsRayVocabulary(t *testing.T) {
	cases := map[string]RayJobState{
		"PENDING": RayJobStatePending, "RUNNING": RayJobStateRunning,
		"SUCCEEDED": RayJobStateSucceeded, "FAILED": RayJobStateFailed, "STOPPED": RayJobStateStopped,
	}
	for in, want := range cases {
		got, ok := ParseRayJobStatus(in)
		if !ok || got != want {
			t.Fatalf("ParseRayJobStatus(%q) = %v %v, want %v true", in, got, ok, want)
		}
	}
	if _, ok := ParseRayJobStatus(""); ok {
		t.Fatal("empty status must not parse")
	}
	if _, ok := ParseRayJobStatus("running"); ok {
		t.Fatal("lower-case is not Ray's vocabulary")
	}
}
