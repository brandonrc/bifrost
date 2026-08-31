package policy

import (
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Ported from mobula-policy/src/gpu.rs #[cfg(test)] mod tests.

func gpuTestPool(mode *core.GpuSharing) core.PoolSpec {
	return core.PoolSpec{
		Name: "gpu-pool",
		Flavors: []core.FlavorSpec{{
			Name:       "a100",
			Resources:  map[string]string{"nvidia.com/gpu": "8"},
			NodeLabels: map[string]string{},
			Taints:     []core.TaintSpec{},
		}},
		Cohort:            "main",
		FairSharingWeight: 1.0,
		Elastic:           false,
		GpuSharing:        mode,
	}
}

func gpuSharingPtr(g core.GpuSharing) *core.GpuSharing { return &g }

func gpuTestCluster(gpu *string) *core.ClusterSpec {
	return &core.ClusterSpec{
		Engine:     core.EngineRay,
		Name:       "c",
		Project:    "p",
		RayVersion: "2.57.0",
		Image:      "img",
		HeadCpu:    "1",
		HeadMemory: "2Gi",
		WorkerGroups: []core.WorkerGroup{{
			Name:        "w",
			Cpu:         "2",
			Memory:      "4Gi",
			Gpu:         gpu,
			MinReplicas: 1,
			MaxReplicas: 1,
			Replicas:    1,
		}},
	}
}

func TestCrossTenantTimeSliceRejected(t *testing.T) {
	p := gpuTestPool(gpuSharingPtr(core.GpuSharingTimeSlice))
	err := CheckPoolGpuIsolation(&p, core.GpuSharingWholeGpu, 2)
	v, ok := err.(GpuSharingViolation)
	if !ok || v.Kind != GpuSharingViolationCrossTenantTimeSlice || v.Pool != "gpu-pool" || v.Tenants != 2 {
		t.Fatalf("got %#v, err=%v", v, err)
	}
	// The error names the tenant-isolation reason.
	if !strings.Contains(err.Error(), "tenant isolation") {
		t.Fatalf("error %q does not contain \"tenant isolation\"", err.Error())
	}
	// …regardless of how many tenants beyond one share the pool.
	if CheckPoolGpuIsolation(&p, core.GpuSharingWholeGpu, 5) == nil {
		t.Fatal("expected an error for 5 tenants")
	}
}

func TestCrossTenantMigAndWholeGpuAllowed(t *testing.T) {
	for _, mode := range []core.GpuSharing{core.GpuSharingMig, core.GpuSharingWholeGpu} {
		p := gpuTestPool(gpuSharingPtr(mode))
		if err := CheckPoolGpuIsolation(&p, core.GpuSharingWholeGpu, 3); err != nil {
			t.Fatalf("mode %v: unexpected error: %v", mode, err)
		}
	}
}

func TestSingleTenantTimeSliceOptInAllowed(t *testing.T) {
	p := gpuTestPool(gpuSharingPtr(core.GpuSharingTimeSlice))
	if err := CheckPoolGpuIsolation(&p, core.GpuSharingWholeGpu, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := CheckPoolGpuIsolation(&p, core.GpuSharingWholeGpu, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// …and fractional requests into it are fine.
	if err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 1, gpuTestCluster(strPtr("0.5"))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPlatformDefaultAppliesWhenPoolUnset(t *testing.T) {
	p := gpuTestPool(nil)
	if got := EffectiveGpuSharing(&p, core.GpuSharingWholeGpu); got != core.GpuSharingWholeGpu {
		t.Fatalf("got %v, want %v", got, core.GpuSharingWholeGpu)
	}
	// A platform default of time-slice makes an unset pool time-slice —
	// and therefore rejects cross-tenant sharing all the same.
	if got := EffectiveGpuSharing(&p, core.GpuSharingTimeSlice); got != core.GpuSharingTimeSlice {
		t.Fatalf("got %v, want %v", got, core.GpuSharingTimeSlice)
	}
	err := CheckPoolGpuIsolation(&p, core.GpuSharingTimeSlice, 2)
	if v, ok := err.(GpuSharingViolation); !ok || v.Kind != GpuSharingViolationCrossTenantTimeSlice {
		t.Fatalf("expected CrossTenantTimeSlice, got %v", err)
	}
	// The pool's own setting wins over the platform default.
	migPool := gpuTestPool(gpuSharingPtr(core.GpuSharingMig))
	if err := CheckPoolGpuIsolation(&migPool, core.GpuSharingTimeSlice, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFractionalGpuRejectedCrossTenant(t *testing.T) {
	p := gpuTestPool(gpuSharingPtr(core.GpuSharingMig))
	err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 2, gpuTestCluster(strPtr("0.5")))
	v, ok := err.(GpuSharingViolation)
	if !ok || v.Kind != GpuSharingViolationCrossTenantFractionalGpu || v.Group != "w" || v.Requested != "0.5" || v.Tenants != 2 {
		t.Fatalf("got %#v, err=%v", v, err)
	}
	if !strings.Contains(err.Error(), "tenant isolation") {
		t.Fatalf("error %q does not contain \"tenant isolation\"", err.Error())
	}
}

func TestWholeGpuRequestsAllowedCrossTenant(t *testing.T) {
	p := gpuTestPool(gpuSharingPtr(core.GpuSharingWholeGpu))
	if err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 4, gpuTestCluster(strPtr("2"))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No GPU request at all is trivially fine.
	if err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 4, gpuTestCluster(nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Zero is not a share of anything.
	if err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 4, gpuTestCluster(strPtr("0"))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClusterAdmissionIntoNoncompliantPoolFailsClosed(t *testing.T) {
	// A multi-tenant time-slice pool is unreachable through validated
	// writes, but a stored pool could predate the rule — never admit.
	p := gpuTestPool(gpuSharingPtr(core.GpuSharingTimeSlice))
	err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 2, gpuTestCluster(strPtr("1")))
	if v, ok := err.(GpuSharingViolation); !ok || v.Kind != GpuSharingViolationCrossTenantTimeSlice {
		t.Fatalf("expected CrossTenantTimeSlice, got %v", err)
	}
}

func TestUnparseableGpuQuantitySurfaces(t *testing.T) {
	p := gpuTestPool(nil)
	err := CheckClusterGpuIsolation(&p, core.GpuSharingWholeGpu, 2, gpuTestCluster(strPtr("half")))
	if v, ok := err.(GpuSharingViolation); !ok || v.Kind != GpuSharingViolationQuantity {
		t.Fatalf("expected Quantity violation, got %v", err)
	}
}
