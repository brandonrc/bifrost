package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// A stream in the shape the requirement binaries actually produce. The first
// three lines are copied from a real grace run (r13_pools), including the
// `=== NAME` line with an empty name that ends a test's attribution; the rest
// adds what that package happened not to exercise: a subtest, a parallel
// interleave, a failure with its assertion line, and the package's own trailer.
const sample = "\x16=== RUN   TestPoolAndAllocationLifecycle\n    pools_test.go:22: REQ: kind=covers req=13 reason=\"an administrator creates a pool\"\n\x16--- PASS: TestPoolAndAllocationLifecycle (21.40s)\n\x16=== NAME\n\x16=== RUN   TestPermissionMatrix\n\x16=== PAUSE TestPermissionMatrix\n\x16=== RUN   TestSuspendResume\n\x16=== CONT  TestPermissionMatrix\n    matrix_test.go:35: REQ: kind=covers req=3 reason=\"every operation\"\n\x16=== NAME  TestSuspendResume\n    lifecycle_test.go:88: suspend as project operator\n\x16--- PASS: TestSuspendResume (3.01s)\n\x16=== NAME  TestPermissionMatrix\n    matrix_test.go:140: create_pool as dev-a = 200, permissions.yaml says deny (403)\n\x16--- FAIL: TestPermissionMatrix (12.02s)\n\x16=== RUN   TestIdleClusterIsReaped\n    hygiene_test.go:20: REQ: kind=covers req=6 reason=\"an idle cluster is reaped\"\n\x16--- SKIP: TestIdleClusterIsReaped (0.00s)\n\x16=== RUN   TestWithSubtests\n\x16=== RUN   TestWithSubtests/first_case\n    sub_test.go:9: checking\n\x16=== NAME  TestWithSubtests\n\x16--- FAIL: TestWithSubtests (0.11s)\n\x16    --- FAIL: TestWithSubtests/first_case (0.10s)\n\x16FAIL\n\n"

const samplePkg = "github.com/brandonrc/bifrost/test/requirements/r03_rbac"

// results is the ordered list of outcomes a report reads: the action and the
// test it belongs to, ignoring output events.
func results(t *testing.T, stream []byte) []string {
	t.Helper()
	var out []string
	dec := json.NewDecoder(bytes.NewReader(stream))
	for {
		var ev struct {
			Action, Test, Package string
		}
		if err := dec.Decode(&ev); err != nil {
			break
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			if ev.Package != samplePkg {
				t.Errorf("event for package %q, want %q", ev.Package, samplePkg)
			}
			out = append(out, ev.Action+" "+ev.Test)
		}
	}
	return out
}

func convertSample(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := convert(samplePkg, strings.NewReader(sample), &buf); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return buf.Bytes()
}

func TestConvertReportsEveryOutcomeWithItsTest(t *testing.T) {
	got := results(t, convertSample(t))
	want := []string{
		"pass TestPoolAndAllocationLifecycle",
		"pass TestSuspendResume",
		"fail TestPermissionMatrix",
		"skip TestIdleClusterIsReaped",
		"fail TestWithSubtests",
		"fail TestWithSubtests/first_case",
		"fail ",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("outcomes:\n got %v\nwant %v", got, want)
	}
}

// Output has to reach the right test, because that is how a coverage marker
// (`REQ: kind=covers req=N`) is attributed to the test that declared it. Get
// this wrong and the traceability matrix credits the wrong requirement.
func TestConvertAttributesOutputToTheTestThatWroteIt(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(convertSample(t)))
	markers := map[string]string{}
	for {
		var ev struct {
			Action, Test, Output string
		}
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev.Action == "output" && strings.Contains(ev.Output, "REQ: kind=covers") {
			markers[ev.Test] = strings.TrimSpace(ev.Output)
		}
	}
	for _, test := range []string{"TestPoolAndAllocationLifecycle", "TestPermissionMatrix", "TestIdleClusterIsReaped"} {
		if _, ok := markers[test]; !ok {
			t.Errorf("%s's coverage marker was attributed elsewhere: %v", test, markers)
		}
	}
	// The interleaved parallel test wrote its marker after a CONT line, which
	// is the case a naive "current test" tracker attributes to whoever ran last.
	if got := markers["TestPermissionMatrix"]; !strings.Contains(got, "req=3") {
		t.Errorf("TestPermissionMatrix marker = %q", got)
	}
}

// The converter stands in for `go tool test2json`, so where that tool is
// available the two must agree about outcomes. This is the check that keeps a
// reimplementation honest as Go's own framing evolves.
//
// Compared as a set, not a sequence: Go holds a parent test's result back until
// its subtests' results are known, so it emits the subtest first where this
// emits in stream order. Nothing downstream reads the order — the runner
// counts outcomes and the report aggregates them per test — so matching Go's
// buffering would be work in service of nothing.
func TestConvertAgreesWithGoToolTest2json(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain; the differential check needs `go tool test2json`")
	}
	cmd := exec.Command(goBin, "tool", "test2json", "-t", "-p", samplePkg)
	cmd.Stdin = strings.NewReader(sample)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Skipf("go tool test2json: %v", err)
	}
	want := results(t, out.Bytes())
	got := results(t, convertSample(t))
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("disagreement with go tool test2json:\n ours %v\ntheirs %v", got, want)
	}
}

// The hand-written sample above is one kind of evidence; a stream a real
// requirement binary produced is another, and it is the one that caught the
// framing byte. This file is the r01 package's output from an in-cluster run,
// captured verbatim.
//
// Named .txt rather than .out because the repository ignores *.out as lane
// artifacts — which is how the first version of this fixture was left
// untracked, passing here and failing in CI on a file that did not exist.
func TestConvertHandlesARealStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/real-stream.txt")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !bytes.Contains(raw, []byte{0x16}) {
		t.Fatal("the fixture lost its framing bytes; re-capture it with cat, not a terminal")
	}
	var buf bytes.Buffer
	if err := convert(samplePkg, bytes.NewReader(raw), &buf); err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := results(t, buf.Bytes())
	// Three tests passed and one skipped — the serve-endpoint test, which
	// needs a capability grace does not declare — plus the package trailer.
	want := []string{
		"pass TestDeployServiceConvergesToServing",
		"pass TestRedeploySameNameBumpsGeneration",
		"pass TestDeleteServiceConvergesToTerminated",
		"skip TestServeEndpointAnswersThroughTheGateway",
		"pass ",
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("real stream:\n got %v\nwant %v", got, want)
	}
	// And no Output field carries the framing byte onwards.
	if bytes.Contains(buf.Bytes(), []byte("\\u0016")) {
		t.Error("a framing byte reached an Output field")
	}
}
