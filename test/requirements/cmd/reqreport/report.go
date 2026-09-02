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
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

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
	Status      string   `json:"status"` // built|partial|not-yet-built|failing|untested
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
// a Report: one row per requirement in req.Requirements() order.
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
		r, ok := results[e.Test]
		if !ok {
			r = &testResult{Name: e.Test}
			results[e.Test] = r
			*order = append(*order, e.Test)
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

// status is DERIVED from counts, never typed by hand (spec §2.4).
func status(r Row) string {
	switch {
	case r.Tests == 0:
		return "untested"
	case r.Failed > 0:
		return "failing"
	case r.NotYetBuilt == r.Tests:
		return "not-yet-built"
	case r.NotYetBuilt > 0:
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
