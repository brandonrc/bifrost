package policy

import (
	"math"
	"testing"
)

// Ported from mobula-policy/src/quantity.rs #[cfg(test)] mod tests.

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}

func wantEq(t *testing.T, v float64, err error, want float64) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != want {
		t.Fatalf("got %v, want %v", v, want)
	}
}

func wantApprox(t *testing.T, v float64, err error, want, tol float64) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !approxEqual(v, want, tol) {
		t.Fatalf("got %v, want ~%v", v, want)
	}
}

func wantErr(t *testing.T, err error, input string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error for %q", input)
	}
}

func TestCPU(t *testing.T) {
	v, err := CPUCores("1")
	wantEq(t, v, err, 1.0)
	v, err = CPUCores("500m")
	wantEq(t, v, err, 0.5)
	v, err = CPUCores("2")
	wantEq(t, v, err, 2.0)
	_, err = CPUCores("abc")
	wantErr(t, err, "abc")
}

func TestMemory(t *testing.T) {
	v, err := MemGiB("2Gi")
	wantEq(t, v, err, 2.0)
	v, err = MemGiB("512Mi")
	wantEq(t, v, err, 0.5)
	v, err = MemGiB("1Ti")
	wantEq(t, v, err, 1024.0)
	// decimal GB -> GiB
	v, err = MemGiB("1G")
	wantApprox(t, v, err, 0.9313, 0.001)
	_, err = MemGiB("nope")
	wantErr(t, err, "nope")
}

func TestGPU(t *testing.T) {
	v, err := GPUCount(nil)
	wantEq(t, v, err, 0.0)
	two := "2"
	v, err = GPUCount(&two)
	wantEq(t, v, err, 2.0)
	x := "x"
	_, err = GPUCount(&x)
	wantErr(t, err, "x")
}

func TestGeneralQuantity(t *testing.T) {
	v, err := ParseQuantity("64")
	wantEq(t, v, err, 64.0)
	v, err = ParseQuantity("500m")
	wantEq(t, v, err, 0.5)
	v, err = ParseQuantity("512Mi")
	wantEq(t, v, err, 512.0*1024.0*1024.0)
	v, err = ParseQuantity("1Gi")
	wantEq(t, v, err, 1073741824.0)
	v, err = ParseQuantity("2k")
	wantEq(t, v, err, 2000.0)
	v, err = ParseQuantity("1.5")
	wantEq(t, v, err, 1.5)
	_, err = ParseQuantity("banana")
	wantErr(t, err, "banana")
	_, err = ParseQuantity("")
	wantErr(t, err, "")
	_, err = ParseQuantity("-3")
	wantErr(t, err, "-3")
}

func TestMemorySuffixesBinaryAndDecimal(t *testing.T) {
	v, err := MemGiB("1024Ki")
	wantEq(t, v, err, 1024.0*1024.0/gib)
	v, err = MemGiB("1000K")
	wantEq(t, v, err, 1_000_000.0/gib)
	v, err = MemGiB("1M")
	wantEq(t, v, err, 1_000_000.0/gib)
	v, err = MemGiB("1T")
	wantEq(t, v, err, 1_000_000_000_000.0/gib)
	// A bare number is bytes.
	v, err = MemGiB("1073741824")
	wantEq(t, v, err, 1.0)
	// Surrounding whitespace is tolerated.
	v, err = MemGiB(" 2Gi ")
	wantEq(t, v, err, 2.0)
	// Negative and non-finite amounts are rejected.
	_, err = MemGiB("-1Gi")
	wantErr(t, err, "-1Gi")
	_, err = MemGiB("NaN")
	wantErr(t, err, "NaN")
}

func TestCPURejectsNegativeAndNonFinite(t *testing.T) {
	_, err := CPUCores("-1")
	wantErr(t, err, "-1")
	_, err = CPUCores("-500m")
	wantErr(t, err, "-500m")
	_, err = CPUCores("inf")
	wantErr(t, err, "inf")
	_, err = CPUCores("2xm")
	wantErr(t, err, "2xm")
}

func TestGPURejectsNegative(t *testing.T) {
	empty := ""
	v, err := GPUCount(&empty)
	wantEq(t, v, err, 0.0)
	neg := "-1"
	_, err = GPUCount(&neg)
	wantErr(t, err, "-1")
}

func TestQuantityFullSuffixTable(t *testing.T) {
	// Binary suffixes multiply by 1024^n.
	v, err := ParseQuantity("1Ki")
	wantEq(t, v, err, 1024.0)
	v, err = ParseQuantity("1Ti")
	wantEq(t, v, err, math.Pow(1024.0, 4))
	v, err = ParseQuantity("1Pi")
	wantEq(t, v, err, math.Pow(1024.0, 5))
	v, err = ParseQuantity("1Ei")
	wantEq(t, v, err, math.Pow(1024.0, 6))
	// Decimal suffixes multiply by 10^n, sub-unit by 10^-n.
	v, err = ParseQuantity("1M")
	wantEq(t, v, err, 1e6)
	v, err = ParseQuantity("1G")
	wantEq(t, v, err, 1e9)
	v, err = ParseQuantity("1T")
	wantEq(t, v, err, 1e12)
	v, err = ParseQuantity("1P")
	wantEq(t, v, err, 1e15)
	v, err = ParseQuantity("1E")
	wantEq(t, v, err, 1e18)
	v, err = ParseQuantity("5n")
	wantEq(t, v, err, 5e-9)
	v, err = ParseQuantity("5u")
	wantApprox(t, v, err, 5e-6, 1e-12)
	// Exponent notation on a bare number parses via float64.
	v, err = ParseQuantity("1e3")
	wantEq(t, v, err, 1000.0)
	// Negative and non-finite quantities are rejected.
	_, err = ParseQuantity("-1G")
	wantErr(t, err, "-1G")
	_, err = ParseQuantity("inf")
	wantErr(t, err, "inf")
}
