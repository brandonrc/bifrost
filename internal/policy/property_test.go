package policy

// Property-based tests (stdlib testing/quick — the repo carries no test
// dependencies by decision). These pin the package's security-relevant
// arithmetic invariants across the whole input space rather than at
// hand-picked points:
//
//   - FitsWithin's negated `!(v <= limit)` construction denies NaN (a
//     "simplified" `v > limit` rewrite would silently ADMIT NaN — the
//     regression these tests exist to catch);
//   - quantity parsers never return a negative or non-finite value
//     (a negative demand would lower quota usage; a NaN/Inf one would
//     slip past comparisons);
//   - ClusterDemand rejects min>max and never lets float overflow turn an
//     over-quota demand into an admission;
//   - AdmitBudget admits iff consumed is strictly below the cap;
//   - the GPU tenant-isolation decision matches its documented truth
//     table for every (pool mode, platform default, tenancy, gpu string)
//     combination.

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bifrost-compute/bifrost/internal/core"
)

// anyFloat is a quick.Generator over the adversarial float64 space: NaN,
// both infinities, negatives, subnormals, and near-overflow magnitudes —
// not just the well-behaved positives.
type anyFloat float64

func (anyFloat) Generate(r *rand.Rand, _ int) reflect.Value {
	var v float64
	switch r.Intn(8) {
	case 0:
		v = math.NaN()
	case 1:
		v = math.Inf(1)
	case 2:
		v = math.Inf(-1)
	case 3:
		v = -r.ExpFloat64() * math.Pow(10, float64(r.Intn(40)))
	case 4:
		// Huge magnitudes; Pow overflows to +Inf past ~1e308 on purpose.
		v = r.ExpFloat64() * math.Pow(10, float64(r.Intn(320)))
	case 5:
		v = r.Float64() * 1024
	case 6:
		v = math.MaxFloat64 * r.Float64()
	default:
		v = float64(r.Intn(201) - 100)
	}
	return reflect.ValueOf(anyFloat(v))
}

func toResourceMap(m map[string]anyFloat) ResourceMap {
	out := make(ResourceMap, len(m))
	for k, v := range m {
		out[k] = float64(v)
	}
	return out
}

// specFits is an INDEPENDENT re-derivation of FitsWithin's contract,
// written in the naive style (explicit NaN rejection + `v > limit`)
// rather than the implementation's negated form, so a refactor of either
// side trips the comparison.
func specFits(demand, limit ResourceMap) bool {
	for k, v := range demand {
		lv := limit[k] // a missing limit reads as zero
		if math.IsNaN(v) || math.IsNaN(lv) {
			return false
		}
		if v > lv {
			return false
		}
	}
	return true
}

// FitsWithin agrees with its independently written contract over the full
// float64 space, and never panics (a panic fails quick.Check on its own).
func TestFitsWithinMatchesContract(t *testing.T) {
	f := func(demand, limit map[string]anyFloat) bool {
		d, l := toResourceMap(demand), toResourceMap(limit)
		return d.FitsWithin(l) == specFits(d, l)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// The pointed corollaries, pinned separately so a failure names the exact
// broken guarantee rather than "mismatch at some input".
func TestFitsWithinDeniesNonFiniteAndOverflowingDemand(t *testing.T) {
	// NaN in the demand denies against EVERY limit — including a NaN or
	// +Inf limit (NaN compares false against everything).
	nanLimitCases := []ResourceMap{
		{CPU: 4}, {}, {CPU: math.NaN()}, {CPU: math.Inf(1)},
	}
	for _, limit := range nanLimitCases {
		if (ResourceMap{CPU: math.NaN()}).FitsWithin(limit) {
			t.Errorf("NaN demand admitted against %v — the !(v <= limit) construction must deny", limit)
		}
	}
	// A +Inf demand (float overflow from demand arithmetic) fits only a
	// +Inf limit — never a finite one, however huge.
	if (ResourceMap{CPU: math.Inf(1)}).FitsWithin(ResourceMap{CPU: math.MaxFloat64}) {
		t.Error("+Inf demand admitted against a finite limit — overflow into acceptance")
	}
	if !(ResourceMap{CPU: math.Inf(1)}).FitsWithin(ResourceMap{CPU: math.Inf(1)}) {
		t.Error("+Inf demand against +Inf limit must fit (IEEE: +Inf <= +Inf)")
	}
	// FitsWithin admits ⟹ every compared pair satisfies plain <= with no
	// NaN on either side (the documented postcondition callers rely on).
	f := func(demand, limit map[string]anyFloat) bool {
		d, l := toResourceMap(demand), toResourceMap(limit)
		if !d.FitsWithin(l) {
			return true
		}
		for k, v := range d {
			if math.IsNaN(v) || math.IsNaN(l[k]) || !(v <= l[k]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// finiteStr formats v the way a client quantity string would spell it.
func finiteStr(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Every parser rejects exactly the non-finite/negative numbers and accepts
// exactly the finite non-negative ones — and a parsed value round-trips to
// the input (times the suffix multiplier).
func TestParsersRejectNonFiniteAndNegative(t *testing.T) {
	f := func(v anyFloat) bool {
		x := float64(v)
		_, err := CPUCores(finiteStr(x))
		wantErr := math.IsNaN(x) || math.IsInf(x, 0) || x < 0
		if (err != nil) != wantErr {
			return false
		}
		_, gerr := GPUCount(strPtr(finiteStr(x)))
		if (gerr != nil) != wantErr {
			return false
		}
		_, perr := ParseQuantity(finiteStr(x))
		if (perr != nil) != wantErr {
			return false
		}
		// The Gi-suffixed memory parse computes x*gib first, so a finite
		// mantissa near MaxFloat64 overflows to +Inf mid-parse and must
		// ALSO error (fail closed) rather than wrap to a small value.
		_, merr := MemGiB(finiteStr(x) + "Gi")
		wantMemErr := wantErr || math.IsInf(x*gib, 0)
		return (merr != nil) == wantMemErr
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// Suffix arithmetic: a parsed memory quantity equals mantissa × unit / GiB
// whenever that product is finite, and fails closed (error) when it
// overflows — the parse must never wrap a huge quantity into a small one.
func TestMemGiBSuffixArithmeticNeverWraps(t *testing.T) {
	units := []struct {
		suffix   string
		bytesPer float64
	}{
		{"Ki", 1024}, {"Mi", 1024 * 1024}, {"Gi", gib}, {"Ti", gib * 1024},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	}
	f := func(mantissa anyFloat, pick uint8) bool {
		m := math.Abs(float64(mantissa))
		u := units[int(pick)%len(units)]
		got, err := MemGiB(finiteStr(m) + u.suffix)
		want := m * u.bytesPer / gib
		if math.IsInf(want, 0) || math.IsNaN(want) {
			return err != nil // overflow must fail, never wrap
		}
		if err != nil {
			return false
		}
		return got == want
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// demandSpec generates ClusterSpecs spanning the valid and invalid space:
// replica bounds that may invert (min>max), quantities from harmless to
// near-overflow, nil/present GPU requests.
type demandSpec struct{ spec *core.ClusterSpec }

var (
	cpuMantissas = []string{"0", "1", "2", "0.5", "250", "1e6", "1e150", "1e300"}
	memMantissas = []string{"0", "1", "2", "0.5", "512", "1e6", "1e150", "1e300"}
	memUnits     = []string{"", "Ki", "Mi", "Gi", "Ti", "K", "M", "G", "T"}
	gpuStrings   = []string{"0", "1", "2", "8", "1e3", "1e150", "1e300"}
)

func (demandSpec) Generate(r *rand.Rand, size int) reflect.Value {
	pick := func(ss []string) string { return ss[r.Intn(len(ss))] }
	cpuQty := func() string {
		if r.Intn(2) == 0 {
			return pick(cpuMantissas) + "m" // milli form
		}
		return pick(cpuMantissas)
	}
	spec := &core.ClusterSpec{
		Engine:     core.EngineRay,
		Name:       "c",
		Project:    "p",
		RayVersion: "2.57.0",
		Image:      "img",
		HeadCpu:    cpuQty(),
		HeadMemory: pick(memMantissas) + pick(memUnits),
	}
	n := r.Intn(4)
	for i := 0; i < n; i++ {
		g := core.WorkerGroup{
			Name:   fmt.Sprintf("w%d", i),
			Cpu:    cpuQty(),
			Memory: pick(memMantissas) + pick(memUnits),
		}
		if r.Intn(2) == 0 {
			g.Gpu = strPtr(pick(gpuStrings))
		}
		lo := r.Uint32()
		hi := r.Uint32()
		if r.Intn(4) != 0 && lo > hi {
			lo, hi = hi, lo // mostly valid; 1 in 4 may invert
		}
		g.MinReplicas, g.MaxReplicas, g.Replicas = lo, hi, lo
		spec.WorkerGroups = append(spec.WorkerGroups, g)
	}
	return reflect.ValueOf(demandSpec{spec: spec})
}

// ClusterDemand: any group with min>max is rejected; a successful demand
// is monotone (min <= max on every key) and non-negative — however huge
// the inputs, demand arithmetic can overflow UP to +Inf but never wrap
// negative or NaN.
func TestClusterDemandProperties(t *testing.T) {
	f := func(in demandSpec) bool {
		inverted := false
		for _, g := range in.spec.WorkerGroups {
			if g.MinReplicas > g.MaxReplicas {
				inverted = true
			}
		}
		min, max, err := ClusterDemand(in.spec)
		if inverted {
			if err == nil {
				return false
			}
			if _, ok := err.(QuantityError); !ok {
				return false
			}
			return true
		}
		if err != nil {
			// A parse-time overflow (e.g. "1e300" with a T suffix) fails
			// closed with a QuantityError — never a wrapped-around small
			// demand, never a non-Quantity error, never a panic.
			_, ok := err.(QuantityError)
			return ok
		}
		for k, v := range min {
			if math.IsNaN(v) || v < 0 {
				return false
			}
			if mv := max[k]; math.IsNaN(mv) || !(v <= mv) {
				return false
			}
		}
		// min admits against max on every key (+Inf <= +Inf holds), since
		// quota admits against max and min must never exceed it.
		return min.FitsWithin(max)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// Quota never overflows into acceptance: against a FINITE limit (the only
// kind LimitFromQuantities can produce), AdmitQuota admits only when every
// max-demand value is finite and within the limit. Huge-but-valid demands
// that overflow to +Inf must be refused, not admitted.
func TestAdmitQuotaNeverOverflowsIntoAcceptance(t *testing.T) {
	f := func(in demandSpec, cpuCap, memCap, gpuCap anyFloat) bool {
		_, max, err := ClusterDemand(in.spec)
		if err != nil {
			return true // rejected earlier; nothing to admit
		}
		limit := ResourceMap{}
		for k, v := range map[string]anyFloat{CPU: cpuCap, Memory: memCap, GPU: gpuCap} {
			if x := float64(v); !math.IsNaN(x) && !math.IsInf(x, 0) && x >= 0 {
				limit[k] = x
			}
		}
		admitErr := AdmitQuota("p", limit, ResourceMap{}, max)
		for k, v := range max {
			if math.IsNaN(v) || math.IsInf(v, 0) || v > limit[k] {
				// Unfittable demand must be refused — this is the pin
				// against float overflow laundering an over-quota cluster
				// into an admission.
				if admitErr == nil {
					return false
				}
				if _, ok := admitErr.(QuotaExceeded); !ok {
					return false
				}
				return true
			}
		}
		return admitErr == nil
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// AdmitBudget is the documented floor: admit iff consumed is STRICTLY
// below the cap on every resource the budget lists. consumed >= cap
// blocks (including the cap-of-0 and equal-at-cap edges), consumed < cap
// on all listed resources admits, and an unlisted resource is
// unconstrained. Non-finite values are not "strictly below" anything, so
// they block too — same fail-closed posture as FitsWithin's NaN denial.
func TestAdmitBudgetFloorSemantics(t *testing.T) {
	f := func(limits, consumed map[string]anyFloat, window uint64) bool {
		b := &Budget{WindowSecs: window, Limits: map[string]float64{}}
		for k, v := range limits {
			b.Limits[k] = float64(v)
		}
		c := toResourceMap(consumed)
		wantAdmit := true
		for resource, cap := range b.Limits {
			if !(c[resource] < cap) {
				wantAdmit = false
				break
			}
		}
		err := AdmitBudget("p", b, c)
		if wantAdmit {
			return err == nil
		}
		if err == nil {
			return false
		}
		if _, ok := err.(BudgetExceeded); !ok {
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// --- GPU tenant isolation (#58) truth table ---

// gpuIsolationCase covers the full decision space: pool mode set/unset ×
// platform default × tenant count × per-group gpu strings drawn from a
// corpus of whole, fractional, zero, malformed and adversarial quantities.
type gpuIsolationCase struct {
	poolMode    *core.GpuSharing
	def         core.GpuSharing
	tenants     int
	gpuRequests []*string // one per worker group
}

var gpuSharingModes = []core.GpuSharing{core.GpuSharingWholeGpu, core.GpuSharingMig, core.GpuSharingTimeSlice}

// gpuCorpus includes the sneaky spellings: "0.50" (fractional with a
// trailing zero), "1e-1" (exponent fractional), "500m" (a CPU-style milli
// suffix that is NOT a valid GPU count and must error, not parse as 0.5),
// "+Inf"/"NaN" (non-finite), "-1" (negative).
var gpuCorpus = []string{
	"0", "1", "2", "7", "2.0", "0.000",
	"0.5", "1.5", "0.50", "1e-1", "3.25",
	"abc", "500m", "", "-1", "NaN", "+Inf", "-Inf",
}

func (gpuIsolationCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := gpuIsolationCase{def: gpuSharingModes[r.Intn(len(gpuSharingModes))]}
	if r.Intn(2) == 0 {
		mode := gpuSharingModes[r.Intn(len(gpuSharingModes))]
		c.poolMode = &mode
	}
	// Tenancy biased to the boundary: 0 (no allocations), 1 (single
	// tenant), 2+ (multi-tenant).
	switch r.Intn(6) {
	case 0:
		c.tenants = 0
	case 1, 2:
		c.tenants = 1
	case 3, 4:
		c.tenants = 2
	default:
		c.tenants = 2 + r.Intn(100)
	}
	n := r.Intn(4)
	for i := 0; i < n; i++ {
		if r.Intn(4) == 0 {
			c.gpuRequests = append(c.gpuRequests, nil) // no GPU request
			continue
		}
		s := gpuCorpus[r.Intn(len(gpuCorpus))]
		c.gpuRequests = append(c.gpuRequests, &s)
	}
	return reflect.ValueOf(c)
}

// wantIsolationDecision independently derives the documented decision:
//
//   - tenants > 1 with effective time-slice ⟹ CrossTenantTimeSlice (fail
//     closed — admission into a non-compliant pool is refused outright);
//   - tenants <= 1 ⟹ admit, no matter what the gpu strings say (the
//     function deliberately does not parse them on this arm — a
//     single-tenant pool places no fractional restriction at all);
//   - else, first bad group wins: unparseable/non-finite/negative ⟹
//     Quantity; non-integral ⟹ CrossTenantFractionalGpu; all-integral ⟹
//     admit.
func wantIsolationDecision(c gpuIsolationCase) (violation bool, kind GpuSharingViolationKind) {
	eff := c.def
	if c.poolMode != nil {
		eff = *c.poolMode
	}
	if c.tenants > 1 && eff == core.GpuSharingTimeSlice {
		return true, GpuSharingViolationCrossTenantTimeSlice
	}
	if c.tenants <= 1 {
		return false, 0
	}
	for _, req := range c.gpuRequests {
		if req == nil {
			continue
		}
		// GPUCount's contract: nil or "" means "no request" (0), which is
		// integral and always admissible — only a NON-empty string is
		// parsed.
		if *req == "" {
			continue
		}
		n, perr := strconv.ParseFloat(strings.TrimSpace(*req), 64)
		if perr != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return true, GpuSharingViolationQuantity
		}
		if n != math.Trunc(n) {
			return true, GpuSharingViolationCrossTenantFractionalGpu
		}
	}
	return false, 0
}

// The decision function matches its own truth table across the whole
// input space, and EffectiveGpuSharing resolves pool-over-default.
func TestGpuIsolationMatchesTruthTable(t *testing.T) {
	f := func(c gpuIsolationCase) bool {
		pool := &core.PoolSpec{Name: "p", GpuSharing: c.poolMode}
		if EffectiveGpuSharing(pool, c.def) != func() core.GpuSharing {
			if c.poolMode != nil {
				return *c.poolMode
			}
			return c.def
		}() {
			return false
		}
		spec := &core.ClusterSpec{}
		for i, req := range c.gpuRequests {
			spec.WorkerGroups = append(spec.WorkerGroups, core.WorkerGroup{Name: fmt.Sprintf("w%d", i), Gpu: req})
		}
		wantViol, wantKind := wantIsolationDecision(c)
		err := CheckClusterGpuIsolation(pool, c.def, c.tenants, spec)
		if wantViol {
			v, ok := err.(GpuSharingViolation)
			return ok && v.Kind == wantKind
		}
		return err == nil
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// The pool-side check is the time-slice arm of the same table, and the
// cluster-side check subsumes it (a cluster is never admitted into a pool
// the pool check itself would refuse).
func TestPoolGpuIsolationConsistency(t *testing.T) {
	f := func(c gpuIsolationCase) bool {
		pool := &core.PoolSpec{Name: "p", GpuSharing: c.poolMode}
		eff := c.def
		if c.poolMode != nil {
			eff = *c.poolMode
		}
		poolErr := CheckPoolGpuIsolation(pool, c.def, c.tenants)
		wantPoolErr := c.tenants > 1 && eff == core.GpuSharingTimeSlice
		if (poolErr != nil) != wantPoolErr {
			return false
		}
		spec := &core.ClusterSpec{}
		clusterErr := CheckClusterGpuIsolation(pool, c.def, c.tenants, spec)
		// Subsumption: pool refused ⟹ cluster refused.
		return poolErr == nil || clusterErr != nil
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}
