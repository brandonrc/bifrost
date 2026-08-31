// Package policy holds governance policy for Bifrost: resource accounting,
// cost estimation, and quota admission. Pure and provider-agnostic — the
// reconciler and API call in; nothing here touches Kubernetes or a live
// autoscaler (Ray owns scaling; we shape bounds and enforce quota, per
// ADR-0007 and the literature audit's "quota is admission control").
//
// Resource accounting is keyed by arbitrary Kubernetes resource names
// (ADR-0010): pools and Kueue quota any resource name, so the fixed
// cpu/gpu/memory vector generalizes to ResourceMap. The well-known keys
// are CPU (cores), Memory (GiB, not bytes — the map keeps the old mem_gib
// semantics under the K8s resource name), and GPU (devices).
//
// Ported from mobula-policy (Rust). Field names and enum wire values
// follow the frozen OpenAPI contract (serde is the wire-format arbiter in
// the Rust reference).
package policy

import (
	"encoding/json"
	"fmt"

	"github.com/brandonrc/bifrost/internal/core"
)

// Well-known resource keys (any other K8s resource name is equally valid).
const (
	CPU    = "cpu"
	Memory = "memory"
	GPU    = "nvidia.com/gpu"
)

// ResourceMap is a multi-resource demand/quota map: resource name -> amount.
//
// Amounts are plain float64 in the key's natural unit (cores for cpu, GiB
// for memory, devices for nvidia.com/gpu). A missing key means zero — maps
// are sparse, so demand for a resource a quota doesn't mention is rejected
// by FitsWithin.
//
// #[serde(transparent)] in the Rust reference: marshals/unmarshals as a
// bare JSON object, never wrapped. MarshalJSON substitutes {} for a nil
// map, mirroring Rust's BTreeMap::default() (frozen contract fields typed
// as required objects, e.g. PolicyView.quotas values, must never be null).
type ResourceMap map[string]float64

func (r ResourceMap) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]float64(r))
}

// Clone returns a shallow copy, mirroring Rust's explicit .clone() at call
// sites that need an independent map (ResourceMap otherwise behaves as an
// immutable value throughout this package: every method below builds and
// returns a new map rather than mutating its receiver).
func (r ResourceMap) Clone() ResourceMap {
	out := make(ResourceMap, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

// Add returns the union of keys; shared keys sum.
func (r ResourceMap) Add(o ResourceMap) ResourceMap {
	out := r.Clone()
	for k, v := range o {
		out[k] += v
	}
	return out
}

// Scale returns r with every value multiplied by n.
func (r ResourceMap) Scale(n float64) ResourceMap {
	out := make(ResourceMap, len(r))
	for k, v := range r {
		out[k] = v * n
	}
	return out
}

// FitsWithin reports whether every key in r is <= limit's value for that
// key. A key missing from limit counts as 0, so any demand for an unlisted
// resource does not fit. The predicate is written as !(v <= limit[k]),
// not v > limit[k], so a NaN value denies (as Rust's `v <= limit` does:
// NaN compares false against everything, so `!(NaN <= x)` is true and the
// negated form here matches).
func (r ResourceMap) FitsWithin(limit ResourceMap) bool {
	for k, v := range r {
		if !(v <= limit[k]) {
			return false
		}
	}
	return true
}

// CPU returns cores under the well-known cpu key (0 when absent).
func (r ResourceMap) Cpu() float64 { return r[CPU] }

// GPU returns devices under the well-known nvidia.com/gpu key (0 when
// absent).
func (r ResourceMap) Gpu() float64 { return r[GPU] }

// MemGiB returns GiB under the well-known memory key (0 when absent).
func (r ResourceMap) MemGiB() float64 { return r[Memory] }

// QuantityError reports a quantity string that failed to parse, or a
// structurally invalid demand (e.g. min_replicas > max_replicas).
// Corresponds to the single-variant Rust PolicyError::Quantity(String).
type QuantityError struct {
	Msg string
}

func (e QuantityError) Error() string {
	return fmt.Sprintf("invalid quantity: %s", e.Msg)
}

func wrapQuantity(err error) error {
	if err == nil {
		return nil
	}
	return QuantityError{Msg: err.Error()}
}

func workerUnit(g *core.WorkerGroup) (ResourceMap, error) {
	cpu, err := CPUCores(g.Cpu)
	if err != nil {
		return nil, wrapQuantity(err)
	}
	mem, err := MemGiB(g.Memory)
	if err != nil {
		return nil, wrapQuantity(err)
	}
	m := ResourceMap{CPU: cpu, Memory: mem}
	gpu, err := GPUCount(g.Gpu)
	if err != nil {
		return nil, wrapQuantity(err)
	}
	if gpu > 0 {
		m[GPU] = gpu
	}
	return m, nil
}

func headUnit(spec *core.ClusterSpec) (ResourceMap, error) {
	cpu, err := CPUCores(spec.HeadCpu)
	if err != nil {
		return nil, wrapQuantity(err)
	}
	mem, err := MemGiB(spec.HeadMemory)
	if err != nil {
		return nil, wrapQuantity(err)
	}
	return ResourceMap{CPU: cpu, Memory: mem}, nil
}

// ClusterDemand is the resource demand of a cluster at its minimum and
// maximum size. min = head + sum(worker_unit * min_replicas); max = head +
// sum(worker_unit * max_replicas). Quota admits against max (worst case,
// conservative — Borg oversells at low priority; that refinement is future
// work).
//
// Emits exactly the keys cpu and memory (GiB), plus nvidia.com/gpu when a
// worker group requests GPUs.
func ClusterDemand(spec *core.ClusterSpec) (min, max ResourceMap, err error) {
	head, err := headUnit(spec)
	if err != nil {
		return nil, nil, err
	}
	min = head
	max = head.Clone()
	for i := range spec.WorkerGroups {
		g := &spec.WorkerGroups[i]
		// A group with min > max is nonsensical and would make the "max"
		// demand smaller than the min — quota admits against max, so this
		// must be rejected, not silently mischarged.
		if g.MinReplicas > g.MaxReplicas {
			return nil, nil, QuantityError{Msg: fmt.Sprintf(
				"worker group %s: min_replicas (%d) > max_replicas (%d)",
				g.Name, g.MinReplicas, g.MaxReplicas)}
		}
		unit, err := workerUnit(g)
		if err != nil {
			return nil, nil, err
		}
		min = min.Add(unit.Scale(float64(g.MinReplicas)))
		max = max.Add(unit.Scale(float64(g.MaxReplicas)))
	}
	return min, max, nil
}

// PriceSheet is the hourly price per unit of each resource key (pluggable;
// a static sheet is fine at v0). Deserialized from config as a flat map of
// resource name -> price: cpu = $/core-hour, memory = $/GiB-hour,
// nvidia.com/gpu = $/GPU-hour. Keys absent from the sheet price at 0 — an
// unpriced resource is free for estimation purposes, never an error.
//
// #[serde(transparent)] in the Rust reference; MarshalJSON substitutes {}
// for a nil map for the same reason as ResourceMap.
type PriceSheet map[string]float64

func (p PriceSheet) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]float64(p))
}

func (p PriceSheet) price(v ResourceMap) float64 {
	var total float64
	for k, amount := range v {
		total += amount * p[k]
	}
	return total
}

// CostEstimate is the estimated $/hr at the cluster's min and max size.
type CostEstimate struct {
	MinHourly float64
	MaxHourly float64
}

// Estimate computes the estimated $/hr at the cluster's min and max size.
func (p PriceSheet) Estimate(spec *core.ClusterSpec) (CostEstimate, error) {
	min, max, err := ClusterDemand(spec)
	if err != nil {
		return CostEstimate{}, err
	}
	return CostEstimate{MinHourly: p.price(min), MaxHourly: p.price(max)}, nil
}

// QuotaExceeded reports that a project's requested max demand plus its
// other clusters' in-use demand exceeds its quota limit.
type QuotaExceeded struct {
	Project   string
	Requested ResourceMap
	InUse     ResourceMap
	Limit     ResourceMap
}

func (e QuotaExceeded) Error() string {
	return fmt.Sprintf(
		"project %s quota exceeded: requested max %v + in-use %v exceeds limit %v",
		e.Project, e.Requested, e.InUse, e.Limit)
}

// AdmitQuota is the admission check (Borg: quota is admission control).
// inUse is the summed max-demand of the project's *other* clusters;
// requested is the max-demand of the cluster being created/updated. Admits
// iff the total fits within limit on every resource key.
func AdmitQuota(project string, limit, inUse, requested ResourceMap) error {
	total := inUse.Add(requested)
	if total.FitsWithin(limit) {
		return nil
	}
	return QuotaExceeded{Project: project, Requested: requested, InUse: inUse, Limit: limit}
}

// Budget is a time-windowed compute budget (#77): a cap on *cumulative*
// consumption over a trailing window, distinct from the AdmitQuota quota
// which caps *concurrent* live demand. Limits is resource name ->
// resource-hours allowed over the last WindowSecs seconds (cpu =
// core-hours, memory = GiB-hours, nvidia.com/gpu = GPU-hours, and any
// extended K8s resource name is equally valid — same key convention as
// ResourceMap).
//
// The wire shape mirrors the [budgets] config shape with an extra
// window_secs key (matches the frozen contract's BudgetView schema):
// window_secs is a named field, and every other JSON key flattens into
// Limits — mirroring serde's #[serde(flatten)] on the Rust reference.
type Budget struct {
	WindowSecs uint64
	Limits     map[string]float64
}

func (b Budget) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(b.Limits)+1)
	for k, v := range b.Limits {
		m[k] = v
	}
	m["window_secs"] = b.WindowSecs
	return json.Marshal(m)
}

func (b *Budget) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// window_secs is a required field on the wire (BudgetView's frozen
	// schema lists it under `required`, and Rust's serde derive errors on
	// a missing non-Option field). Silently defaulting to 0 here would
	// produce a zero-width window that makes AdmitBudget a permanent
	// no-op — fail loudly instead.
	wsRaw, ok := raw["window_secs"]
	if !ok {
		return fmt.Errorf("policy: Budget missing required field \"window_secs\"")
	}
	if err := json.Unmarshal(wsRaw, &b.WindowSecs); err != nil {
		return err
	}
	delete(raw, "window_secs")
	limits := make(map[string]float64, len(raw))
	for k, v := range raw {
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return err
		}
		limits[k] = f
	}
	b.Limits = limits
	return nil
}

// LimitMap returns the cap map as a ResourceMap.
func (b Budget) LimitMap() ResourceMap {
	out := make(ResourceMap, len(b.Limits))
	for k, v := range b.Limits {
		out[k] = v
	}
	return out
}

// BudgetExceeded reports that a project's windowed consumption has reached
// or passed its budget cap on at least one resource.
type BudgetExceeded struct {
	Project    string
	Consumed   ResourceMap
	Limit      ResourceMap
	WindowSecs uint64
}

func (e BudgetExceeded) Error() string {
	return fmt.Sprintf(
		"project %s budget exceeded: consumed %v of %v resource-hours over the last %ds",
		e.Project, e.Consumed, e.Limit, e.WindowSecs)
}

// AdmitBudget is time-windowed budget admission (#77). consumed is the
// project's cumulative resource-hours over the trailing budget.WindowSecs,
// derived from the metering usage samples (see WindowedResourceHours).
//
// Enforcement model (v1, deliberately simple and documented): admit iff the
// *already-consumed* windowed usage is strictly below the cap on every
// resource the budget lists. We do NOT project the new cluster's future
// consumption onto the window — a cluster's lifetime is unknown at
// admission (TTL is optional, autoscaling is Ray's), so any projection
// would be a guess. Blocking on consumed >= cap is the honest floor: once a
// project has burned its window allowance it can create nothing new until
// the window rolls forward and older usage ages out. A resource the budget
// does not list is unconstrained. A cap of 0 admits nothing for that
// resource.
func AdmitBudget(project string, budget *Budget, consumed ResourceMap) error {
	for resource, limit := range budget.Limits {
		if consumed[resource] >= limit {
			return BudgetExceeded{
				Project:    project,
				Consumed:   consumed,
				Limit:      budget.LimitMap(),
				WindowSecs: budget.WindowSecs,
			}
		}
	}
	return nil
}
