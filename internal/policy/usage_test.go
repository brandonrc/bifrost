package policy

import (
	"fmt"
	"math"
	"testing"
)

// Ported from the predecessor's policy crate, src/usage.rs #[cfg(test)] mod tests.

func samples(pairs ...[2]float64) []UsageSampleView {
	out := make([]UsageSampleView, len(pairs))
	for i, p := range pairs {
		out[i] = UsageSampleView{TS: uint64(p[0]), Quantity: p[1]}
	}
	return out
}

func hoursApprox(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) >= 1e-9 {
		t.Fatalf("got %v, want ~%v", got, want)
	}
}

func TestConstantSeries(t *testing.T) {
	// 4 cores held for the whole 3600s window = 4 core-hours.
	s := samples([2]float64{0, 4.0})
	hoursApprox(t, ResourceHours(s, 0, 3600), 4.0)
}

func TestStepChangeMidWindow(t *testing.T) {
	// 4 cores for the first half, 8 for the second.
	s := samples([2]float64{0, 4.0}, [2]float64{1800, 8.0})
	hoursApprox(t, ResourceHours(s, 0, 3600), 2.0+4.0)
}

func TestCarryInFromBeforeFrom(t *testing.T) {
	// A sample before the window sets the level entering it: 10 cores
	// held from t=50 carries into [100, 200] and holds to the end.
	s := samples([2]float64{50, 10.0})
	hoursApprox(t, ResourceHours(s, 100, 200), 10.0*100.0/3600.0)
	// A later pre-window sample overrides an earlier one.
	s = samples([2]float64{10, 1.0}, [2]float64{90, 5.0})
	hoursApprox(t, ResourceHours(s, 100, 200), 5.0*100.0/3600.0)
}

func TestNoCarryInMeansZeroUntilFirstSample(t *testing.T) {
	s := samples([2]float64{150, 10.0})
	// [100, 200]: 0 for [100,150), then 10 for [150,200).
	hoursApprox(t, ResourceHours(s, 100, 200), 10.0*50.0/3600.0)
}

func TestEmptyIsZero(t *testing.T) {
	if got := ResourceHours(nil, 0, 3600); got != 0.0 {
		t.Fatalf("got %v, want 0.0", got)
	}
}

func TestWindowClamping(t *testing.T) {
	// Samples outside the window on both sides: level at `from` is 2
	// (from t=0), the t=2000 sample is beyond `to` and never applies.
	s := samples([2]float64{0, 2.0}, [2]float64{2000, 9.0})
	hoursApprox(t, ResourceHours(s, 100, 1000), 2.0*900.0/3600.0)
	// A sample exactly at `to` contributes nothing.
	s = samples([2]float64{0, 2.0}, [2]float64{1000, 9.0})
	hoursApprox(t, ResourceHours(s, 100, 1000), 2.0*900.0/3600.0)
	// Degenerate window.
	if got := ResourceHours(s, 500, 500); got != 0.0 {
		t.Fatalf("got %v, want 0.0", got)
	}
	if got := ResourceHours(s, 900, 100); got != 0.0 {
		t.Fatalf("got %v, want 0.0", got)
	}
}

func TestGapStepHoldsLastKnownState(t *testing.T) {
	// Sampler was down for hours: the last reading persists across the
	// gap instead of interpolating to zero.
	s := samples([2]float64{0, 4.0}, [2]float64{7200, 4.0})
	hoursApprox(t, ResourceHours(s, 0, 7200), 8.0)
}

func TestUnsortedInputIsSorted(t *testing.T) {
	s := samples([2]float64{1800, 8.0}, [2]float64{0, 4.0})
	hoursApprox(t, ResourceHours(s, 0, 3600), 6.0)
}

func TestCostRollsUpPricedAndUnpricedKeys(t *testing.T) {
	hrs := rmap(map[string]float64{
		"cpu":                 10.0,  // 10 x 0.04 = 0.40
		"memory":              100.0, // 100 x 0.005 = 0.50
		"example.com/license": 7.0,   // unpriced -> 0
	})
	sheet := PriceSheet{"cpu": 0.04, "memory": 0.005}
	hoursApprox(t, Cost(hrs, sheet), 0.90)
	// Empty sheet prices everything at 0.
	if got := Cost(hrs, nil); got != 0.0 {
		t.Fatalf("got %v, want 0.0", got)
	}
}

func TestWindowedHoursSumsAcrossPoolsPerResource(t *testing.T) {
	// proj-a ran in two pools during the window; GPU-hours sum across
	// both, CPU only appears in pool gpu.
	by := map[PoolResource][]UsageSampleView{
		// pool "gpu": 2 GPUs held the whole 3600s window = 2 GPU-hours.
		{Pool: "gpu", Resource: "nvidia.com/gpu"}: samples([2]float64{0, 2.0}),
		{Pool: "gpu", Resource: "cpu"}:            samples([2]float64{0, 8.0}),
		// pool "gpu2": 1 GPU held the whole window = 1 GPU-hour.
		{Pool: "gpu2", Resource: "nvidia.com/gpu"}: samples([2]float64{0, 1.0}),
	}
	hrs := WindowedResourceHours(by, 0, 3600)
	hoursApprox(t, hrs.Gpu(), 3.0) // 2 + 1 GPU-hours
	hoursApprox(t, hrs.Cpu(), 8.0)
}

func TestWindowedHoursRespectsWindowCarryIn(t *testing.T) {
	// A reading before `from` carries into the window (older usage that
	// has not yet aged out still counts against the trailing window).
	by := map[PoolResource][]UsageSampleView{
		{Pool: "p", Resource: "cpu"}: samples([2]float64{0, 4.0}),
	}
	// Window [1800, 5400): 4 cores held the whole hour = 4 core-hours.
	hrs := WindowedResourceHours(by, 1800, 5400)
	hoursApprox(t, hrs.Cpu(), 4.0)
	if got := WindowedResourceHours(nil, 0, 3600); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestSampleViewIsTheDocumentedInputShape(t *testing.T) {
	// UsageSampleView exists so callers can project their own row type;
	// it carries exactly (ts, quantity).
	v := UsageSampleView{TS: 0, Quantity: 2.0}
	hoursApprox(t, ResourceHours([]UsageSampleView{v}, 0, 3600), 2.0)
}

// Fix-round-1 regression test (review finding M3): the sort must be
// stable, mirroring Rust's sort_by_key, so duplicate timestamps keep their
// input relative order instead of making the integration order-dependent.
func TestResourceHoursStableOnDuplicateTimestamps(t *testing.T) {
	// Three readings all at t=100, given in this order; a stable sort
	// preserves that order, so the level after t=100 is the *last* one
	// given (3.0), holding to the window end at t=200: 3.0 cores for
	// 100s = 300/3600 core-hours. An unstable sort could pick any of the
	// three as "last" and change the result.
	s := samples([2]float64{100, 1.0}, [2]float64{100, 2.0}, [2]float64{100, 3.0})
	hoursApprox(t, ResourceHours(s, 0, 200), 300.0/3600.0)
}

// Strengthens TestResourceHoursStableOnDuplicateTimestamps: Go's sort.Slice
// (pattern-defeating quicksort) falls back to a stable insertion sort for
// small slices, so a handful of tied elements doesn't actually discriminate
// sort.Slice from sort.SliceStable — the instability only becomes
// observable once the slice is large enough to leave that fast path. 30
// same-timestamp samples, fed in ascending order, forces the
// discrimination.
func TestResourceHoursStableOnManyDuplicateTimestamps(t *testing.T) {
	const n = 30
	pairs := make([][2]float64, n)
	for i := range pairs {
		pairs[i] = [2]float64{100, float64(i + 1)}
	}
	s := samples(pairs...)
	// A stable sort preserves input order, so the last-given quantity (n)
	// is the one holding from t=100 to the window end at t=200: n cores
	// for 100s.
	hoursApprox(t, ResourceHours(s, 0, 200), float64(n)*100.0/3600.0)
}

// C5 determinism regression: Cost sums hours*price over a ResourceMap's
// keys. Go randomizes map iteration order, and float addition is not
// associative, so without a fixed (sorted) accumulation order the result
// can vary from call to call. Magnitudes are chosen (huge alternating with
// tiny) so summation order actually changes the rounded result; running
// the same accumulation repeatedly must yield a bit-identical total.
func TestCostIsAccumulationOrderDeterministic(t *testing.T) {
	hours := ResourceMap{}
	sheet := PriceSheet{}
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("resource-%02d", i)
		if i%2 == 0 {
			hours[key] = 1e16
		} else {
			hours[key] = 1.0
		}
		sheet[key] = 1.0
	}
	want := Cost(hours, sheet)
	for i := 0; i < 20; i++ {
		if got := Cost(hours, sheet); got != want {
			t.Fatalf("run %d: Cost() = %v, want %v (non-deterministic accumulation order)", i, got, want)
		}
	}
}

// C5 determinism regression, WindowedResourceHours: the per-(pool,resource)
// hours are summed into out[resource] in map iteration order; without a
// fixed (sorted) accumulation order the per-resource total can vary from
// call to call. Many pools sharing one resource, with alternating huge/tiny
// hour magnitudes, make the accumulation order observable.
func TestWindowedResourceHoursIsAccumulationOrderDeterministic(t *testing.T) {
	by := map[PoolResource][]UsageSampleView{}
	for i := 0; i < 30; i++ {
		pool := fmt.Sprintf("pool-%02d", i)
		qty := 1.0
		if i%2 == 0 {
			qty = 1e16
		}
		by[PoolResource{Pool: pool, Resource: "cpu"}] = samples([2]float64{0, qty})
	}
	want := WindowedResourceHours(by, 0, 3600)
	for i := 0; i < 20; i++ {
		got := WindowedResourceHours(by, 0, 3600)
		if got["cpu"] != want["cpu"] {
			t.Fatalf("run %d: WindowedResourceHours()[cpu] = %v, want %v (non-deterministic accumulation order)", i, got["cpu"], want["cpu"])
		}
	}
}
