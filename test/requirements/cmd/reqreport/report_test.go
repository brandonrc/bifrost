package main

import (
	"os"
	"strings"
	"testing"
)

func TestBuildFromFixture(t *testing.T) {
	rep, err := Build([]string{"testdata/l2.json"}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	r3 := rep.Rows[2]
	if r3.N != 3 || r3.Tests != 2 || r3.Passed != 1 || r3.Failed != 1 || r3.Status != "failing" {
		t.Errorf("row 3 = %+v", r3)
	}
	r5 := rep.Rows[4]
	if r5.NotYetBuilt != 1 || r5.Status != "not-yet-built" {
		t.Errorf("row 5 = %+v", r5)
	}
	r6 := rep.Rows[5]
	if r6.Tests != 1 || r6.Skipped != 1 || len(r6.SkipReasons) != 1 {
		t.Errorf("row 6 = %+v", r6)
	}
	// Row 7 pins the parent/subtest counting model (Task 8 decision 3): a
	// REQ line logged by the PARENT test (before its t.Run loop) is
	// attributed to the parent's own Test name and outcome, never to the
	// subtests that carry pass/fail individually. go test -json marks the
	// parent itself "fail" when any subtest fails, so this fixture's
	// TestE/sub_fail failing is what makes row 7 fail, via TestE's own
	// fail action -- not via any subtest event.
	r7 := rep.Rows[6]
	if r7.Tests != 1 || r7.Failed != 1 || r7.Status != "failing" {
		t.Errorf("row 7 = %+v", r7)
	}
	if len(r7.TestNames) != 1 || r7.TestNames[0] != "TestE" {
		t.Errorf("row 7 test names = %v, want [TestE]", r7.TestNames)
	}
	if len(rep.Untested()) != 14 {
		t.Errorf("untested = %d, want 14", len(rep.Untested()))
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	rep, err := Build([]string{"testdata/l2.json"}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	got := rep.Markdown()
	want, err := os.ReadFile("testdata/expected.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
		t.Errorf("markdown differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestMarkerDriftIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/drift.json"
	if err := os.WriteFile(p, []byte(`{"Action":"run","Test":"T"}
{"Action":"output","Test":"T","Output":"REQ: kind=notyetbuilt req=5 reason=\"x\" outcome=passed\n"}
{"Action":"fail","Test":"T"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Build([]string{p}, "l2")
	if err != nil {
		t.Fatal(err)
	}
	if errs := rep.Problems(true); len(errs) == 0 || !strings.Contains(errs[0], "appears built") {
		t.Errorf("Problems = %v; want a marker-drift error", errs)
	}
}
