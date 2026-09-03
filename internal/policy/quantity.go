package policy

// Minimal Kubernetes quantity parsing for CPU, memory, GPU, and arbitrary
// resource strings. Ported from the predecessor's policy crate, src/quantity.rs.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// finiteNonNeg rejects NaN, infinities, and negatives — a quantity must be
// a finite, non-negative number (a negative demand would lower a project's
// quota usage and let over-provisioning slip through).
func finiteNonNeg(v float64, what string) (float64, error) {
	if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0.0 {
		return v, nil
	}
	return 0, fmt.Errorf("invalid %s: %v", what, v)
}

// CPUCores parses a CPU quantity to whole cores: "1" -> 1.0, "500m" -> 0.5.
func CPUCores(s string) (float64, error) {
	s = strings.TrimSpace(s)
	var v float64
	if milli, ok := strings.CutSuffix(s, "m"); ok {
		f, err := strconv.ParseFloat(milli, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu %q", s)
		}
		v = f / 1000.0
	} else {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu %q", s)
		}
		v = f
	}
	return finiteNonNeg(v, "cpu")
}

// gib is one gibibyte in bytes.
const gib = 1024.0 * 1024.0 * 1024.0

// MemGiB parses a memory quantity to GiB. Supports Ki/Mi/Gi/Ti (binary) and
// K/M/G/T (decimal); a bare number is bytes.
func MemGiB(s string) (float64, error) {
	s = strings.TrimSpace(s)
	num, bytesPer := s, 1.0
	switch {
	case strings.HasSuffix(s, "Ki"):
		num, bytesPer = strings.TrimSuffix(s, "Ki"), 1024.0
	case strings.HasSuffix(s, "Mi"):
		num, bytesPer = strings.TrimSuffix(s, "Mi"), 1024.0*1024.0
	case strings.HasSuffix(s, "Gi"):
		num, bytesPer = strings.TrimSuffix(s, "Gi"), gib
	case strings.HasSuffix(s, "Ti"):
		num, bytesPer = strings.TrimSuffix(s, "Ti"), gib*1024.0
	case strings.HasSuffix(s, "K"):
		num, bytesPer = strings.TrimSuffix(s, "K"), 1_000.0
	case strings.HasSuffix(s, "M"):
		num, bytesPer = strings.TrimSuffix(s, "M"), 1_000_000.0
	case strings.HasSuffix(s, "G"):
		num, bytesPer = strings.TrimSuffix(s, "G"), 1_000_000_000.0
	case strings.HasSuffix(s, "T"):
		num, bytesPer = strings.TrimSuffix(s, "T"), 1_000_000_000_000.0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q", s)
	}
	return finiteNonNeg(f*bytesPer/gib, "memory")
}

// GPUCount parses an optional GPU count. nil or "" -> 0.
func GPUCount(s *string) (float64, error) {
	if s == nil || *s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(*s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid gpu %q", *s)
	}
	return finiteNonNeg(f, "gpu")
}

// ki is one kibi (2^10), the binary-suffix multiplier base.
const ki = 1024.0

// ParseQuantity parses an arbitrary Kubernetes quantity to a bare number
// (ADR-0010: pool flavors quota any resource name, so no resource-specific
// unit conversion). Binary suffixes (Ki…Ei) multiply by 1024^n, decimal
// suffixes (n, u, m, k, M…E) by 10^n; a bare number (including exponent
// notation, via float64 parsing) is its own value. Unlike MemGiB, a bare
// number is NOT treated as bytes — pool quantities are counts of whatever
// the resource key measures.
func ParseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	num, mult := s, 1.0
	switch {
	case strings.HasSuffix(s, "Ki"):
		num, mult = strings.TrimSuffix(s, "Ki"), ki
	case strings.HasSuffix(s, "Mi"):
		num, mult = strings.TrimSuffix(s, "Mi"), ki*ki
	case strings.HasSuffix(s, "Gi"):
		num, mult = strings.TrimSuffix(s, "Gi"), ki*ki*ki
	case strings.HasSuffix(s, "Ti"):
		num, mult = strings.TrimSuffix(s, "Ti"), math.Pow(ki, 4)
	case strings.HasSuffix(s, "Pi"):
		num, mult = strings.TrimSuffix(s, "Pi"), math.Pow(ki, 5)
	case strings.HasSuffix(s, "Ei"):
		num, mult = strings.TrimSuffix(s, "Ei"), math.Pow(ki, 6)
	case strings.HasSuffix(s, "n"):
		num, mult = strings.TrimSuffix(s, "n"), 1e-9
	case strings.HasSuffix(s, "u"):
		num, mult = strings.TrimSuffix(s, "u"), 1e-6
	case strings.HasSuffix(s, "m"):
		num, mult = strings.TrimSuffix(s, "m"), 1e-3
	case strings.HasSuffix(s, "k"):
		num, mult = strings.TrimSuffix(s, "k"), 1e3
	case strings.HasSuffix(s, "M"):
		num, mult = strings.TrimSuffix(s, "M"), 1e6
	case strings.HasSuffix(s, "G"):
		num, mult = strings.TrimSuffix(s, "G"), 1e9
	case strings.HasSuffix(s, "T"):
		num, mult = strings.TrimSuffix(s, "T"), 1e12
	case strings.HasSuffix(s, "P"):
		num, mult = strings.TrimSuffix(s, "P"), 1e15
	case strings.HasSuffix(s, "E"):
		num, mult = strings.TrimSuffix(s, "E"), 1e18
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q", s)
	}
	return finiteNonNeg(f*mult, "quantity")
}
