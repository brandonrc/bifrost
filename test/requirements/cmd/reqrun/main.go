// Command reqrun runs the compiled requirement suite from inside the cluster
// it is testing.
//
// The grace lane (scripts/l3-grace.sh) cross-compiles the same binaries on a
// laptop, copies them over ssh and runs them there. That works, and it means
// the deployment can only be validated by someone with the repo, a Go
// toolchain and a tunnel. This is the other half: the binaries in an image, so
// a Job or a CronJob in the cluster can validate it on its own — nightly, or
// on demand from a `kubectl create job`.
//
// It is deliberately a Go program and not a shell script. The runtime image is
// UBI9-micro, which has no grep, no curl and no package manager to add them;
// the publish workflow already carries a comment about a check that was red
// forever because it shelled out to a tool the base does not ship.
//
// Environment:
//
//	REQ_TARGET               required: the targets.yaml target (grace, kind)
//	BIFROST_URL              API root; default http://bifrost.<ns>.svc:8484
//	BIFROST_NAMESPACE        namespace for that default; default bifrost
//	BIFROST_ADMIN_PASSWORD   local admin password (mount the Secret)
//	REQ_BIN_DIR              where the .test binaries are; default
//	                         /usr/local/lib/bifrost-requirements
//	REQ_OUT_DIR              where to write per-package output and the report;
//	                         default /tmp/req-out
//	REQ_TEST_TIMEOUT         per-package timeout; default 40m
//	REQ_PREFLIGHT_TIMEOUT    how long to wait for /healthz; default 3m
//	REQ_ONLY                 comma-separated package names to run (default all)
//
// Everything else the suite reads (REQ_RUN_ID, REQ_GATEWAY_DOMAIN,
// REQ_NOWGET_RAY_IMAGE, REQ_EVENTUALLY_TIMEOUT, …) is passed through from this
// process's environment untouched, so the manifest is the single place that
// tunes a lane.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "reqrun: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	target := os.Getenv("REQ_TARGET")
	if target == "" {
		return fmt.Errorf("REQ_TARGET is required (a target in targets.yaml, such as grace)")
	}
	binDir := envOr("REQ_BIN_DIR", "/usr/local/lib/bifrost-requirements")
	outDir := envOr("REQ_OUT_DIR", "/tmp/req-out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	url := os.Getenv("BIFROST_URL")
	if url == "" {
		url = fmt.Sprintf("http://bifrost.%s.svc:8484", envOr("BIFROST_NAMESPACE", "bifrost"))
	}
	url = strings.TrimRight(url, "/")
	if err := os.Setenv("BIFROST_URL", url); err != nil {
		return fmt.Errorf("setting BIFROST_URL: %w", err)
	}
	// A run id groups every object the suite creates, and every guard and
	// postflight sweep is written in terms of it. One id per Job, not per
	// package, so a package that dies mid-test is still swept by the next.
	if os.Getenv("REQ_RUN_ID") == "" {
		if err := os.Setenv("REQ_RUN_ID", fmt.Sprintf("t%x", time.Now().Unix())); err != nil {
			return fmt.Errorf("setting REQ_RUN_ID: %w", err)
		}
	}

	fmt.Printf("reqrun: target=%s url=%s run=%s\n", target, url, os.Getenv("REQ_RUN_ID"))
	if os.Getenv("BIFROST_ADMIN_PASSWORD") == "" {
		fmt.Println("reqrun: BIFROST_ADMIN_PASSWORD is empty; every API test will fail to seed principals")
	}

	if err := waitHealthy(url); err != nil {
		return err
	}

	binaries, err := discover(binDir, os.Getenv("REQ_ONLY"))
	if err != nil {
		return err
	}
	fmt.Printf("reqrun: %d packages\n\n", len(binaries))

	timeout := envOr("REQ_TEST_TIMEOUT", "40m")
	var jsonFiles []string
	failed := make([]string, 0, len(binaries))
	for _, bin := range binaries {
		name := strings.TrimSuffix(filepath.Base(bin), ".test")
		outPath := filepath.Join(outDir, name+".json")
		ok, err := runPackage(bin, name, outPath, timeout)
		if err != nil {
			return err
		}
		jsonFiles = append(jsonFiles, outPath)
		if !ok {
			failed = append(failed, name)
		}
	}

	// The report is the point of a scheduled run: a pass count says the lane
	// was green, the matrix says which requirements it was green *about*.
	reportDir := filepath.Join(outDir, "report")
	if err := reqreport(jsonFiles, reportDir); err != nil {
		fmt.Fprintf(os.Stderr, "reqrun: report: %v\n", err)
	}

	fmt.Println()
	if len(failed) > 0 {
		return fmt.Errorf("%d package(s) failed: %s", len(failed), strings.Join(failed, " "))
	}
	fmt.Println("reqrun: every package passed")
	return nil
}

// waitHealthy blocks until the API answers /healthz, so a CronJob that starts
// during a rollout waits instead of failing every test at once.
func waitHealthy(url string) error {
	budget := envDuration("REQ_PREFLIGHT_TIMEOUT", 3*time.Minute)
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s/healthz did not answer within %s (last: %s)", url, budget, last)
}

// discover lists the test binaries to run, in a stable order.
//
// Order matters for the report, not for correctness: each package is
// independent and cleans up after itself, but a fixed order makes two runs
// comparable line by line.
func discover(dir, only string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(only, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	var bins []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".test") {
			continue
		}
		if len(wanted) > 0 && !wanted[strings.TrimSuffix(e.Name(), ".test")] {
			continue
		}
		bins = append(bins, filepath.Join(dir, e.Name()))
	}
	if len(bins) == 0 {
		return nil, fmt.Errorf("no .test binaries in %s (REQ_ONLY=%q)", dir, only)
	}
	sort.Strings(bins)
	return bins, nil
}

// runPackage runs one package's binary, writing its test2json stream to
// outPath and a one-line summary to stdout.
// importPath prefixes a package name to match what `go test -json` reports,
// which is what reqreport keys a package by.
const importPath = "github.com/brandonrc/bifrost/test/requirements/"

func runPackage(bin, name, outPath, timeout string) (bool, error) {
	file, err := os.Create(outPath)
	if err != nil {
		return false, fmt.Errorf("creating %s: %w", outPath, err)
	}
	defer func() { _ = file.Close() }()

	fmt.Printf("=== %s\n", name)
	// The binary's own stream is kept next to the events: it is what a person
	// reads when a failure needs more than the summary, and it is the input
	// the conversion is answerable to.
	rawPath := strings.TrimSuffix(outPath, ".json") + ".out"
	raw, err := os.Create(rawPath)
	if err != nil {
		return false, fmt.Errorf("creating %s: %w", rawPath, err)
	}
	defer func() { _ = raw.Close() }()

	cmd := exec.Command(bin, "-test.v=test2json", "-test.timeout", timeout)
	cmd.Stdout = raw
	cmd.Stderr = raw
	start := time.Now()
	runErr := cmd.Run()

	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("rereading %s: %w", rawPath, err)
	}
	if err := convert(importPath+name, raw, file); err != nil {
		return false, fmt.Errorf("converting %s: %w", name, err)
	}
	// Read the stream back for the summary rather than parsing it inline: the
	// file is the artifact a human or reqreport reads afterwards, so it is the
	// thing whose contents should decide what this line says.
	pass, fail, skip := tally(outPath)
	status := "ok"
	if runErr != nil {
		status = "FAIL"
	}
	fmt.Printf("--- %s %s  %d passed, %d failed, %d skipped  (%s)\n",
		status, name, pass, fail, skip, time.Since(start).Round(time.Second))
	if runErr != nil {
		for _, line := range failures(outPath) {
			fmt.Printf("      %s\n", line)
		}
	}
	return runErr == nil, nil
}

func reqreport(jsonFiles []string, outDir string) error {
	cmd := exec.Command("/usr/local/bin/reqreport",
		"-in", strings.Join(jsonFiles, ","), "-lane", "l3", "-out", outDir, "-allow-untested")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// tally counts the package's test results from its test2json stream.
func tally(path string) (pass, fail, skip int) {
	each(path, func(ev event) {
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			return
		}
		switch ev.Action {
		case "pass":
			pass++
		case "fail":
			fail++
		case "skip":
			skip++
		}
	})
	return pass, fail, skip
}

// failures names the tests that failed and the last line each of them logged,
// which is what makes a Job's log readable without downloading the stream.
func failures(path string) []string {
	last := map[string]string{}
	var failedTests []string
	each(path, func(ev event) {
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			return
		}
		switch ev.Action {
		case "output":
			line := strings.TrimSpace(ev.Output)
			// The REQ: marker lines are the coverage declaration, not a result.
			if line != "" && !strings.HasPrefix(line, "===") && !strings.HasPrefix(line, "---") &&
				!strings.Contains(line, "REQ: kind=") {
				last[ev.Test] = line
			}
		case "fail":
			failedTests = append(failedTests, ev.Test)
		}
	})
	out := make([]string, 0, len(failedTests))
	for _, name := range failedTests {
		out = append(out, fmt.Sprintf("%s: %s", name, truncate(last[name])))
	}
	return out
}

type event struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

// each calls fn for every event in a test2json stream, ignoring lines that are
// not events: a binary that panics before the harness starts writes plain text.
func each(path string, fn func(event)) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev event
		if json.Unmarshal(scanner.Bytes(), &ev) == nil && ev.Action != "" {
			fn(ev)
		}
	}
}

func truncate(s string) string {
	const max = 160
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
