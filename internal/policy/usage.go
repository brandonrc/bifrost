package policy

// Usage aggregation: turn the append-only usage-sample timeseries into
// resource-hours and dollars. Pure functions over plain input shapes,
// decoupled from the controller's stored sample type so this package stays
// storage-agnostic. Ported from the predecessor's policy crate, src/usage.rs.

import "sort"

// UsageSampleView is one usage sample as the aggregator sees it: a
// quantity reading at TS (unix seconds). Decoupled from any store-specific
// sample type on purpose — the caller projects its own rows down to this
// shape.
type UsageSampleView struct {
	TS       uint64
	Quantity float64
}

// ResourceHours integrates a usage timeseries over [from, to] (unix
// seconds) into resource-hours: sum(qty_i * held_seconds) / 3600.
//
// The series is a step function: a sample's quantity holds until the next
// sample changes it (samples are point readings of a level, not deltas).
// Rules:
//
//   - Carry-in: the last sample at-or-before from establishes the level
//     entering the window. With no such sample the level is 0 until the
//     first in-window sample (unknown history reads as zero, never an
//     invented value).
//   - Clamping: the window edges from/to bound the integration; a sample's
//     level never extends past to, and levels entering at from start
//     accruing there.
//   - Sampler gaps: a gap longer than the sampling cadence (e.g. the
//     metering loop was down) is treated as "last known state persisted"
//     — the step still holds across it. This is simple and honest: the
//     gap is visible in the sample density, and no interpolation invents
//     usage.
//
// Input need not be sorted; it is sorted internally. from >= to yields 0.
func ResourceHours(samples []UsageSampleView, from, to uint64) float64 {
	if from >= to {
		return 0
	}
	pts := make([]UsageSampleView, len(samples))
	copy(pts, samples)
	// Stable: Rust's sort_by_key is a stable sort, so samples sharing a
	// timestamp must keep their input relative order — an unstable sort
	// would make the integration result order-dependent on ties.
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].TS < pts[j].TS })

	level := 0.0
	cursor := from
	seconds := 0.0
	for _, p := range pts {
		if p.TS <= from {
			// Carry-in: keep the latest level at-or-before the window start.
			level = p.Quantity
			continue
		}
		if p.TS >= to {
			break
		}
		seconds += level * float64(p.TS-cursor)
		cursor = p.TS
		level = p.Quantity
	}
	// The final level holds to the window end.
	seconds += level * float64(to-cursor)
	return seconds / 3600.0
}

// PoolResource identifies a (pool, resource) series in WindowedResourceHours'
// input — the same key convention as the predecessor's policy::usage's tuple key.
type PoolResource struct {
	Pool     string
	Resource string
}

// WindowedResourceHours computes windowed cumulative consumption per
// resource (#77), summed correctly across pools. Input is keyed by
// (pool, resource) -> the step-series of (ts, quantity) readings; each
// series is integrated over [from, to] with ResourceHours (carry-in and
// clamping included) and the per-pool hours are summed per resource.
//
// Series from different pools must NOT be interleaved into one step series
// (readings of different levels), so grouping by pool is load-bearing — it
// matches how GET /api/v1/usage groups by (project, pool). A project
// normally lives in one pool, but a re-homed project can have samples in
// two; this sums them honestly.
func WindowedResourceHours(byPoolResource map[PoolResource][]UsageSampleView, from, to uint64) ResourceMap {
	// Accumulate in sorted (pool, resource) key order — Rust's reference
	// keys this map with a BTreeMap<(String, String), _>, whose iteration
	// order is the tuple's lexicographic order, so summing the per-pool
	// hours into out[resource] in a Go map's (random) iteration order
	// would make float-summation rounding order-dependent where Rust is
	// deterministic. Sorting first makes the two match.
	keys := make([]PoolResource, 0, len(byPoolResource))
	for k := range byPoolResource {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Pool != keys[j].Pool {
			return keys[i].Pool < keys[j].Pool
		}
		return keys[i].Resource < keys[j].Resource
	})
	out := ResourceMap{}
	for _, key := range keys {
		hours := ResourceHours(byPoolResource[key], from, to)
		out[key.Resource] += hours
	}
	return out
}

// Cost is the dollar cost of a per-resource hours roll-up under a price
// sheet: sum(hours_r * price_r). Resources absent from the sheet price at
// 0 (an unpriced resource is free for estimation, never an error — same
// rule as PriceSheet.Estimate).
func Cost(quantityHoursPerResource ResourceMap, sheet PriceSheet) float64 {
	// Sorted key order for the same reason as WindowedResourceHours:
	// deterministic float-summation order matching Rust's BTreeMap.
	keys := make([]string, 0, len(quantityHoursPerResource))
	for k := range quantityHoursPerResource {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var total float64
	for _, k := range keys {
		total += quantityHoursPerResource[k] * sheet[k]
	}
	return total
}
