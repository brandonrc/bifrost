package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A compiled test binary run with `-test.v=test2json` writes verbose output
// with attribution markers, not JSON. The grace lane pipes that through
// `go tool test2json`; this image cannot, because modern Go implements that
// converter inside the `go` command rather than as a copyable tool binary, and
// carrying a Go SDK to run a test suite would be a several-hundred-megabyte
// answer to an eighty-line problem.
//
// So the events the runner and reqreport read are produced here. The
// conversion is the part those two consume and nothing more: which test each
// line belongs to, and how each test ended. Timing, subtest nesting and the
// elapsed fields `go test -json` also carries are left out — reqreport reads
// Action, Package, Test and Output.
//
// The framing byte matters: each line the harness writes carries a leading
// 0x16, and a converter that ignores it sees no markers at all — every line
// looks like package output, every count reads zero, and the run still exits
// 0 because the binaries did pass. That was the first in-cluster run.
//
// The markers, from cmd/internal/test2json:
//
//	=== RUN   TestX      a test starts
//	=== PAUSE TestX      a parallel test yields
//	=== CONT  TestX      it resumes, and later output is its own again
//	=== NAME  TestX      attribution changes with no state change
//	--- PASS: TestX (0s) a result, indented for subtests
//	    t.Log output     anything else belongs to the current test
//
// A line before any marker, or after a result with no new marker, belongs to
// the package rather than to a test — a panic in TestMain, or the final
// `FAIL`/`ok` line — and is emitted with no Test field, which is where
// reqreport expects package-level output too.

// event is the subset of a `go test -json` event that the report reads.
type jsonEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test,omitempty"`
	Output  string `json:"Output,omitempty"`
}

// convert turns one binary's `-test.v=test2json` stream into the JSON event
// stream reqreport and the runner's own tally read.
func convert(pkg string, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(w)

	current := "" // the test later output belongs to
	emit := func(action, test, output string) error {
		return enc.Encode(jsonEvent{Action: action, Package: pkg, Test: test, Output: output})
	}

	for scanner.Scan() {
		// A binary run with -test.v=test2json prefixes each line it writes as
		// the harness (rather than as the test) with 0x16. That byte is the
		// framing this mode exists to provide: it is what tells a converter
		// that `=== RUN` is a marker and not something a test happened to
		// print. It is consumed here, so it never reaches an Output field.
		line := strings.TrimLeft(scanner.Text(), "\x16")
		trimmed := strings.TrimLeft(line, " \t")

		// Attribution markers. `=== RUN` also starts the test.
		if name, ok := marker(trimmed, "=== RUN"); ok {
			current = name
			if err := emit("run", name, ""); err != nil {
				return err
			}
			if err := emit("output", name, line+"\n"); err != nil {
				return err
			}
			continue
		}
		for _, kind := range []string{"=== PAUSE", "=== CONT", "=== NAME"} {
			if name, ok := marker(trimmed, kind); ok {
				current = name
				if err := emit("output", name, line+"\n"); err != nil {
					return err
				}
				goto next
			}
		}

		// Results. The name may be a subtest ("TestX/case"), which is reported
		// as its own test, as `go test -json` does. Order differs from Go's:
		// it holds a parent's result until its subtests are known, and this
		// emits in stream order. Neither consumer reads the order.
		if action, name, ok := result(trimmed); ok {
			if err := emit("output", name, line+"\n"); err != nil {
				return err
			}
			if err := emit(action, name, ""); err != nil {
				return err
			}
			// Output after a result and before the next marker is the
			// package's, not the finished test's.
			current = ""
			continue
		}

		// The package's own trailer: `PASS`, `FAIL`, or an `ok …` line. Go's
		// converter reports these as a result for the package with no test
		// attached, and a report that counts packages reads them.
		if action, ok := packageResult(line); ok {
			if err := emit("output", "", line+"\n"); err != nil {
				return err
			}
			if err := emit(action, "", ""); err != nil {
				return err
			}
			current = ""
			continue
		}

		if err := emit("output", current, line+"\n"); err != nil {
			return err
		}
	next:
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading test output: %w", err)
	}
	return nil
}

// marker matches an attribution line such as "=== RUN   TestX" and returns the
// test name.
func marker(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if name == "" {
		return "", false
	}
	return name, true
}

// packageResult matches the trailer a test binary writes once it is done.
func packageResult(line string) (string, bool) {
	switch {
	case line == "PASS":
		return "pass", true
	case line == "FAIL":
		return "fail", true
	case strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "ok "):
		return "pass", true
	case strings.HasPrefix(line, "FAIL\t"), strings.HasPrefix(line, "FAIL "):
		return "fail", true
	}
	return "", false
}

// result matches "--- PASS: TestX (0.00s)" and returns the action and name.
func result(line string) (action, name string, ok bool) {
	if !strings.HasPrefix(line, "--- ") {
		return "", "", false
	}
	rest := strings.TrimPrefix(line, "--- ")
	for word, act := range map[string]string{"PASS:": "pass", "FAIL:": "fail", "SKIP:": "skip"} {
		if !strings.HasPrefix(rest, word) {
			continue
		}
		tail := strings.TrimSpace(strings.TrimPrefix(rest, word))
		// "TestX (0.00s)" — the name is everything before the timing.
		if idx := strings.LastIndex(tail, " ("); idx > 0 {
			tail = tail[:idx]
		}
		if tail == "" {
			return "", "", false
		}
		return act, strings.TrimSpace(tail), true
	}
	return "", "", false
}
