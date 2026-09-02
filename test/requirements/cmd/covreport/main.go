// covreport computes risk-tiered coverage from a Go coverprofile and gates
// it against .coverage-ratchet.json: a tier may drop at most 0.5 points;
// -update raises the ratchet to the measured value. It prints the exclusion
// list on every run so "excluded" is never invisible (spec §4).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "covreport:", err)
		os.Exit(1)
	}
}

func main() {
	profile := flag.String("profile", "coverage.txt", "coverprofile path")
	tiersPath := flag.String("tiers", ".coverage-tiers.yaml", "tier policy")
	excludePath := flag.String("exclude", ".coverage-exclude", "exclusion substrings, one per line")
	ratchetPath := flag.String("ratchet", ".coverage-ratchet.json", "ratchet file")
	update := flag.Bool("update", false, "raise the ratchet to measured values")
	flag.Parse()

	var p Policy
	tb, err := os.ReadFile(*tiersPath)
	must(err)
	must(yaml.Unmarshal(tb, &p))
	eb, err := os.ReadFile(*excludePath)
	must(err)
	for _, l := range strings.Split(string(eb), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			p.Exclude = append(p.Exclude, l)
		}
	}
	ratchet := map[string]float64{}
	if rb, err := os.ReadFile(*ratchetPath); err == nil {
		must(json.Unmarshal(rb, &ratchet))
	}

	got, err := Compute(*profile, p)
	must(err)

	fmt.Println("exclusions:", strings.Join(p.Exclude, " "))
	failed := false
	for _, t := range p.Tiers {
		have := got[t.Name]
		r := ratchet[t.Name]
		ok := WithinRatchet(have, r)
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed = true
		}
		fmt.Printf("%s %-6s %s (ratchet %s, spec floor %s)\n", mark, t.Name, fmtPct(have), fmtPct(r), fmtPct(t.Floor))
		if *update && have > r {
			ratchet[t.Name] = float64(int(have*10)) / 10
		}
	}
	if *update {
		js, _ := json.MarshalIndent(ratchet, "", "  ")
		must(os.WriteFile(*ratchetPath, append(js, '\n'), 0o644))
		fmt.Println("ratchet updated:", *ratchetPath)
	}
	if failed {
		os.Exit(1)
	}
}
