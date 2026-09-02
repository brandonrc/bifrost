package req

import (
	"fmt"
	"strings"
	"testing"
)

func TestRequirementsHasEighteenRows(t *testing.T) {
	rs := Requirements()
	if len(rs) != 18 {
		t.Fatalf("got %d requirements, want 18", len(rs))
	}
	for i, r := range rs {
		if r.N != i+1 {
			t.Errorf("row %d has n=%d; rows must be 1..18 in order", i, r.N)
		}
		if r.Title == "" || r.Priority == "" {
			t.Errorf("row %d missing title or priority", r.N)
		}
	}
}

func TestCoversPanicsOutOfRange(t *testing.T) {
	for _, n := range []int{0, 19, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Covers(%d) did not panic", n)
				}
			}()
			Covers(t, n, "x")
		}()
	}
}

func TestReqLineRoundTrip(t *testing.T) {
	cases := []Line{
		{Kind: "covers", Req: 6, Reason: "creates a cluster"},
		{Kind: "notyetbuilt", Req: 5, Reason: "ephemeral RayJob", Outcome: "failed"},
		{Kind: "skip", Req: 0, Reason: "needs k8s"},
	}
	for _, c := range cases {
		got, ok := ParseLine(c.Format())
		if !ok || got != c {
			t.Errorf("round trip %+v -> %+v ok=%v", c, got, ok)
		}
	}
	if _, ok := ParseLine("some unrelated log line"); ok {
		t.Error("ParseLine accepted a non-REQ line")
	}
}

// NotYetBuilt must invert: a failing body is the expected state and passes;
// a passing body means the requirement appears built and must FAIL.
func TestNotYetBuiltInverts(t *testing.T) {
	// A failing body is the expected, happy-path state: NotYetBuilt must not
	// fail the caller. Since nothing here fails, a real subtest is fine --
	// there is nothing to cascade.
	passed := t.Run("failing-body-is-ok", func(t *testing.T) {
		NotYetBuilt(t, 5, "not built yet", func(t *testing.T) {
			t.Error("the feature is missing")
		})
	})
	if !passed {
		t.Error("NotYetBuilt with a failing body should PASS the outer test")
	}

	// A passing body means the requirement appears built: NotYetBuilt must
	// fail its caller. testing.Fail unconditionally propagates up a real
	// subtest's parent chain the instant it is called, regardless of what a
	// caller later does with t.Run's returned bool -- so observing that
	// failure through a genuine subtest would also fail this test (and the
	// whole package). Give NotYetBuilt an unparented *testing.T instead and
	// inspect it directly; that is exactly the isolation NotYetBuilt itself
	// uses to run body without letting body's own failure escape upward.
	caller := &testing.T{}
	NotYetBuilt(caller, 5, "not built yet", func(t *testing.T) {})
	if !caller.Failed() {
		t.Error("NotYetBuilt with a passing body should fail its caller")
	}
}

func TestNotYetBuiltPassingBodyMessage(t *testing.T) {
	var got string
	rec := &recorder{T: t, onError: func(s string) { got = s }}
	notYetBuiltImpl(rec, 5, "r", func() bool { return true })
	if !strings.Contains(got, "requirement 5 appears built") || !strings.Contains(got, "remove the NotYetBuilt marker") {
		t.Errorf("message = %q", got)
	}
}

// recorder captures Errorf output for message assertions.
type recorder struct {
	*testing.T
	onError func(string)
}

func (r *recorder) Errorf(format string, args ...any) { r.onError(fmt.Sprintf(format, args...)) }
func (r *recorder) Logf(string, ...any)               {}
