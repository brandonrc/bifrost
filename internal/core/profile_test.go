package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPoolPurposeStrictAndDefault(t *testing.T) {
	var p PoolPurpose
	if err := json.Unmarshal([]byte(`"serving"`), &p); err != nil || p != PoolPurposeServing {
		t.Fatalf("unmarshal serving: %v %v", p, err)
	}
	if err := json.Unmarshal([]byte(`"gpu"`), &p); err == nil || !strings.Contains(err.Error(), "invalid PoolPurpose") {
		t.Fatalf("unknown purpose must be rejected: %v", err)
	}
	if PoolPurpose("").OrDefault() != PoolPurposeCompute || PoolPurposeServing.OrDefault() != PoolPurposeServing {
		t.Fatal("OrDefault must map only the zero value onto compute")
	}
}

func TestPoolSpecPurposeDefaultsToComputeWhenOmitted(t *testing.T) {
	var spec PoolSpec
	if err := json.Unmarshal([]byte(`{"name":"gpu","flavors":[],"cohort":"c","fair_sharing_weight":1,"elastic":false}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Purpose != PoolPurposeCompute {
		t.Fatalf("Purpose = %q, want compute", spec.Purpose)
	}
	b, err := json.Marshal(PoolSpec{Name: "gpu"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	if v["purpose"] != string(PoolPurposeCompute) {
		t.Fatalf("zero-value Purpose must marshal as compute: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"name":"gpu","purpose":"serving"}`), &spec); err != nil || spec.Purpose != PoolPurposeServing {
		t.Fatalf("explicit serving: %v %v", spec.Purpose, err)
	}
}

func TestStorageModeStrict(t *testing.T) {
	var m StorageMode
	if err := json.Unmarshal([]byte(`"file"`), &m); err != nil || m != StorageModeFile {
		t.Fatalf("unmarshal file: %v %v", m, err)
	}
	if err := json.Unmarshal([]byte(`"volume"`), &m); err == nil || !strings.Contains(err.Error(), "invalid StorageMode") {
		t.Fatalf("unknown mode must be rejected: %v", err)
	}
}

func TestCatalogTypesMarshalNilSlicesAsEmpty(t *testing.T) {
	check := func(t *testing.T, v any, keys ...string) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal to map: %v", err)
		}
		for _, k := range keys {
			arr, ok := m[k].([]any)
			if !ok || len(arr) != 0 {
				t.Fatalf("%T.%s = %v, want []", v, k, m[k])
			}
		}
	}
	check(t, StorageEntry{Name: "s", SecretName: "sec", Mode: StorageModeEnv}, "projects")
	check(t, Profile{Name: "p"}, "worker_groups", "projects")
	check(t, AdmissionRule{}, "allowed_images")
}

func TestProfileRoundTripsPointers(t *testing.T) {
	desc, max, ttl := "small", uint32(4), uint64(3600)
	in := Profile{Name: "small", Description: &desc, Image: "rayproject/ray:2.57.0", RayVersion: "2.57.0",
		HeadCpu: "1", HeadMemory: "2Gi", MaxWorkers: &max, TtlSeconds: &ttl, Projects: []string{"team-a"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Profile
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if *out.Description != desc || *out.MaxWorkers != max || *out.TtlSeconds != ttl || out.IdleTimeoutSecs != nil || len(out.Projects) != 1 {
		t.Fatalf("round trip lost fields: %+v", out)
	}
}
