package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Tier struct {
	Name     string   `yaml:"name"`
	Floor    float64  `yaml:"floor"`
	Packages []string `yaml:"packages"`
}

type Policy struct {
	Tiers   []Tier `yaml:"tiers"`
	Exclude []string
}

const modulePrefix = "github.com/brandonrc/bifrost/"

// Compute returns covered-statement percentage per tier from a coverprofile.
// Lines: file:startLine.col,endLine.col numStatements count
func Compute(profile string, p Policy) (map[string]float64, error) {
	fh, err := os.Open(profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	total := map[string]int{}
	covered := map[string]int{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		fileEnd := strings.Index(line, ":")
		if fileEnd < 0 {
			continue
		}
		file := strings.TrimPrefix(line[:fileEnd], modulePrefix)
		if excluded(file, p.Exclude) {
			continue
		}
		tier := tierFor(file, p.Tiers)
		if tier == "" {
			continue
		}
		fields := strings.Fields(line[fileEnd+1:])
		if len(fields) != 3 {
			continue
		}
		n, _ := strconv.Atoi(fields[1])
		c, _ := strconv.Atoi(fields[2])
		total[tier] += n
		if c > 0 {
			covered[tier] += n
		}
	}
	out := map[string]float64{}
	for _, t := range p.Tiers {
		if total[t.Name] == 0 {
			out[t.Name] = 0
			continue
		}
		out[t.Name] = 100 * float64(covered[t.Name]) / float64(total[t.Name])
	}
	return out, sc.Err()
}

func excluded(file string, patterns []string) bool {
	for _, pat := range patterns {
		if pat != "" && strings.Contains("/"+file, pat) {
			return true
		}
	}
	return false
}

func tierFor(file string, tiers []Tier) string {
	for _, t := range tiers {
		for _, pkg := range t.Packages {
			if strings.HasPrefix(file, pkg+"/") {
				return t.Name
			}
		}
	}
	return ""
}

// WithinRatchet allows a drop of at most 0.5 points (spec §4).
func WithinRatchet(have, ratchet float64) bool { return have >= ratchet-0.5 }

func fmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v) }
