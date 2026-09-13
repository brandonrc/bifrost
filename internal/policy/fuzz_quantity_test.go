package policy

import (
	"math"
	"testing"
)

// FuzzQuantity fuzzes the quantity parsers. The audit invariant — made the
// fuzz invariant, not just a crash check: a parser success ALWAYS yields a
// finite, non-negative value (NaN/Inf/negative are denied), and FitsWithin
// never admits a NaN or +Inf demand against ANY parsed limit, and agrees
// with plain `<=` for every parsed demand/limit pair.
func FuzzQuantity(f *testing.F) {
	for _, s := range []string{
		// Valid forms from the table tests.
		"1", "500m", "2", "2Gi", "512Mi", "1Ti", "1G", "1024Ki", "1000K",
		"1M", "1T", "1073741824", " 2Gi ", "64", "512Mi", "2k", "1.5",
		"1Ki", "1Pi", "1Ei", "1P", "1E", "5n", "5u", "1e3", "0", "0m",
		".5", "1e-3",
		// Denied forms from the table tests.
		"abc", "nope", "x", "banana", "", "-3", "-1Gi", "NaN", "-1",
		"-500m", "inf", "2xm", "-1G",
		// Adversarial: overflow/underflow, case variants, sign tricks.
		"nan", "NAN", "NaNm", "Inf", "INF", "+Inf", "-inf", "1e309",
		"1e308E", "9e999", "1e-400", "+1", "+500m", "-0", "1E2m",
		"0x10", "1_000", "1,5", "  ", "\t1\n", "1mM", "m", "Gi", "e3",
		"1.2.3", "٣", // Arabic-Indic digit: ParseFloat must reject
	} {
		f.Add(s, s)
	}
	// Explicit demand/limit pairs for the FitsWithin cross-checks.
	for _, p := range [][2]string{
		{"1", "10"}, {"10", "1"}, {"500m", "0.5"}, {"2Gi", "2147483648"},
		{"NaN", "10"}, {"inf", "inf"}, {"-1", "-1"}, {"", ""},
	} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, s, limitStr string) {
		// Invariant 1: parser success implies finite non-negative.
		check := func(name string, v float64, err error) {
			if err == nil && (math.IsNaN(v) || math.IsInf(v, 0) || v < 0) {
				t.Fatalf("%s(%q) = %v, nil error — NaN/Inf/negative must be denied", name, s, v)
			}
		}
		v1, e1 := CPUCores(s)
		check("CPUCores", v1, e1)
		v2, e2 := MemGiB(s)
		check("MemGiB", v2, e2)
		v3, e3 := ParseQuantity(s)
		check("ParseQuantity", v3, e3)
		g := s
		v4, e4 := GPUCount(&g)
		check("GPUCount", v4, e4)

		lv, lerr := ParseQuantity(limitStr)
		if lerr == nil {
			// Invariant 2: NaN/+Inf demands are denied against every limit.
			if (ResourceMap{"r": math.NaN()}).FitsWithin(ResourceMap{"r": lv}) {
				t.Fatalf("NaN demand fit within limit %v (from %q)", lv, limitStr)
			}
			if (ResourceMap{"r": math.Inf(1)}).FitsWithin(ResourceMap{"r": lv}) {
				t.Fatalf("+Inf demand fit within limit %v (from %q)", lv, limitStr)
			}
			// Invariant 3: for parsed values, FitsWithin is exactly `<=`.
			if e3 == nil {
				got := (ResourceMap{"r": v3}).FitsWithin(ResourceMap{"r": lv})
				if want := v3 <= lv; got != want {
					t.Fatalf("FitsWithin(%v, %v) = %v, want %v", v3, lv, got, want)
				}
			}
		}
		if e3 == nil {
			// Invariant 4: every parsed quantity fits within itself.
			if !(ResourceMap{"r": v3}).FitsWithin(ResourceMap{"r": v3}) {
				t.Fatalf("ParseQuantity(%q) = %v does not fit within itself", s, v3)
			}
		}
	})
}
