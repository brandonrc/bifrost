// Package req is the requirement-test framework's vocabulary: which
// requirement a test proves (Covers), which it is waiting on (NotYetBuilt),
// what a target must have for it to run (NeedsCapability), and the Target
// seam every test speaks through. It imports nothing from internal/.
package req

import (
	_ "embed"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed requirements.yaml
var requirementsYAML []byte

// Requirement is one row of the 18-row table.
type Requirement struct {
	N        int    `yaml:"n"`
	Priority string `yaml:"priority"`
	Title    string `yaml:"title"`
}

var (
	reqsOnce sync.Once
	reqs     []Requirement
)

// Requirements returns the 18 rows, in order.
func Requirements() []Requirement {
	reqsOnce.Do(func() {
		if err := yaml.Unmarshal(requirementsYAML, &reqs); err != nil {
			panic("req: requirements.yaml: " + err.Error())
		}
	})
	return reqs
}

func mustValid(n int) {
	if n < 1 || n > len(Requirements()) {
		panic(fmt.Sprintf("req: requirement %d is out of range 1..%d", n, len(Requirements())))
	}
}

// Covers declares that the calling test proves requirement n. Call it first.
// A test may Covers two requirements when one scenario genuinely proves
// both; give each its own reason.
func Covers(t testing.TB, n int, reason string) {
	t.Helper()
	mustValid(n)
	t.Log(Line{Kind: "covers", Req: n, Reason: reason}.Format())
}

// NotYetBuilt runs body in isolation and INVERTS its result: a failing body
// is the expected state for an unbuilt requirement and the outer test
// passes; a passing body means the requirement appears built, and the outer
// test fails until a human removes the marker in the same PR that built it.
//
// body runs against a *testing.T of its own, detached from t's test tree:
// go's testing.Fail unconditionally propagates up through a subtest's
// parent chain, so running body via t.Run would fail t (and every ancestor
// up to the top-level test) the moment body's assertions fail -- which is
// the expected, common case here. Detaching body's T avoids that cascade
// while still letting body use the normal *testing.T assertion methods.
func NotYetBuilt(t *testing.T, n int, reason string, body func(t *testing.T)) {
	t.Helper()
	mustValid(n)
	bodyPassed := runDetached(body)
	notYetBuiltImpl(t, n, reason, func() bool { return bodyPassed })
}

// runDetached runs body against a fresh, unparented *testing.T and reports
// whether it completed without failing. body runs on its own goroutine so
// that t.Fatal (which calls runtime.Goexit) unwinds only that goroutine.
// A panic in body is treated the same as a failed assertion.
func runDetached(body func(t *testing.T)) bool {
	st := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		body(st)
	}()
	<-done
	return !st.Failed()
}

type errorLogger interface {
	Errorf(string, ...any)
	Logf(string, ...any)
}

func notYetBuiltImpl(t errorLogger, n int, reason string, bodyPassed func() bool) {
	if bodyPassed() {
		t.Logf("%s", Line{Kind: "notyetbuilt", Req: n, Reason: reason, Outcome: "passed"}.Format())
		t.Errorf("requirement %d appears built: the NotYetBuilt body passed. remove the NotYetBuilt marker in the same PR that made this pass (reason was: %s)", n, reason)
		return
	}
	t.Logf("%s", Line{Kind: "notyetbuilt", Req: n, Reason: reason, Outcome: "failed"}.Format())
}

// NeedsCapability skips unless the target declares cap. The skip is
// recorded as a REQ line so the report can say WHY a column is partial.
func NeedsCapability(t testing.TB, tgt Target, cap string) {
	t.Helper()
	if !tgt.Has(cap) {
		reason := "target " + tgt.Name() + " lacks capability " + cap
		t.Log(Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
}

// NeedK8s skips on targets without Kubernetes (inproc).
func NeedK8s(t testing.TB, tgt Target) {
	t.Helper()
	if _, ok := tgt.K8s(); !ok {
		reason := "target " + tgt.Name() + " has no Kubernetes"
		t.Log(Line{Kind: "skip", Req: 0, Reason: reason}.Format())
		t.Skip(reason)
	}
}

// EventuallyTimeout is the per-lane convergence budget (spec §8).
func EventuallyTimeout(tgt Target) time.Duration {
	if _, ok := tgt.K8s(); ok {
		return 5 * time.Minute
	}
	return 5 * time.Second
}

// Eventually polls cond until it reports true or the lane timeout elapses.
// cond returns a human-readable state for the failure message. This is the
// only sanctioned way to wait; time.Sleep is forbidden under test/requirements.
func Eventually(t testing.TB, tgt Target, cond func() (ok bool, state string)) {
	t.Helper()
	deadline := time.Now().Add(EventuallyTimeout(tgt))
	var last string
	for time.Now().Before(deadline) {
		ok, state := cond()
		if ok {
			return
		}
		last = state
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s; last state: %s", EventuallyTimeout(tgt), last)
}

var (
	runIDOnce sync.Once
	runID     string
)

// RunID is a short per-process id used as the cluster-name prefix so every
// object a run creates can be found and removed (spec §1.4).
func RunID() string {
	runIDOnce.Do(func() {
		if v := os.Getenv("REQ_RUN_ID"); v != "" {
			runID = v
			return
		}
		runID = fmt.Sprintf("t%x", time.Now().UnixNano()%0xffffff)
	})
	return runID
}

// Name returns a cluster id carrying the run prefix: "t<run>-<short>".
func Name(short string) string { return RunID() + "-" + short }
