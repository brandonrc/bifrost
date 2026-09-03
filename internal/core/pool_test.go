package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Ported from mobula-core/src/pool.rs #[cfg(test)] mod tests.

func testFlavor(name string) FlavorSpec {
	return FlavorSpec{
		Name: name,
		Resources: map[string]string{
			"cpu":    "64",
			"memory": "256Gi",
		},
		NodeLabels: map[string]string{},
		Taints:     []TaintSpec{},
	}
}

func testPool() PoolSpec {
	return PoolSpec{
		Name:              "gpu-pool",
		Flavors:           []FlavorSpec{testFlavor("a100")},
		Cohort:            "research",
		FairSharingWeight: 1.0,
		Elastic:           true,
		GpuSharing:        nil,
	}
}

func TestValidPoolPasses(t *testing.T) {
	p := testPool()
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPoolNameMustBeK8sSafe(t *testing.T) {
	for _, bad := range []string{"", "GPU_Pool", "-lead", "trail-", "has space", "under_score"} {
		p := testPool()
		p.Name = bad
		err, ok := p.Validate().(PoolSpecError)
		if !ok || err.Kind != PoolSpecErrInvalidName {
			t.Fatalf("name %q: expected InvalidName error, got %v", bad, p.Validate())
		}
	}
	// Dots and dashes inside are fine.
	p := testPool()
	p.Name = "gpu-pool.v2"
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCohortMustBeK8sSafe(t *testing.T) {
	p := testPool()
	p.Cohort = ""
	err, ok := p.Validate().(PoolSpecError)
	if !ok || err.Kind != PoolSpecErrInvalidCohort {
		t.Fatalf("expected InvalidCohort error, got %v", p.Validate())
	}
}

// E2/L2: exhaustive byte-membership test for IsK8sName. Brute-forces every
// possible byte value (0-255) both as a lone character (exercising the
// lead/trail alnum rule) and sandwiched between two 'a's (exercising
// interior accepted-set membership), cross-checked against the documented
// accepted set (`-.0-9a-z`, no leading/trailing `-`/`.`) rather than
// IsK8sName's own implementation, plus explicit named boundary cases.
func TestIsK8sNameExhaustiveByteMembership(t *testing.T) {
	accepted := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '.'
	}
	alnum := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
	}
	for i := 0; i < 256; i++ {
		b := byte(i)
		lone := string([]byte{b})
		if want, got := alnum(b), IsK8sName(lone); got != want {
			t.Fatalf("IsK8sName(%q) [lone byte %d] = %v, want %v", lone, b, got, want)
		}
		mid := "a" + lone + "a"
		if want, got := accepted(b), IsK8sName(mid); got != want {
			t.Fatalf("IsK8sName(%q) [interior byte %d] = %v, want %v", mid, b, got, want)
		}
	}
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"-a", false}, // leading '-'
		{"a-", false}, // trailing '-'
		{".a", false}, // leading '.'
		{"a.", false}, // trailing '.'
		{"a-a", true}, // interior '-'
		{"a.a", true}, // interior '.'
		{"0", true},   // single digit: alnum, valid lead and trail
		{"a", true},   // single letter
		{"-", false},  // single '-': accepted-set member, not alnum
		{".", false},  // single '.': accepted-set member, not alnum
	} {
		if got := IsK8sName(tc.s); got != tc.want {
			t.Fatalf("IsK8sName(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestPoolRequiresAFlavor(t *testing.T) {
	p := testPool()
	p.Flavors = nil
	want := PoolSpecError{Kind: PoolSpecErrNoFlavors}
	if got := p.Validate(); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDuplicateFlavorNamesRejected(t *testing.T) {
	p := testPool()
	p.Flavors = append(p.Flavors, testFlavor("a100"))
	want := PoolSpecError{Kind: PoolSpecErrDuplicateFlavor, Name: "a100"}
	if got := p.Validate(); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFlavorErrorsCarryFlavorContext(t *testing.T) {
	p := testPool()
	p.Flavors[0].Taints = append(p.Flavors[0].Taints, TaintSpec{
		Key:    "nvidia.com/gpu",
		Value:  "present",
		Effect: "",
	})
	want := PoolSpecError{
		Kind:   PoolSpecErrFlavor,
		Flavor: "a100",
		Source: FlavorSpecError{
			Kind:        FlavorSpecErrTaint,
			Key:         "nvidia.com/gpu",
			TaintSource: ErrTaintEmptyEffect,
		},
	}
	if got := p.Validate(); got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// C7: PoolSpecError.Unwrap() must expose the wrapped FlavorSpecError so
// errors.As reaches it, mirroring Rust's thiserror #[source] chain
// (mobula-core/src/pool.rs).
func TestPoolSpecErrorUnwrapReachesFlavorSpecError(t *testing.T) {
	p := testPool()
	p.Flavors[0].Name = "BAD_NAME"
	err := p.Validate()
	var fse FlavorSpecError
	if !errors.As(err, &fse) {
		t.Fatalf("errors.As(%v, &FlavorSpecError{}) = false, want true", err)
	}
	if fse.Kind != FlavorSpecErrInvalidName || fse.Name != "BAD_NAME" {
		t.Fatalf("unwrapped FlavorSpecError = %#v, want InvalidName(%q)", fse, "BAD_NAME")
	}
}

// C7: FlavorSpecError.Unwrap() must expose the wrapped TaintSpecError, and
// the chain must reach all the way from PoolSpecError through
// FlavorSpecError to TaintSpecError via errors.As.
func TestPoolSpecErrorUnwrapReachesTaintSpecError(t *testing.T) {
	p := testPool()
	p.Flavors[0].Taints = append(p.Flavors[0].Taints, TaintSpec{
		Key:    "nvidia.com/gpu",
		Value:  "present",
		Effect: "",
	})
	err := p.Validate()

	var fse FlavorSpecError
	if !errors.As(err, &fse) {
		t.Fatalf("errors.As(%v, &FlavorSpecError{}) = false, want true", err)
	}
	if fse.Kind != FlavorSpecErrTaint {
		t.Fatalf("unwrapped FlavorSpecError.Kind = %v, want FlavorSpecErrTaint", fse.Kind)
	}

	var tse TaintSpecError
	if !errors.As(err, &tse) {
		t.Fatalf("errors.As(%v, &TaintSpecError{}) = false, want true (full PoolSpecError -> FlavorSpecError -> TaintSpecError chain)", err)
	}
	if tse != ErrTaintEmptyEffect {
		t.Fatalf("unwrapped TaintSpecError = %v, want %v", tse, ErrTaintEmptyEffect)
	}
}

// C7: PoolSpecError variants other than Flavor carry no source and must
// not unwrap to a bogus zero-value FlavorSpecError.
func TestPoolSpecErrorUnwrapNilForNonFlavorVariant(t *testing.T) {
	err := PoolSpecError{Kind: PoolSpecErrNoFlavors}
	if unwrapped := err.Unwrap(); unwrapped != nil {
		t.Fatalf("Unwrap() = %v, want nil", unwrapped)
	}
}

func TestArbitraryResourceKeysAllowed(t *testing.T) {
	f := testFlavor("mixed")
	f.Resources["nvidia.com/mig-1g.10gb"] = "7"
	f.Resources["example.com/license"] = "2"
	if err := f.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ...but the empty key is never a resource name.
	f.Resources[""] = "1"
	want := FlavorSpecError{Kind: FlavorSpecErrEmptyResourceKey}
	if got := f.Validate(); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTaintFieldsMustBeNonEmpty(t *testing.T) {
	if got := (TaintSpec{Key: "", Value: "v", Effect: "NoSchedule"}).Validate(); got != ErrTaintEmptyKey {
		t.Fatalf("got %v, want %v", got, ErrTaintEmptyKey)
	}
	if got := (TaintSpec{Key: "k", Value: "", Effect: "NoSchedule"}).Validate(); got != ErrTaintEmptyValue {
		t.Fatalf("got %v, want %v", got, ErrTaintEmptyValue)
	}
}

func TestAllocationNamesMustBeK8sSafe(t *testing.T) {
	alloc := AllocationSpec{
		Pool:           "gpu-pool",
		Project:        "proj-a",
		Namespace:      "proj-a",
		Nominal:        map[string]string{},
		BorrowingLimit: map[string]string{},
		LendingLimit:   map[string]string{},
	}
	if err := alloc.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bad := alloc
	bad.Namespace = "Not_A_Namespace"
	want := AllocationSpecError{Field: "namespace", Name: "Not_A_Namespace"}
	if got := bad.Validate(); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSerdeRoundTripSnakeCase(t *testing.T) {
	p := testPool()
	mig := GpuSharingMig
	p.GpuSharing = &mig

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := v["fair_sharing_weight"]; !ok {
		t.Fatal("expected fair_sharing_weight in JSON")
	}
	if v["gpu_sharing"] != "mig" {
		t.Fatalf("gpu_sharing = %v, want \"mig\"", v["gpu_sharing"])
	}
	if _, ok := v["node_labels"]; ok {
		t.Fatal("node_labels must not appear at the top level (that's on flavors)")
	}

	var round PoolSpec
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !poolSpecEqual(round, p) {
		t.Fatalf("round trip mismatch: got %#v, want %#v", round, p)
	}
}

func TestGpuSharingDefaultsToPlatformDefaultWhenAbsent(t *testing.T) {
	// A spec without the field (incl. rows stored before #58) carries
	// nil — the platform default applies at enforcement time, and the
	// field is omitted from serialization rather than stored as null.
	p := testPool()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := v["gpu_sharing"]; ok {
		t.Fatal("gpu_sharing must be omitted when nil")
	}
	var round PoolSpec
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !poolSpecEqual(round, p) {
		t.Fatalf("round trip mismatch: got %#v, want %#v", round, p)
	}
}

func TestGpuSharingValuesAreKebabCase(t *testing.T) {
	cases := []struct {
		json string
		mode GpuSharing
	}{
		{`"whole-gpu"`, GpuSharingWholeGpu},
		{`"mig"`, GpuSharingMig},
		{`"time-slice"`, GpuSharingTimeSlice},
	}
	for _, c := range cases {
		var g GpuSharing
		if err := json.Unmarshal([]byte(c.json), &g); err != nil {
			t.Fatalf("%s: unexpected error: %v", c.json, err)
		}
		if g != c.mode {
			t.Fatalf("%s: got %v, want %v", c.json, g, c.mode)
		}
	}
	// The safe default is whole-GPU.
	if DefaultGpuSharing != GpuSharingWholeGpu {
		t.Fatalf("DefaultGpuSharing = %v, want %v", DefaultGpuSharing, GpuSharingWholeGpu)
	}
	// Unknown modes are rejected at parse time, never silently coerced.
	var g GpuSharing
	if err := json.Unmarshal([]byte(`"timeslice"`), &g); err == nil {
		t.Fatal("expected error for \"timeslice\"")
	}
	if err := json.Unmarshal([]byte(`"shared"`), &g); err == nil {
		t.Fatal("expected error for \"shared\"")
	}
}

func TestFairSharingWeightMustBeFiniteAndNonNegative(t *testing.T) {
	for _, bad := range []float64{-1.0, nan(), inf()} {
		p := testPool()
		p.FairSharingWeight = bad
		want := PoolSpecError{Kind: PoolSpecErrInvalidFairSharingWeight}
		if got := p.Validate(); got != want {
			t.Fatalf("weight %v: got %v, want %v", bad, got, want)
		}
	}
	// Zero is a legitimate weight (Kueue's default).
	p := testPool()
	p.FairSharingWeight = 0.0
	if err := p.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFlavorNameMustBeK8sSafe(t *testing.T) {
	f := testFlavor("Bad_Name")
	want := FlavorSpecError{Kind: FlavorSpecErrInvalidName, Name: "Bad_Name"}
	if got := f.Validate(); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTaintEffectMustBeNonEmpty(t *testing.T) {
	got := (TaintSpec{Key: "k", Value: "v", Effect: ""}).Validate()
	if got != ErrTaintEmptyEffect {
		t.Fatalf("got %v, want %v", got, ErrTaintEmptyEffect)
	}
}

// Added (not ported from Rust): fix round 1 (review finding M3). A
// zero-value FlavorSpec (nil Resources/NodeLabels/Taints) must still
// marshal each as `{}`/`[]`, not the Go zero value `null`, matching
// Rust's Vec::default()/BTreeMap::default() serde behavior.
func TestFlavorSpecMarshalsNilCollectionsAsEmpty(t *testing.T) {
	var f FlavorSpec
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if resources, ok := v["resources"].(map[string]any); !ok || len(resources) != 0 {
		t.Fatalf("resources = %v, want {}", v["resources"])
	}
	if labels, ok := v["node_labels"].(map[string]any); !ok || len(labels) != 0 {
		t.Fatalf("node_labels = %v, want {}", v["node_labels"])
	}
	if taints, ok := v["taints"].([]any); !ok || len(taints) != 0 {
		t.Fatalf("taints = %v, want []", v["taints"])
	}
}

// Added (not ported from Rust): fix round 2 (review re-pass, same class
// as M3). A zero-value PoolSpec (nil Flavors) must still marshal as `[]`,
// not the Go zero value `null`, matching the frozen contract's `flavors`
// array type and Rust's Vec::default() serde behavior.
func TestPoolSpecMarshalsNilFlavorsAsEmpty(t *testing.T) {
	var p PoolSpec
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if flavors, ok := v["flavors"].([]any); !ok || len(flavors) != 0 {
		t.Fatalf("flavors = %v, want []", v["flavors"])
	}
}

// Added (not ported from Rust): fix round 2 (review re-pass, same class
// as M3). A zero-value AllocationSpec (nil Nominal/BorrowingLimit/
// LendingLimit) must still marshal each as `{}`, not the Go zero value
// `null`, matching the frozen contract's required object types and
// Rust's BTreeMap::default() serde behavior.
func TestAllocationSpecMarshalsNilMapsAsEmpty(t *testing.T) {
	var a AllocationSpec
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, field := range []string{"nominal", "borrowing_limit", "lending_limit"} {
		m, ok := v[field].(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("%s = %v, want {}", field, v[field])
		}
	}
}

// poolSpecEqual compares two PoolSpecs by value, since maps/slices/pointers
// prevent a plain ==.
func poolSpecEqual(a, b PoolSpec) bool {
	if a.Name != b.Name || a.Cohort != b.Cohort || a.FairSharingWeight != b.FairSharingWeight || a.Elastic != b.Elastic {
		return false
	}
	if (a.GpuSharing == nil) != (b.GpuSharing == nil) {
		return false
	}
	if a.GpuSharing != nil && *a.GpuSharing != *b.GpuSharing {
		return false
	}
	if len(a.Flavors) != len(b.Flavors) {
		return false
	}
	for i := range a.Flavors {
		if !flavorSpecEqual(a.Flavors[i], b.Flavors[i]) {
			return false
		}
	}
	return true
}

func flavorSpecEqual(a, b FlavorSpec) bool {
	if a.Name != b.Name || len(a.Resources) != len(b.Resources) || len(a.NodeLabels) != len(b.NodeLabels) || len(a.Taints) != len(b.Taints) {
		return false
	}
	for k, v := range a.Resources {
		if b.Resources[k] != v {
			return false
		}
	}
	// E5/M2/M3: NodeLabels and Taints length checks alone would pass two
	// flavors whose contents differ but whose sizes match — compare
	// contents too. Taints is a slice, so order matters (it isn't a set).
	for k, v := range a.NodeLabels {
		if b.NodeLabels[k] != v {
			return false
		}
	}
	for i := range a.Taints {
		if a.Taints[i] != b.Taints[i] {
			return false
		}
	}
	return true
}

func nan() float64 {
	var zero float64
	return zero / zero
}

func inf() float64 {
	var zero float64
	one := 1.0
	return one / zero
}

// #4: purpose is compute | serving; the zero value reads as compute and
// any other spelling is refused at Validate (JSON ingress already rejects
// it at UnmarshalJSON, this covers struct-literal construction).
func TestPoolPurposeMustBeKnown(t *testing.T) {
	p := testPool()
	if err := p.Validate(); err != nil {
		t.Fatalf("zero-value purpose must validate as compute: %v", err)
	}
	for _, ok := range []PoolPurpose{PoolPurposeCompute, PoolPurposeServing} {
		p.Purpose = ok
		if err := p.Validate(); err != nil {
			t.Fatalf("purpose %q: unexpected error: %v", ok, err)
		}
	}
	p.Purpose = PoolPurpose("inference")
	err, isPool := p.Validate().(PoolSpecError)
	if !isPool || err.Kind != PoolSpecErrInvalidPurpose || err.Name != "inference" {
		t.Fatalf("expected InvalidPurpose error, got %v", p.Validate())
	}
	if !strings.Contains(err.Error(), "inference") {
		t.Fatalf("error should name the offending purpose: %v", err)
	}
}
