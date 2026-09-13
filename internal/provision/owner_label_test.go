package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// dns1123LabelValue is the RFC 1123 label shape the API server enforces
// on label values.
var dns1123LabelValue = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func assertValidLabelValue(t *testing.T, owner, value string) {
	t.Helper()
	if len(value) > 63 || !dns1123LabelValue.MatchString(value) {
		t.Fatalf("ownerLabelValue(%q) = %q, not a valid RFC 1123 label value", owner, value)
	}
}

func TestOwnerLabelValuePassesValidOwnersThroughUnchanged(t *testing.T) {
	// The per-owner NetworkPolicy and the hub's notebook label predate
	// F9: owners that are already valid label values must not change, or
	// every existing selector stops matching.
	for _, owner := range []string{"alice", "bob", "user-123", "a1", strings.Repeat("x", 63)} {
		if got := ownerLabelValue(owner); got != owner {
			t.Fatalf("ownerLabelValue(%q) = %q, want unchanged", owner, got)
		}
	}
}

func TestOwnerLabelValueRewritesEmailShapedOwners(t *testing.T) {
	owner := "jane.doe@example.com"
	got := ownerLabelValue(owner)
	assertValidLabelValue(t, owner, got)
	if got == owner {
		t.Fatalf("email owner must be rewritten, got %q", got)
	}
	if !strings.HasPrefix(got, "jane-doe-example-com-") {
		t.Fatalf("rewritten value should keep a readable stem, got %q", got)
	}
}

func TestOwnerLabelValueNeverCollidesOnErasedChars(t *testing.T) {
	// Two owners differing only in characters the mapping erases would
	// collide without the hash suffix — and would then share one
	// NetworkPolicy pin.
	pairs := [][2]string{
		{"jane@x.com", "jane#x.com"},
		{"Alice", "alice"},
		{"UPPER", "upper"},
	}
	for _, p := range pairs {
		a, b := ownerLabelValue(p[0]), ownerLabelValue(p[1])
		if a == b {
			t.Fatalf("ownerLabelValue(%q) == ownerLabelValue(%q) == %q: distinct owners collided", p[0], p[1], a)
		}
	}
}

func TestOwnerLabelValueHandlesDegenerateAndOverlongOwners(t *testing.T) {
	for _, owner := range []string{
		"@@@",                    // erases to nothing
		"-leading-and-trailing-", // dashes are not valid at the edges
		"jáne ß",                 // non-ASCII folds to dashes
		strings.Repeat("a", 100), // valid chars but overlong
		strings.Repeat("a", 50) + "@" + strings.Repeat("b", 50),
	} {
		assertValidLabelValue(t, owner, ownerLabelValue(owner))
	}
	// Overlong owners sharing a 63-char prefix still differ via the hash.
	a := ownerLabelValue(strings.Repeat("a", 60) + "1")
	b := ownerLabelValue(strings.Repeat("a", 60) + "2")
	if a == b {
		t.Fatalf("overlong owners collided: %q", a)
	}
}

func TestOwnerLabelValueSuffixIs64Bits(t *testing.T) {
	// Collision resistance of the suffix is a SECURITY property, not a
	// convenience: the label selects the per-owner NetworkPolicy pin, and
	// an attacker with an IdP-controlled preferred_username could mint an
	// owner whose rewritten label collides with a victim's. The red team
	// measured ~5.5 Mhash/s single-core, so the old 8-hex (32-bit) suffix
	// fell to a full preimage grind in minutes; 16 hex chars (64 bits of
	// SHA-256 over the raw owner) puts it out of brute-force reach. The
	// assumption is SHA-256 preimage resistance only — nothing about the
	// attacker's chosen usernames.
	hexSuffix := regexp.MustCompile(`-[0-9a-f]{16}$`)
	owner := "jane.doe@example.com"
	got := ownerLabelValue(owner)
	if !hexSuffix.MatchString(got) {
		t.Fatalf("rewritten owner %q lacks the -<16 hex> suffix", got)
	}
	// The suffix must commit to the RAW owner: same hash as a direct
	// SHA-256 of the untransformed string.
	sum := sha256.Sum256([]byte(owner))
	if want := hex.EncodeToString(sum[:])[:16]; !strings.HasSuffix(got, "-"+want) {
		t.Fatalf("suffix of %q = want %q", got, want)
	}
}

func TestOwnerLabelValueStaysWithin63CharsForPathologicalOwners(t *testing.T) {
	// Stem + "-" + 16 hex must never exceed the RFC 1123 63-char label
	// budget: stem is capped at 46 and dash-trimmed, so the worst case is
	// exactly 63.
	for _, owner := range []string{
		strings.Repeat("a", 46) + "-" + strings.Repeat("b", 100), // stem trimmed at a dash boundary
		strings.Repeat("a", 47),                                  // one char over the stem cap
		strings.Repeat("ab", 500),                                // far overlong
		strings.Repeat("-", 200),                                 // erases to nothing -> "owner" stem
		"@" + strings.Repeat("x", 100) + "@",                     // dash edges after mapping
		strings.Repeat("a", 45) + "-" + strings.Repeat("-", 60),  // trim cascades
		"jáne.ß@example.com" + strings.Repeat("9", 100),          // non-ASCII + overlong
	} {
		got := ownerLabelValue(owner)
		assertValidLabelValue(t, owner, got)
		if len(got) > 63 {
			t.Fatalf("ownerLabelValue(%q) = %q (%d chars), exceeds 63", owner, got, len(got))
		}
		// Deterministic: the label and the NetworkPolicy selector are
		// produced by separate call sites and must agree.
		if again := ownerLabelValue(owner); again != got {
			t.Fatalf("ownerLabelValue(%q) nondeterministic: %q vs %q", owner, got, again)
		}
	}
}

// F9 end to end: the label stamped on the RayCluster and its pods and the
// NetworkPolicy selector for the same owner must be one value, valid, and
// the raw owner must survive in the annotation.
func TestOwnerLabelSelectorConsistencyForEmailOwner(t *testing.T) {
	owner := "jane.doe@example.com"
	spec := testSpec(t, wg("cpu", 0, 1, 1))
	spec.Owner = &owner
	rc, err := RayClusterFor("sess-jane", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	label := rc.Labels[OwnerLabel]
	assertValidLabelValue(t, owner, label)
	if rc.Annotations[OwnerRawAnnotation] != owner {
		t.Fatalf("raw-owner annotation = %q, want %q", rc.Annotations[OwnerRawAnnotation], owner)
	}
	head := rc.Spec.HeadGroupSpec.Template.Labels[OwnerLabel]
	worker := rc.Spec.WorkerGroupSpecs[0].Template.Labels[OwnerLabel]
	if head != label || worker != label {
		t.Fatalf("pod labels %q/%q must match the CR label %q", head, worker, label)
	}

	p := ClusterAllowNetworkPolicy("sess-jane", &owner)
	sel := p.Spec.Ingress[1].From[0].PodSelector.MatchLabels[OwnerLabel]
	assertValidLabelValue(t, owner, sel)
	if sel != label {
		t.Fatalf("selector %q must equal the stamped label %q", sel, label)
	}
}

func TestRayJobOwnerLabelIsSanitizedConsistently(t *testing.T) {
	owner := "jane.doe@example.com"
	spec := testJobSpec(wg("cpu", 1, 2, 1))
	spec.Owner = &owner
	rj, err := RayJobFor("job-1", spec, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	label := rj.Labels[OwnerLabel]
	assertValidLabelValue(t, owner, label)
	if rj.Annotations[OwnerRawAnnotation] != owner {
		t.Fatalf("raw-owner annotation = %q, want %q", rj.Annotations[OwnerRawAnnotation], owner)
	}
	if got := rj.Spec.SubmitterPodTemplate.Labels[OwnerLabel]; got != label {
		t.Fatalf("submitter label %q must equal the job label %q", got, label)
	}
	if got := rj.Spec.RayClusterSpec.HeadGroupSpec.Template.Labels[OwnerLabel]; got != label {
		t.Fatalf("head pod label %q must equal the job label %q", got, label)
	}
}

func TestOwnerlessResourcesCarryNoOwnerLabelOrAnnotation(t *testing.T) {
	rc, err := RayClusterFor("sess-x", testSpec(t), false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if _, ok := rc.Labels[OwnerLabel]; ok {
		t.Fatal("ownerless cluster must carry no owner label")
	}
	if _, ok := rc.Annotations[OwnerRawAnnotation]; ok {
		t.Fatal("ownerless cluster must carry no raw-owner annotation")
	}
}
