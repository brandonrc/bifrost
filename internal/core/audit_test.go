package core

import (
	"encoding/json"
	"testing"
)

// Ported from the predecessor's core crate, src/audit.rs #[cfg(test)] mod tests.

func strPtr(s string) *string { return &s }

func u16Ptr(v uint16) *uint16 { return &v }

func TestDecisionRoundTripsSnakeCase(t *testing.T) {
	for _, d := range []AuditDecision{AuditDecisionAllow, AuditDecisionDeny} {
		got, ok := ParseAuditDecision(d.AsStr())
		if !ok || got != d {
			t.Fatalf("ParseAuditDecision(%s.AsStr()) = %v, %v", d, got, ok)
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `"` + d.AsStr() + `"`
		if string(b) != want {
			t.Fatalf("json = %s, want %s", b, want)
		}
	}
	if _, ok := ParseAuditDecision("bogus"); ok {
		t.Fatal("expected ParseAuditDecision(\"bogus\") to fail")
	}
}

func TestOptionFieldsSerializeNullPresent(t *testing.T) {
	event := AuditEvent{
		Ts:       1_755_280_000,
		Decision: AuditDecisionDeny,
	}
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, field := range []string{
		"subject", "reason", "action", "cluster", "method",
		"path", "status", "latency_ms", "required",
	} {
		raw, ok := v[field]
		if !ok {
			t.Fatalf("%s must be present as null", field)
		}
		if raw != nil {
			t.Fatalf("%s must be present as null, got %v", field, raw)
		}
	}
	roles, ok := v["granted_roles"].([]any)
	if !ok || len(roles) != 0 {
		t.Fatalf("granted_roles = %v, want []", v["granted_roles"])
	}
	if v["decision"] != "deny" {
		t.Fatalf("decision = %v, want deny", v["decision"])
	}
}

func TestEffectiveLimitDefaultsAndClamps(t *testing.T) {
	if got := (AuditFilter{}).EffectiveLimit(); got != 100 {
		t.Fatalf("default EffectiveLimit = %d, want 100", got)
	}
	zero := uint32(0)
	if got := (AuditFilter{Limit: &zero}).EffectiveLimit(); got != 1 {
		t.Fatalf("EffectiveLimit(0) = %d, want 1", got)
	}
	ten := uint32(10_000)
	if got := (AuditFilter{Limit: &ten}).EffectiveLimit(); got != 1000 {
		t.Fatalf("EffectiveLimit(10000) = %d, want 1000", got)
	}
}

func TestMatchesAppliesEachCondition(t *testing.T) {
	event := AuditEvent{
		Ts:       100,
		Subject:  strPtr("u1"),
		Decision: AuditDecisionDeny,
		Reason:   strPtr("insufficient_permission"),
		Cluster:  strPtr("demo"),
		Method:   strPtr("GET"),
		Path:     strPtr("/api/jobs/abc"),
		Status:   u16Ptr(403),
	}
	yes := func(f AuditFilter) bool { return f.Matches(&event) }

	if !yes(AuditFilter{}) {
		t.Fatal("empty filter should match")
	}
	hundred := uint64(100)
	if !yes(AuditFilter{From: &hundred, To: &hundred}) {
		t.Fatal("from=to=100 should match ts=100")
	}
	oh1 := uint64(101)
	if yes(AuditFilter{From: &oh1}) {
		t.Fatal("from=101 should not match ts=100")
	}
	ninetynine := uint64(99)
	if yes(AuditFilter{To: &ninetynine}) {
		t.Fatal("to=99 should not match ts=100")
	}
	if !yes(AuditFilter{PathPrefix: strPtr("/api/jobs")}) {
		t.Fatal("path prefix /api/jobs should match")
	}
	if yes(AuditFilter{PathPrefix: strPtr("/api/v1")}) {
		t.Fatal("path prefix /api/v1 should not match")
	}
	if yes(AuditFilter{MinStatus: u16Ptr(500)}) {
		t.Fatal("min_status=500 should not match status=403")
	}
	if yes(AuditFilter{Subject: strPtr("other")}) {
		t.Fatal("subject=other should not match")
	}
	// A filter on a field the event lacks never matches.
	noSubject := AuditEvent{}
	if (AuditFilter{Subject: strPtr("u1")}).Matches(&noSubject) {
		t.Fatal("subject filter should not match an event with no subject")
	}
}

// Added (not ported from Rust): fix round 1 (review finding M2). A
// zero-value AuditEvent (built as a Go struct literal without setting
// Decision) must still marshal Decision as the documented Rust default
// (#[default] Allow), not the Go zero value "".
func TestAuditEventMarshalsZeroValueDecisionAsDefault(t *testing.T) {
	var event AuditEvent
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if v["decision"] != string(AuditDecisionAllow) {
		t.Fatalf("decision = %v, want %q", v["decision"], AuditDecisionAllow)
	}
}
