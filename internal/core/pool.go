package core

import (
	"encoding/json"
	"fmt"
	"math"
)

// Resource pool domain types (ADR-0010).
//
// A ResourcePool is Bifrost's unit of shared capacity: a set of hardware
// flavors drawing from a common cohort, with per-project allocations.
// These types are provider-agnostic; the translation to Kueue objects
// (ResourceFlavor / ClusterQueue / LocalQueue) lives outside this package,
// and quantity *parseability* validation lives in the policy package —
// core validates shape (names, structure), never quantity syntax.
//
// Resource keys are arbitrary Kubernetes resource names (cpu, memory,
// nvidia.com/gpu, nvidia.com/mig-1g.10gb, example.com/license, …); there is
// deliberately no hard-coded key list, matching Kueue's ability to quota
// any resource name.

// IsK8sName reports whether s is an RFC 1123 subdomain: lowercase
// alphanumerics, `-` and `.`, starting and ending alphanumeric, <= 253
// chars. Kueue object names (flavors, queues, cohorts), namespaces, and
// local-auth usernames all follow this.
func IsK8sName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.') {
			return false
		}
	}
	first, last := s[0], s[len(s)-1]
	isAlnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
	return isAlnum(first) && isAlnum(last)
}

// GpuSharing is how a pool's GPUs may be shared between workloads (#58).
//
// NVIDIA GPU time-slicing (and fractional nvidia.com/gpu requests via the
// device plugin) shares one GPU's SMs across processes with no hardware
// isolation — acceptable within one tenant, never across tenants. MIG is
// hardware partitioning and whole-GPU allocation needs no sharing at all,
// so both are isolation-safe. The tenant-isolation rule itself is enforced
// by the policy package at admission time; this field is the per-pool knob
// it evaluates.
type GpuSharing string

const (
	// GpuSharingWholeGpu: one workload per GPU (the safe default).
	GpuSharingWholeGpu GpuSharing = "whole-gpu"
	// GpuSharingMig: MIG hardware partitioning — isolation-safe sharing.
	GpuSharingMig GpuSharing = "mig"
	// GpuSharingTimeSlice: device-plugin time-slicing — software sharing,
	// single-tenant pools only.
	GpuSharingTimeSlice GpuSharing = "time-slice"
)

// DefaultGpuSharing is the safe default (mirrors Rust's #[derive(Default)]
// on GpuSharing).
const DefaultGpuSharing = GpuSharingWholeGpu

func (g GpuSharing) isValid() bool {
	switch g {
	case GpuSharingWholeGpu, GpuSharingMig, GpuSharingTimeSlice:
		return true
	}
	return false
}

// UnmarshalJSON rejects any value other than the known GpuSharing variants,
// mirroring serde's strict enum deserialization.
func (g *GpuSharing) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v := GpuSharing(s)
	if !v.isValid() {
		return fmt.Errorf("core: invalid GpuSharing %q", s)
	}
	*g = v
	return nil
}

// PoolSpec is a shared capacity pool: flavors + a cohort to borrow from
// (ADR-0010).
type PoolSpec struct {
	Name    string       `json:"name"`
	Flavors []FlavorSpec `json:"flavors"`
	// Cohort is the name of the Kueue cohort this pool's ClusterQueue
	// joins for elastic borrowing.
	Cohort string `json:"cohort"`
	// FairSharingWeight is Kueue `spec.fairSharing.weight` for the pool's
	// ClusterQueue.
	FairSharingWeight float64 `json:"fair_sharing_weight"`
	// Elastic is whether workloads in this pool may be elastically
	// resized (Kueue elastic jobs / Workload Slices).
	Elastic bool `json:"elastic"`
	// GpuSharing is the GPU sharing mode for this pool (#58). nil inherits
	// the platform default ([gpu] default_sharing in the policy file,
	// itself defaulting to whole-gpu). A pool shared by more than one
	// project may not resolve to time-slice — enforced at admission by
	// the policy package, not here (core validates shape; tenancy is
	// known only at the API edge, where allocations live).
	GpuSharing *GpuSharing `json:"gpu_sharing,omitempty"`
}

// poolSpecAlias breaks the recursion MarshalJSON would otherwise cause by
// re-entering PoolSpec's own MarshalJSON.
type poolSpecAlias PoolSpec

// MarshalJSON substitutes an empty slice for a nil Flavors, mirroring
// Rust's Vec::default(), which serde always writes as `[]`, never `null`
// (the frozen contract's PoolSpec schema types `flavors` as an array).
func (p PoolSpec) MarshalJSON() ([]byte, error) {
	a := poolSpecAlias(p)
	if a.Flavors == nil {
		a.Flavors = []FlavorSpec{}
	}
	return json.Marshal(a)
}

// PoolSpecErrorKind discriminates PoolSpec.Validate failures.
type PoolSpecErrorKind int

const (
	PoolSpecErrInvalidName PoolSpecErrorKind = iota
	PoolSpecErrInvalidCohort
	PoolSpecErrNoFlavors
	PoolSpecErrDuplicateFlavor
	PoolSpecErrInvalidFairSharingWeight
	PoolSpecErrFlavor
)

// PoolSpecError reports why a PoolSpec failed validation.
type PoolSpecError struct {
	Kind PoolSpecErrorKind
	// Name holds the offending pool/cohort/flavor name for InvalidName,
	// InvalidCohort, and DuplicateFlavor.
	Name string
	// Flavor and Source are set for the Flavor variant: the flavor whose
	// validation failed, and why.
	Flavor string
	Source FlavorSpecError
}

func (e PoolSpecError) Error() string {
	switch e.Kind {
	case PoolSpecErrInvalidName:
		return fmt.Sprintf("pool name %q is not a valid Kubernetes name (RFC 1123 subdomain)", e.Name)
	case PoolSpecErrInvalidCohort:
		return fmt.Sprintf("cohort name %q is not a valid Kubernetes name (RFC 1123 subdomain)", e.Name)
	case PoolSpecErrNoFlavors:
		return "pool must declare at least one flavor"
	case PoolSpecErrDuplicateFlavor:
		return fmt.Sprintf("duplicate flavor name %q", e.Name)
	case PoolSpecErrInvalidFairSharingWeight:
		return "fair_sharing_weight must be a finite, non-negative number"
	case PoolSpecErrFlavor:
		return fmt.Sprintf("flavor %s: %s", e.Flavor, e.Source.Error())
	}
	return "pool spec error"
}

// Validate checks a PoolSpec's shape: names, flavor uniqueness, and the
// fair-sharing weight. It never validates quantity syntax (that is the
// policy package's job).
func (p *PoolSpec) Validate() error {
	if !IsK8sName(p.Name) {
		return PoolSpecError{Kind: PoolSpecErrInvalidName, Name: p.Name}
	}
	if !IsK8sName(p.Cohort) {
		return PoolSpecError{Kind: PoolSpecErrInvalidCohort, Name: p.Cohort}
	}
	if len(p.Flavors) == 0 {
		return PoolSpecError{Kind: PoolSpecErrNoFlavors}
	}
	if !isFiniteNonNegative(p.FairSharingWeight) {
		return PoolSpecError{Kind: PoolSpecErrInvalidFairSharingWeight}
	}
	for i, f := range p.Flavors {
		if err := f.Validate(); err != nil {
			fse, ok := err.(FlavorSpecError)
			if !ok {
				return fmt.Errorf("core: unexpected error type from FlavorSpec.Validate: %w", err)
			}
			return PoolSpecError{Kind: PoolSpecErrFlavor, Flavor: f.Name, Source: fse}
		}
		for _, o := range p.Flavors[:i] {
			if o.Name == f.Name {
				return PoolSpecError{Kind: PoolSpecErrDuplicateFlavor, Name: f.Name}
			}
		}
	}
	return nil
}

func isFiniteNonNegative(w float64) bool {
	return !math.IsNaN(w) && !math.IsInf(w, 0) && w >= 0.0
}

// FlavorSpec is a hardware flavor within a pool: node selection plus
// per-resource nominal quota (K8s quantity strings, e.g. "4", "512Gi").
type FlavorSpec struct {
	Name string `json:"name"`
	// Resources maps resource key -> nominal quota quantity string.
	Resources  map[string]string `json:"resources"`
	NodeLabels map[string]string `json:"node_labels"`
	Taints     []TaintSpec       `json:"taints"`
}

// flavorSpecAlias breaks the recursion MarshalJSON would otherwise cause
// by re-entering FlavorSpec's own MarshalJSON.
type flavorSpecAlias FlavorSpec

// MarshalJSON substitutes an empty map/slice for a nil Resources,
// NodeLabels, or Taints, mirroring Rust's Vec/HashMap ::default(), which
// serde always writes as `{}`/`[]`, never `null`.
func (f FlavorSpec) MarshalJSON() ([]byte, error) {
	a := flavorSpecAlias(f)
	if a.Resources == nil {
		a.Resources = map[string]string{}
	}
	if a.NodeLabels == nil {
		a.NodeLabels = map[string]string{}
	}
	if a.Taints == nil {
		a.Taints = []TaintSpec{}
	}
	return json.Marshal(a)
}

// FlavorSpecErrorKind discriminates FlavorSpec.Validate failures.
type FlavorSpecErrorKind int

const (
	FlavorSpecErrInvalidName FlavorSpecErrorKind = iota
	FlavorSpecErrEmptyResourceKey
	FlavorSpecErrTaint
)

// FlavorSpecError reports why a FlavorSpec failed validation.
type FlavorSpecError struct {
	Kind FlavorSpecErrorKind
	Name string // InvalidName
	Key  string // Taint: the offending taint's key
	// TaintSource is set for the Taint variant.
	TaintSource TaintSpecError
}

func (e FlavorSpecError) Error() string {
	switch e.Kind {
	case FlavorSpecErrInvalidName:
		return fmt.Sprintf("flavor name %q is not a valid Kubernetes name (RFC 1123 subdomain)", e.Name)
	case FlavorSpecErrEmptyResourceKey:
		return "resource key must be non-empty"
	case FlavorSpecErrTaint:
		return fmt.Sprintf("taint %q: %s", e.Key, e.TaintSource.Error())
	}
	return "flavor spec error"
}

// Validate checks a FlavorSpec's shape.
func (f *FlavorSpec) Validate() error {
	if !IsK8sName(f.Name) {
		return FlavorSpecError{Kind: FlavorSpecErrInvalidName, Name: f.Name}
	}
	for k := range f.Resources {
		if k == "" {
			return FlavorSpecError{Kind: FlavorSpecErrEmptyResourceKey}
		}
	}
	for _, t := range f.Taints {
		if err := t.Validate(); err != nil {
			tse, ok := err.(TaintSpecError)
			if !ok {
				return fmt.Errorf("core: unexpected error type from TaintSpec.Validate: %w", err)
			}
			return FlavorSpecError{Kind: FlavorSpecErrTaint, Key: t.Key, TaintSource: tse}
		}
	}
	return nil
}

// TaintSpec is a Kubernetes taint on a flavor's nodes. Effect is e.g.
// "NoSchedule"; validated non-empty here (the set of legal effects is
// Kubernetes', not ours).
type TaintSpec struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// TaintSpecError is a sentinel error for TaintSpec.Validate failures — the
// Rust reference's TaintSpecError enum has no payload on any variant.
type TaintSpecError string

const (
	ErrTaintEmptyKey    TaintSpecError = "taint key must be non-empty"
	ErrTaintEmptyValue  TaintSpecError = "taint value must be non-empty"
	ErrTaintEmptyEffect TaintSpecError = "taint effect must be non-empty (e.g. \"NoSchedule\")"
)

func (e TaintSpecError) Error() string { return string(e) }

// Validate checks a TaintSpec's shape.
func (t TaintSpec) Validate() error {
	if t.Key == "" {
		return ErrTaintEmptyKey
	}
	if t.Value == "" {
		return ErrTaintEmptyValue
	}
	if t.Effect == "" {
		return ErrTaintEmptyEffect
	}
	return nil
}

// AllocationSpec is a project's allocation within a pool (translates to a
// Kueue LocalQueue).
//
// Nominal / BorrowingLimit / LendingLimit are reserved for a future
// per-project ClusterQueue layout (ADR-0010): in the v0 layout all
// allocations in a pool share one ClusterQueue, so these are recorded as
// LocalQueue annotations rather than enforced quotas.
type AllocationSpec struct {
	Pool           string            `json:"pool"`
	Project        string            `json:"project"`
	Namespace      string            `json:"namespace"`
	Nominal        map[string]string `json:"nominal"`
	BorrowingLimit map[string]string `json:"borrowing_limit"`
	LendingLimit   map[string]string `json:"lending_limit"`
}

// allocationSpecAlias breaks the recursion MarshalJSON would otherwise
// cause by re-entering AllocationSpec's own MarshalJSON.
type allocationSpecAlias AllocationSpec

// MarshalJSON substitutes an empty map for a nil Nominal, BorrowingLimit,
// or LendingLimit, mirroring Rust's HashMap::default(), which serde
// always writes as `{}`, never `null` (the frozen contract's
// AllocationSpec schema types all three as required objects).
func (a AllocationSpec) MarshalJSON() ([]byte, error) {
	alias := allocationSpecAlias(a)
	if alias.Nominal == nil {
		alias.Nominal = map[string]string{}
	}
	if alias.BorrowingLimit == nil {
		alias.BorrowingLimit = map[string]string{}
	}
	if alias.LendingLimit == nil {
		alias.LendingLimit = map[string]string{}
	}
	return json.Marshal(alias)
}

// AllocationSpecError reports which named field of an AllocationSpec is
// not a valid Kubernetes name.
type AllocationSpecError struct {
	Field string
	Name  string
}

func (e AllocationSpecError) Error() string {
	return fmt.Sprintf("%s name %q is not a valid Kubernetes name (RFC 1123 subdomain)", e.Field, e.Name)
}

// Validate checks an AllocationSpec's shape.
func (a *AllocationSpec) Validate() error {
	for _, nv := range []struct{ field, name string }{
		{"pool", a.Pool},
		{"project", a.Project},
		{"namespace", a.Namespace},
	} {
		if !IsK8sName(nv.name) {
			return AllocationSpecError{Field: nv.field, Name: nv.name}
		}
	}
	return nil
}
