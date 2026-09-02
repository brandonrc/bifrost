// reqreport turns `go test -json` output into docs/requirements/
// traceability.md + .json. Status is derived from REQ lines and test
// outcomes; it is never typed by hand. A P3 milestone adds JUnit XML input
// for the L3 lane.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	in := flag.String("in", "", "comma-separated go test -json files")
	lane := flag.String("lane", "l2", "lane label: l2|l3")
	out := flag.String("out", "docs/requirements", "output directory")
	allowUntested := flag.Bool("allow-untested", false, "P0 only: do not fail on requirements with zero tests")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "reqreport: -in is required")
		os.Exit(2)
	}

	rep, err := Build(strings.Split(*in, ","), *lane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*out, "traceability.md"), []byte(rep.Markdown()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*out, "traceability.json"), append(js, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "reqreport:", err)
		os.Exit(1)
	}

	for _, row := range rep.Rows {
		fmt.Printf("req %2d  %-13s tests=%d pass=%d fail=%d nyb=%d\n",
			row.N, row.Status, row.Tests, row.Passed, row.Failed, row.NotYetBuilt)
	}

	if probs := rep.Problems(*allowUntested); len(probs) > 0 {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, "reqreport: PROBLEM:", p)
		}
		os.Exit(1)
	}
}
