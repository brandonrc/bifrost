package main

import "testing"

func TestTierPercentages(t *testing.T) {
	policy := Policy{
		Tiers:   []Tier{{Name: "tier1", Packages: []string{"internal/core"}}, {Name: "tier2", Packages: []string{"internal/api"}}},
		Exclude: []string{"/cmd/", "zz_generated_api.go"},
	}
	got, err := Compute("testdata/profile.txt", policy)
	if err != nil {
		t.Fatal(err)
	}
	if got["tier1"] != 50.0 { // 2 of 4 statements covered
		t.Errorf("tier1 = %v, want 50", got["tier1"])
	}
	if got["tier2"] != 100.0 { // generated file excluded; 4 of 4
		t.Errorf("tier2 = %v, want 100", got["tier2"])
	}
}

func TestRatchetTolerance(t *testing.T) {
	cases := []struct {
		have, ratchet float64
		ok            bool
	}{{90, 90, true}, {89.6, 90, true}, {89.4, 90, false}, {95, 90, true}}
	for _, c := range cases {
		if got := WithinRatchet(c.have, c.ratchet); got != c.ok {
			t.Errorf("WithinRatchet(%v,%v)=%v want %v", c.have, c.ratchet, got, c.ok)
		}
	}
}
