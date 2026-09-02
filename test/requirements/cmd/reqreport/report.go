package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// event is one line of `go test -json` output. Only the fields reqreport
// needs are decoded; everything else in the stream is ignored.
type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// selfTestPackageSuffix is the package whose tests must never feed the
// matrix. test/requirements/req is the requirement-test framework's own
// implementation; its unit tests (req_test.go) exercise Covers/NotYetBuilt
// end to end using example requirement numbers (e.g. "5") to prove the
// mechanism works, not to claim coverage of that requirement. Every number
// 1..18 names a real row, so there is no "safe" example number to pick
// instead, and guards_test.go's TestEveryRequirementTestDeclaresCoverage
// exempts this same directory from the Covers/NotYetBuilt rule for the
// identical reason: it is infrastructure, not a requirement test. Events
// from this package are dropped in readEvents before a testResult is ever
// created for them, so their REQ lines never reach a Row.
const selfTestPackageSuffix = "/test/requirements/req"

// testResult is one top-level or sub test with the REQ lines it logged and
// its final pass/fail/skip outcome.
//
// Counting model (Task 8 decision 3): `go test -json` emits subtests as
// "TestParent/sub_name". A REQ line logged by the PARENT (e.g. req.Covers
// called before a t.Run loop) arrives on an "output" event whose Test field
// is still the parent's name, so it is attributed to the parent's own
// testResult -- never to a subtest. Each requirement-covering REQ line
// counts as exactly one test, and its pass/fail/skip is the outcome of the
// test that logged it (the parent's own outcome event), not any subtest's.
// This is consistent with go test itself: when any subtest fails, go test
// also emits a "fail" action for the parent, so a parent-logged REQ line
// correctly comes out failing.
type testResult struct {
	Name    string
	File    string // the -in file this test was first seen in; see readEvents
	Outcome string // pass|fail|skip
	Lines   []req.Line
}

// Row is one requirement's aggregate for one lane.
type Row struct {
	N           int      `json:"n"`
	Title       string   `json:"title"`
	Priority    string   `json:"priority"`
	Tests       int      `json:"tests"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	Skipped     int      `json:"skipped"`
	NotYetBuilt int      `json:"not_yet_built"`
	Status      string   `json:"status"` // built|partial|skipped|not-yet-built|failing|untested
	TestNames   []string `json:"test_names"`
	SkipReasons []string `json:"skip_reasons"`
}

// Report is the full traceability matrix for one lane.
type Report struct {
	Lane  string `json:"lane"`
	Rows  []Row  `json:"rows"`
	drift []string
}

// Build parses one or more `go test -json` streams and aggregates them into
// a Report: one row per requirement in req.Requirements() order. It returns
// an error if the same test name appears in two different files (see
// readEvents).
func Build(files []string, lane string) (*Report, error) {
	results := map[string]*testResult{}
	var order []string
	for _, f := range files {
		if err := readEvents(f, results, &order); err != nil {
			return nil, err
		}
	}

	rep := &Report{Lane: lane}
	for _, rq := range req.Requirements() {
		rep.Rows = append(rep.Rows, Row{N: rq.N, Title: rq.Title, Priority: rq.Priority})
	}
	for _, name := range order {
		rep.absorb(results[name], name)
	}
	for i := range rep.Rows {
		rep.Rows[i].Status = status(rep.Rows[i])
		sort.Strings(rep.Rows[i].TestNames)
	}
	return rep, nil
}

// readEvents scans one `go test -json` file (one JSON event per line),
// parsing REQ lines out of "output" events and recording each test's final
// outcome. results and order are shared accumulators across files so
// -in a,b aggregates as one run.
//
// A test name recurring within the SAME file (run, then one or more output
// events, then an outcome event) is normal -- that's how go test reports
// one test -- and folds into the same testResult. The same name recurring
// across DIFFERENT files is almost certainly two unrelated inputs
// (different lanes, a stale rerun, a copy/paste) that happen to share a
// name; silently merging their REQ lines would double-count Tests, and
// whichever file's outcome event is read last would silently clobber the
// other's, with no error and no sign of the lost coverage. So it's a hard
// error instead.
func readEvents(path string, results map[string]*testResult, order *[]string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Test == "" {
			continue // package-level events (no Test field) carry no REQ lines
		}
		if strings.HasSuffix(e.Package, selfTestPackageSuffix) {
			continue // req's own self-tests; see selfTestPackageSuffix
		}
		r, ok := results[e.Test]
		if !ok {
			r = &testResult{Name: e.Test, File: path}
			results[e.Test] = r
			*order = append(*order, e.Test)
		} else if r.File != path {
			return fmt.Errorf("reqreport: test %q appears in both %s and %s", e.Test, r.File, path)
		}
		switch e.Action {
		case "output":
			if l, ok := req.ParseLine(strings.TrimSpace(e.Output)); ok {
				r.Lines = append(r.Lines, l)
			}
		case "pass", "fail", "skip":
			r.Outcome = e.Action
		}
	}
	return sc.Err()
}

// absorb folds one test's REQ lines into the report's rows.
func (r *Report) absorb(t *testResult, name string) {
	if t == nil {
		return
	}
	for _, l := range t.Lines {
		switch l.Kind {
		case "covers":
			row := &r.Rows[l.Req-1]
			row.Tests++
			row.TestNames = append(row.TestNames, name)
			switch t.Outcome {
			case "pass":
				row.Passed++
			case "fail":
				row.Failed++
			case "skip":
				row.Skipped++
			}
		case "notyetbuilt":
			row := &r.Rows[l.Req-1]
			row.Tests++
			row.TestNames = append(row.TestNames, name)
			row.NotYetBuilt++
			if l.Outcome == "passed" {
				r.drift = append(r.drift, fmt.Sprintf(
					"requirement %d appears built: %s passed under a NotYetBuilt marker (%s)",
					l.Req, name, l.Reason))
			}
		}
	}
	// A skip line (req.NeedsCapability / req.NeedK8s) attaches its reason
	// to every requirement the same test also declared covers=. req=0 skip
	// lines with no accompanying covers= are target-level noise (no
	// requirement to attach to) and are dropped.
	for _, l := range t.Lines {
		if l.Kind != "skip" {
			continue
		}
		for _, c := range t.Lines {
			if c.Kind == "covers" {
				row := &r.Rows[c.Req-1]
				row.SkipReasons = append(row.SkipReasons, l.Reason)
			}
		}
	}
}

// status is DERIVED from counts, never typed by hand (spec §2.4: "Built ⇔
// every covering test passed ... Partial ⇔ some tests pass and at least one
// NotYetBuilt remains. Not yet built ⇔ all tests carry the marker.
// Untested ⇔ zero tests"). Checked in this order:
//
//   - untested:      Tests == 0
//   - failing:       Failed > 0
//   - not-yet-built: NotYetBuilt == Tests (every test carries the marker)
//   - skipped:       Passed == 0 && Skipped > 0 && NotYetBuilt == 0 --
//     tests exist and none failed, but none of them ran (passed) on this
//     lane either. A populated row, not an untested-problem.
//   - partial:       NotYetBuilt > 0 || Skipped > 0 -- something is
//     outstanding: a marker remains, or a covering test didn't run.
//   - built:         Passed == Tests -- every covering test passed. A
//     skipped test did NOT pass, so a skip-only row is never built.
func status(r Row) string {
	switch {
	case r.Tests == 0:
		return "untested"
	case r.Failed > 0:
		return "failing"
	case r.NotYetBuilt == r.Tests:
		return "not-yet-built"
	case r.Passed == 0 && r.Skipped > 0 && r.NotYetBuilt == 0:
		return "skipped"
	case r.NotYetBuilt > 0 || r.Skipped > 0:
		return "partial"
	default:
		return "built"
	}
}

// Untested returns the requirement numbers with zero tests.
func (r *Report) Untested() []int {
	var out []int
	for _, row := range r.Rows {
		if row.Tests == 0 {
			out = append(out, row.N)
		}
	}
	return out
}

// Problems returns the conditions that must fail the run: any
// notyetbuilt-marker-on-a-passing-test drift (always fatal), plus any
// zero-test requirement unless allowUntested.
func (r *Report) Problems(allowUntested bool) []string {
	out := append([]string{}, r.drift...)
	if !allowUntested {
		if u := r.Untested(); len(u) > 0 {
			out = append(out, fmt.Sprintf("requirements with zero tests: %v (every row needs at least one test, or pass -allow-untested)", u))
		}
	}
	return out
}

// Markdown renders the traceability matrix.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Requirement traceability (%s)\n\n", strings.ToUpper(r.Lane))
	b.WriteString("Generated by `test/requirements/cmd/reqreport`. Do not edit; CI regenerates and diffs.\n\n")
	b.WriteString("| # | Requirement | Priority | Tests | Pass | Fail | Skip | Not yet built | Status |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "| %d | %s | %s | %d | %d | %d | %d | %d | **%s** |\n",
			row.N, row.Title, row.Priority, row.Tests, row.Passed, row.Failed, row.Skipped, row.NotYetBuilt, row.Status)
	}
	b.WriteString("\n## Per-requirement detail\n")
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "\n### %d. %s\n", row.N, row.Title)
		if len(row.TestNames) == 0 {
			b.WriteString("- _no tests_\n")
		}
		for _, n := range row.TestNames {
			fmt.Fprintf(&b, "- `%s`\n", n)
		}
		for _, s := range row.SkipReasons {
			fmt.Fprintf(&b, "- skipped: %s\n", s)
		}
	}
	return b.String()
}
