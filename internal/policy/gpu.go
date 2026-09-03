package policy

// GPU-sharing tenant isolation (#58).
//
// Threat model: NVIDIA GPU time-slicing — and equivalently fractional
// nvidia.com/gpu requests via the device plugin — multiplexes one GPU's
// SMs across processes with no hardware isolation: co-resident tenants can
// observe or starve each other (and share the same failure domain). That
// is acceptable *within* one tenant, never *across* tenants. MIG partitions
// the GPU in hardware (dedicated SMs, memory, and L2 per slice) and
// whole-GPU allocation shares nothing, so both are isolation-safe.
//
// The rule, enforced at admission (pool allocation and cluster creation):
//
//   - A pool shared by more than one project may not resolve to
//     GpuSharingTimeSlice, and clusters admitted to it may not make
//     fractional GPU requests. whole-gpu and mig are always allowed.
//   - A single-project pool may opt into time-slice explicitly
//     (gpu_sharing = "time-slice" on the pool spec), and fractional
//     requests into it are fine.
//
// Pure functions over plain inputs, mirroring the rest of this package: the
// caller (the API edge) supplies the pool spec, the platform default, and
// the tenant count — tenancy lives in allocations, which core types never
// see. Ported from the predecessor's policy crate, src/gpu.rs.

import (
	"fmt"
	"math"

	"github.com/brandonrc/bifrost/internal/core"
)

// EffectiveGpuSharing is the sharing mode a pool effectively runs: its own
// GpuSharing when set, else the platform default ([gpu] default_sharing in
// the policy file, itself defaulting to whole-gpu).
func EffectiveGpuSharing(pool *core.PoolSpec, platformDefault core.GpuSharing) core.GpuSharing {
	if pool.GpuSharing != nil {
		return *pool.GpuSharing
	}
	return platformDefault
}

// GpuSharingViolationKind discriminates GpuSharingViolation causes.
type GpuSharingViolationKind int

const (
	GpuSharingViolationCrossTenantTimeSlice GpuSharingViolationKind = iota
	GpuSharingViolationCrossTenantFractionalGpu
	GpuSharingViolationQuantity
)

// GpuSharingViolation reports a GPU-sharing tenant-isolation failure.
type GpuSharingViolation struct {
	Kind GpuSharingViolationKind
	Pool string
	// Tenants is set for CrossTenantTimeSlice and CrossTenantFractionalGpu.
	Tenants int
	// Group and Requested are set for CrossTenantFractionalGpu.
	Group     string
	Requested string
	// Msg is set for Quantity.
	Msg string
}

func (e GpuSharingViolation) Error() string {
	switch e.Kind {
	case GpuSharingViolationCrossTenantTimeSlice:
		return fmt.Sprintf(
			"tenant isolation: pool %q is shared by %d projects, so gpu_sharing = \"time-slice\" is forbidden — "+
				"time-slicing shares GPU SMs across processes with no hardware isolation; use \"mig\" (hardware partitioning) or \"whole-gpu\"",
			e.Pool, e.Tenants)
	case GpuSharingViolationCrossTenantFractionalGpu:
		return fmt.Sprintf(
			"tenant isolation: pool %q is shared by %d projects, so fractional GPU requests are forbidden — "+
				"worker group %q requests %s nvidia.com/gpu, and a fractional GPU is device-plugin time-slicing; "+
				"request whole GPUs or a MIG slice resource (e.g. nvidia.com/mig-1g.10gb)",
			e.Pool, e.Tenants, e.Group, e.Requested)
	case GpuSharingViolationQuantity:
		return fmt.Sprintf("invalid quantity: %s", e.Msg)
	}
	return "gpu sharing violation"
}

// CheckPoolGpuIsolation is the pool-side check: a pool shared by more than
// one project may not resolve to time-slice. tenants is the number of
// distinct projects holding an allocation in the pool *after* the pending
// change.
func CheckPoolGpuIsolation(pool *core.PoolSpec, platformDefault core.GpuSharing, tenants int) error {
	if tenants > 1 && EffectiveGpuSharing(pool, platformDefault) == core.GpuSharingTimeSlice {
		return GpuSharingViolation{Kind: GpuSharingViolationCrossTenantTimeSlice, Pool: pool.Name, Tenants: tenants}
	}
	return nil
}

// CheckClusterGpuIsolation is the cluster-side check for admission of spec
// into pool: never admit into a non-compliant pool at all (fail closed — a
// multi-tenant time-slice pool should be unreachable through validated
// writes, but rows predate rules), and reject fractional nvidia.com/gpu
// requests when the pool is shared, since a fractional GPU *is*
// time-slicing.
func CheckClusterGpuIsolation(pool *core.PoolSpec, platformDefault core.GpuSharing, tenants int, spec *core.ClusterSpec) error {
	if err := CheckPoolGpuIsolation(pool, platformDefault, tenants); err != nil {
		return err
	}
	if tenants <= 1 {
		return nil
	}
	for i := range spec.WorkerGroups {
		g := &spec.WorkerGroups[i]
		if g.Gpu == nil {
			continue
		}
		raw := *g.Gpu
		n, err := GPUCount(&raw)
		if err != nil {
			return GpuSharingViolation{Kind: GpuSharingViolationQuantity, Msg: err.Error()}
		}
		if n != math.Trunc(n) {
			return GpuSharingViolation{
				Kind:      GpuSharingViolationCrossTenantFractionalGpu,
				Pool:      pool.Name,
				Group:     g.Name,
				Requested: raw,
				Tenants:   tenants,
			}
		}
	}
	return nil
}
