package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Ported from the predecessor's policy crate, src/lib.rs #[cfg(test)] mod tests.

func testSpec(min, max uint32, gpu *string) *core.ClusterSpec {
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
			MinReplicas: min,
			MaxReplicas: max,
			Replicas:    min,
		}},
	}
}

func strPtr(s string) *string { return &s }

func rmap(pairs map[string]float64) ResourceMap {
	out := make(ResourceMap, len(pairs))
	for k, v := range pairs {
		out[k] = v
	}
	return out
}

func rmapEq(a, b ResourceMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestDemandMinAndMax(t *testing.T) {
	min, max, err := ClusterDemand(testSpec(1, 3, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// head(1cpu,2Gi) + 1 worker(2cpu,4Gi)
	want := rmap(map[string]float64{"cpu": 3.0, "memory": 6.0})
	if !rmapEq(min, want) {
		t.Fatalf("min = %v, want %v", min, want)
	}
	// head + 3 workers
	want = rmap(map[string]float64{"cpu": 7.0, "memory": 14.0})
	if !rmapEq(max, want) {
		t.Fatalf("max = %v, want %v", max, want)
	}
}

func TestGpuDemandCounted(t *testing.T) {
	_, max, err := ClusterDemand(testSpec(0, 2, strPtr("1")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if max.Gpu() != 2.0 {
		t.Fatalf("gpu = %v, want 2.0", max.Gpu())
	}
	// The well-known helpers read the well-known keys.
	if max.Cpu() != 5.0 { // head 1 + 2x2
		t.Fatalf("cpu = %v, want 5.0", max.Cpu())
	}
	if max.MemGiB() != 10.0 { // head 2 + 2x4
		t.Fatalf("memory = %v, want 10.0", max.MemGiB())
	}
}

func TestNoGpuKeyWithoutGpuWorkers(t *testing.T) {
	// Sparse maps: no GPU request means no key at all, so a quota sheet
	// without a GPU entry still admits GPU-free clusters.
	_, max, err := ClusterDemand(testSpec(0, 2, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := max[GPU]; ok {
		t.Fatalf("expected no %q key, got %v", GPU, max)
	}
}

func TestAddIsKeyUnion(t *testing.T) {
	a := rmap(map[string]float64{"cpu": 1.0, "nvidia.com/gpu": 2.0})
	b := rmap(map[string]float64{"cpu": 3.0, "example.com/license": 1.0})
	got := a.Add(b)
	want := rmap(map[string]float64{"cpu": 4.0, "nvidia.com/gpu": 2.0, "example.com/license": 1.0})
	if !rmapEq(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFitsWithinTreatsMissingLimitKeyAsZero(t *testing.T) {
	// Any demand for a resource the limit doesn't list must reject.
	demand := rmap(map[string]float64{"cpu": 1.0, "example.com/license": 1.0})
	limit := rmap(map[string]float64{"cpu": 10.0})
	if demand.FitsWithin(limit) {
		t.Fatal("expected demand not to fit")
	}
	// Zero demand for an unlisted resource fits.
	demand = rmap(map[string]float64{"cpu": 1.0})
	if !demand.FitsWithin(limit) {
		t.Fatal("expected demand to fit")
	}
}

func TestExtendedResourceKeysInDemandMaps(t *testing.T) {
	// Demand maps are constructed directly with arbitrary K8s resource
	// names (MIG slices, custom licenses) — no hard-coded key list.
	demand := rmap(map[string]float64{"nvidia.com/mig-1g.10gb": 7.0, "cpu": 4.0})
	limit := rmap(map[string]float64{"nvidia.com/mig-1g.10gb": 7.0, "cpu": 8.0})
	if !demand.FitsWithin(limit) {
		t.Fatal("expected demand to fit")
	}
	scaled := demand.Scale(2.0)
	if scaled["nvidia.com/mig-1g.10gb"] != 14.0 {
		t.Fatalf("got %v, want 14.0", scaled["nvidia.com/mig-1g.10gb"])
	}
	if scaled.FitsWithin(limit) {
		t.Fatal("expected scaled demand not to fit")
	}
}

func TestCostEstimateMinBelowMax(t *testing.T) {
	prices := PriceSheet{"cpu": 0.04, "nvidia.com/gpu": 2.0, "memory": 0.005}
	est, err := prices.Estimate(testSpec(1, 3, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !(est.MinHourly < est.MaxHourly) {
		t.Fatalf("min %v not < max %v", est.MinHourly, est.MaxHourly)
	}
	// max = 7cpu*0.04 + 0 + 14*0.005 = 0.28 + 0.07 = 0.35
	if math.Abs(est.MaxHourly-0.35) >= 1e-9 {
		t.Fatalf("max = %v, want ~0.35", est.MaxHourly)
	}
}

func TestPriceSheetIgnoresUnknownKeys(t *testing.T) {
	// A resource with no price entry contributes 0, never an error.
	prices := PriceSheet{"cpu": 0.04}
	demand := rmap(map[string]float64{"cpu": 2.0, "example.com/license": 5.0})
	if math.Abs(prices.price(demand)-0.08) >= 1e-9 {
		t.Fatalf("price = %v, want ~0.08", prices.price(demand))
	}
}

func TestPriceSheetDeserializesFromFlatConfigMap(t *testing.T) {
	var sheet PriceSheet
	if err := json.Unmarshal([]byte(`{"cpu": 0.04, "nvidia.com/gpu": 2.0, "memory": 0.005}`), &sheet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(sheet["nvidia.com/gpu"]-2.0) >= 1e-9 {
		t.Fatalf("got %v, want ~2.0", sheet["nvidia.com/gpu"])
	}
}

func TestQuotaAdmitsAndRejects(t *testing.T) {
	limit := rmap(map[string]float64{"cpu": 10.0, "memory": 20.0})
	inUse := rmap(map[string]float64{"cpu": 4.0, "memory": 8.0})
	// requested max for spec(1,3): 7cpu/14Gi -> 4+7=11 > 10 -> reject.
	_, req, err := ClusterDemand(testSpec(1, 3, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := AdmitQuota("p", limit.Clone(), inUse.Clone(), req); err == nil {
		t.Fatal("expected quota to be exceeded")
	}
	// Smaller cluster fits.
	_, small, err := ClusterDemand(testSpec(0, 1, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := AdmitQuota("p", limit, inUse, small); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGpuQuotaEnforcedIndependently(t *testing.T) {
	limit := rmap(map[string]float64{"cpu": 100.0, "nvidia.com/gpu": 1.0, "memory": 100.0})
	_, req, err := ClusterDemand(testSpec(0, 2, strPtr("1"))) // 2 GPUs
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := AdmitQuota("p", limit, nil, req); err == nil {
		t.Fatal("expected quota to be exceeded")
	}
}

func TestBadQuantitySurfacesError(t *testing.T) {
	s := testSpec(1, 1, nil)
	s.HeadCpu = "banana"
	_, _, err := ClusterDemand(s)
	if _, ok := err.(QuantityError); !ok {
		t.Fatalf("expected QuantityError, got %T: %v", err, err)
	}
}

func TestBudgetDeserializesWindowAndFlattenedResourceHours(t *testing.T) {
	// window_secs is a named field; every other key flattens into limits.
	var b Budget
	if err := json.Unmarshal([]byte(`{"window_secs":604800,"nvidia.com/gpu":100.0,"cpu":5000.0}`), &b); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.WindowSecs != 604800 {
		t.Fatalf("window_secs = %v, want 604800", b.WindowSecs)
	}
	if b.Limits["nvidia.com/gpu"] != 100.0 {
		t.Fatalf("got %v, want 100.0", b.Limits["nvidia.com/gpu"])
	}
	if b.Limits["cpu"] != 5000.0 {
		t.Fatalf("got %v, want 5000.0", b.Limits["cpu"])
	}
	if b.LimitMap().Gpu() != 100.0 {
		t.Fatalf("got %v, want 100.0", b.LimitMap().Gpu())
	}
	// The window field is not swept into the resource limits.
	if _, ok := b.Limits["window_secs"]; ok {
		t.Fatal("window_secs leaked into limits")
	}
}

func TestBudgetAdmitsUnderAndDeniesAtOrOverCap(t *testing.T) {
	budget := &Budget{WindowSecs: 604800, Limits: map[string]float64{"nvidia.com/gpu": 100.0}}
	// Under the cap admits.
	if err := AdmitBudget("team-a", budget, rmap(map[string]float64{"nvidia.com/gpu": 99.9})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// At the cap denies (consumed >= cap).
	if err := AdmitBudget("team-a", budget, rmap(map[string]float64{"nvidia.com/gpu": 100.0})); err == nil {
		t.Fatal("expected budget to be exceeded")
	}
	// Over the cap denies, and the error carries the accounting.
	err := AdmitBudget("team-a", budget, rmap(map[string]float64{"nvidia.com/gpu": 150.0}))
	be, ok := err.(BudgetExceeded)
	if !ok {
		t.Fatalf("expected BudgetExceeded, got %T: %v", err, err)
	}
	if be.Project != "team-a" {
		t.Fatalf("project = %v, want team-a", be.Project)
	}
	if be.Consumed.Gpu() != 150.0 {
		t.Fatalf("consumed.gpu = %v, want 150.0", be.Consumed.Gpu())
	}
	if be.Limit.Gpu() != 100.0 {
		t.Fatalf("limit.gpu = %v, want 100.0", be.Limit.Gpu())
	}
	if be.WindowSecs != 604800 {
		t.Fatalf("window_secs = %v, want 604800", be.WindowSecs)
	}
}

func TestBudgetOnlyConstrainsListedResources(t *testing.T) {
	// A budget on GPU-hours does not constrain CPU-hours at all.
	budget := &Budget{WindowSecs: 604800, Limits: map[string]float64{"nvidia.com/gpu": 100.0}}
	// Huge CPU consumption, zero GPU consumption -> admits.
	if err := AdmitBudget("team-a", budget, rmap(map[string]float64{"cpu": 1_000_000.0})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty budget (no listed resources) never denies.
	empty := &Budget{WindowSecs: 604800, Limits: map[string]float64{}}
	if err := AdmitBudget("team-a", empty, rmap(map[string]float64{"cpu": 9e9, "nvidia.com/gpu": 9e9})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMinReplicasAboveMaxIsRejected(t *testing.T) {
	// A group with min > max would make "max" demand smaller than the
	// min — quota admits against max, so this is rejected, not silently
	// mischarged.
	_, _, err := ClusterDemand(testSpec(3, 1, nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	qe, ok := err.(QuantityError)
	if !ok {
		t.Fatalf("expected QuantityError, got %T: %v", err, err)
	}
	if want := "min_replicas (3) > max_replicas (1)"; !strings.Contains(qe.Msg, want) {
		t.Fatalf("message %q does not contain %q", qe.Msg, want)
	}
}

// Fix-round-1 regression tests (review findings I1, M1, M2).

func TestBudgetUnmarshalRequiresWindowSecs(t *testing.T) {
	// window_secs is required on the wire (BudgetView's frozen schema, and
	// Rust's serde derive errors on a missing non-Option field). Silently
	// defaulting to 0 would make AdmitBudget a permanent no-op.
	var b Budget
	err := json.Unmarshal([]byte(`{"nvidia.com/gpu":100.0,"cpu":5000.0}`), &b)
	if err == nil {
		t.Fatal("expected an error for missing window_secs")
	}
}

func TestFitsWithinDeniesNaN(t *testing.T) {
	// NaN must diverge the same direction as Rust's `v <= limit` (false
	// for any comparison against NaN, so the demand is denied, not
	// silently admitted).
	demand := rmap(map[string]float64{"cpu": math.NaN()})
	limit := rmap(map[string]float64{"cpu": 10.0})
	if demand.FitsWithin(limit) {
		t.Fatal("expected NaN demand to be denied, not fit")
	}
}

func TestClusterDemandMinAndMaxAreIndependentMaps(t *testing.T) {
	min, max, err := ClusterDemand(testSpec(1, 3, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMax := max.Clone()
	min["cpu"] = -999.0
	if !rmapEq(max, wantMax) {
		t.Fatalf("mutating min affected max — they alias the same map (max = %v, want %v)", max, wantMax)
	}
	wantMin := min.Clone()
	max["memory"] = -999.0
	if !rmapEq(min, wantMin) {
		t.Fatalf("mutating max affected min — they alias the same map (min = %v, want %v)", min, wantMin)
	}
}

// Strengthens TestClusterDemandMinAndMaxAreIndependentMaps: that test's
// spec always has one worker group, so the accumulation loop runs at least
// once and every iteration's Add() call allocates a fresh map — which
// masks a reintroduced `max = head` (no .Clone()) aliasing bug the moment
// the loop runs, since min/max are reassigned to brand-new map objects
// either way. The case that actually discriminates the bug is zero worker
// groups: the loop body never runs, so min/max are returned exactly as
// head/head.Clone() produced them, with no Add() call to paper over an
// aliased max.
func TestClusterDemandMinAndMaxAreIndependentMapsNoWorkerGroups(t *testing.T) {
	spec := &core.ClusterSpec{
		Engine:     core.EngineRay,
		Name:       "c",
		Project:    "p",
		RayVersion: "2.57.0",
		Image:      "img",
		HeadCpu:    "1",
		HeadMemory: "2Gi",
	}
	min, max, err := ClusterDemand(spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMax := max.Clone()
	min["cpu"] = -999.0
	if !rmapEq(max, wantMax) {
		t.Fatalf("mutating min affected max — they alias the same map (max = %v, want %v)", max, wantMax)
	}
}

// D: QuotaExceeded.Error() must render its ResourceMap fields the way
// Rust's derived Debug renders `ResourceMap(BTreeMap<String, f64>)` —
// sorted keys, `ResourceMap({"cpu": 4.0})` newtype-wrapped form, always a
// decimal point on the float — not Go's %v map form.
func TestQuotaExceededMessageMatchesRustDebug(t *testing.T) {
	err := QuotaExceeded{
		Project:   "p",
		Requested: ResourceMap{"cpu": 4.0, "memory": 8.0},
		InUse:     ResourceMap{},
		Limit:     ResourceMap{"cpu": 2.0},
	}
	want := `project p quota exceeded: requested max ResourceMap({"cpu": 4.0, "memory": 8.0}) + in-use ResourceMap({}) exceeds limit ResourceMap({"cpu": 2.0})`
	if got := err.Error(); got != want {
		t.Fatalf("Error() =\n%q\nwant\n%q", got, want)
	}
}

// D: BudgetExceeded.Error() — same rustDebug fidelity as QuotaExceeded.
func TestBudgetExceededMessageMatchesRustDebug(t *testing.T) {
	err := BudgetExceeded{
		Project:    "p",
		Consumed:   ResourceMap{"cpu": 100.5},
		Limit:      ResourceMap{"cpu": 100.0},
		WindowSecs: 604800,
	}
	want := `project p budget exceeded: consumed ResourceMap({"cpu": 100.5}) of ResourceMap({"cpu": 100.0}) resource-hours over the last 604800s`
	if got := err.Error(); got != want {
		t.Fatalf("Error() =\n%q\nwant\n%q", got, want)
	}
}

// D: rustFloatDebug's NaN/inf spellings and the always-a-decimal-point rule.
func TestRustFloatDebugFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{4, "4.0"},
		{4.5, "4.5"},
		{0, "0.0"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "inf"},
		{math.Inf(-1), "-inf"},
	}
	for _, c := range cases {
		if got := rustFloatDebug(c.in); got != c.want {
			t.Fatalf("rustFloatDebug(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// C5 determinism regression: PriceSheet.price sums amount*price over a
// ResourceMap's keys. Go randomizes map iteration order, and float
// addition is not associative, so without a fixed (sorted) accumulation
// order the result can vary from call to call. Magnitudes are chosen (huge
// alternating with tiny) so summation order actually changes the rounded
// result; running the same accumulation repeatedly must yield a
// bit-identical total.
func TestPriceSheetPriceIsAccumulationOrderDeterministic(t *testing.T) {
	v := ResourceMap{}
	sheet := PriceSheet{}
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("resource-%02d", i)
		if i%2 == 0 {
			v[key] = 1e16
		} else {
			v[key] = 1.0
		}
		sheet[key] = 1.0
	}
	want := sheet.price(v)
	for i := 0; i < 20; i++ {
		if got := sheet.price(v); got != want {
			t.Fatalf("run %d: price() = %v, want %v (non-deterministic accumulation order)", i, got, want)
		}
	}
}

// Requirement 4: a service's demand is head + replicas × worker, one map
// (no min/max — Serve replicas are fixed on the cluster).
func TestServiceDemand(t *testing.T) {
	spec := &core.ServiceSpec{HeadCpu: "1", HeadMemory: "2Gi", WorkerCpu: "2", WorkerMemory: "4Gi", WorkerReplicas: 3}
	d, err := ServiceDemand(spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Cpu() != 7 || d.MemGiB() != 14 || len(d) != 2 {
		t.Fatalf("demand = %v, want cpu 7 memory 14 and no other keys", d)
	}
	zero := &core.ServiceSpec{HeadCpu: "500m", HeadMemory: "1Gi", WorkerCpu: "2", WorkerMemory: "4Gi", WorkerReplicas: 0}
	d, err = ServiceDemand(zero)
	if err != nil || d.Cpu() != 0.5 || d.MemGiB() != 1 {
		t.Fatalf("zero replicas: demand = %v err=%v, want head only", d, err)
	}
	if _, err := ServiceDemand(&core.ServiceSpec{HeadCpu: "lots", HeadMemory: "1Gi", WorkerCpu: "1", WorkerMemory: "1Gi"}); err == nil {
		t.Fatal("unparseable head_cpu must be an error")
	}
}

// An allocation's nominal map reads as a limit in demand units: cpu in
// cores, memory in GiB, anything else as a count.
func TestLimitFromQuantities(t *testing.T) {
	limit, err := LimitFromQuantities(map[string]string{"cpu": "4", "memory": "16Gi", "nvidia.com/gpu": "2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if limit.Cpu() != 4 || limit.MemGiB() != 16 || limit.Gpu() != 2 {
		t.Fatalf("limit = %v", limit)
	}
	if _, err := LimitFromQuantities(map[string]string{"cpu": "many"}); err == nil {
		t.Fatal("unparseable quantity must be an error")
	}
	if got, err := LimitFromQuantities(nil); err != nil || len(got) != 0 {
		t.Fatalf("nil map: %v %v", got, err)
	}
}
