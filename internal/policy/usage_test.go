package policy

import (
	"math"
	"testing"
)

// Ported from mobula-policy/src/usage.rs #[cfg(test)] mod tests.

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
