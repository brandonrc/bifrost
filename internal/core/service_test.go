package core

import (
	"encoding/json"
	"testing"
)

// Ported from mobula-core/src/service.rs #[cfg(test)] mod tests.

func TestUpgradeDefaultsToCanaryWhenOmitted(t *testing.T) {
	// serve_config_v2 passes through verbatim; upgrade has a serde
	// default so older clients can omit it.
	data := []byte(`{
		"name": "svc",
		"project": "p",
		"ray_version": "2.57.0",
		"image": "img",
		"serve_config_v2": "applications: []",
		"head_cpu": "1",
		"head_memory": "2Gi",
		"worker_replicas": 2,
		"worker_cpu": "1",
		"worker_memory": "2Gi"
	}`)
	var spec ServiceSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spec.Upgrade != UpgradeStrategyCanary {
		t.Fatalf("upgrade = %v, want %v", spec.Upgrade, UpgradeStrategyCanary)
	}
}

func TestUpgradeRoundTripsSnakeCase(t *testing.T) {
	b, err := json.Marshal(UpgradeStrategyInPlace)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"in_place"` {
		t.Fatalf("json = %s, want \"in_place\"", b)
	}

	var got UpgradeStrategy
	if err := json.Unmarshal([]byte(`"canary"`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != UpgradeStrategyCanary {
		t.Fatalf("got %v, want %v", got, UpgradeStrategyCanary)
	}
}
