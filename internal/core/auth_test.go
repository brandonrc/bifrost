package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// Ported from the predecessor's core crate, src/auth.rs #[cfg(test)] mod tests.

func TestLocalRoleRoundTrips(t *testing.T) {
	for _, r := range []LocalRole{LocalRoleViewer, LocalRoleDeveloper, LocalRoleOperator, LocalRoleAdmin} {
		got, ok := ParseLocalRole(r.AsStr())
		if !ok || got != r {
			t.Fatalf("ParseLocalRole(%s.AsStr()) = %v, %v", r, got, ok)
		}
	}
	if _, ok := ParseLocalRole("bogus"); ok {
		t.Fatal("expected ParseLocalRole(\"bogus\") to fail")
	}
}

func TestViewsCarryNoHashes(t *testing.T) {
	rec := LocalUserRecord{
		Username:     "alice",
		Email:        nil,
		PasswordHash: "$2b$12$secret",
		Role:         LocalRoleAdmin,
		Disabled:     false,
		CreatedAt:    1,
		FailedLogins: 0,
		LockedUntil:  nil,
	}
	b, err := json.Marshal(rec.View())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	userJSON := string(b)
	if strings.Contains(userJSON, "secret") || strings.Contains(userJSON, "hash") {
		t.Fatalf("view leaked hash material: %s", userJSON)
	}

	tok := ApiTokenRecord{
		Prefix:     "abcd1234",
		TokenHash:  "$2b$12$secret",
		Username:   "alice",
		Label:      "ci",
		CreatedAt:  1,
		ExpiresAt:  2,
		Revoked:    false,
		LastUsedAt: nil,
	}
	b, err = json.Marshal(tok.View())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tokJSON := string(b)
	if strings.Contains(tokJSON, "secret") || strings.Contains(tokJSON, "hash") {
		t.Fatalf("view leaked hash material: %s", tokJSON)
	}
}

// Added (not ported from Rust): the Rust reference makes LocalUserRecord
// and ApiTokenRecord deliberately non-Serialize, so marshaling either is a
// Rust compile error. Go has no compile-time equivalent, so this
// characterizes the runtime guard (MarshalJSON always fails) added in fix
// round 1 (review finding H1) to close the gap.
func TestLocalUserRecordNeverMarshals(t *testing.T) {
	rec := LocalUserRecord{Username: "alice", PasswordHash: "$2b$12$secret"}
	if _, err := json.Marshal(rec); err == nil {
		t.Fatal("expected json.Marshal(LocalUserRecord) to fail")
	}
	if _, err := json.Marshal(&rec); err == nil {
		t.Fatal("expected json.Marshal(*LocalUserRecord) to fail")
	}
}

func TestApiTokenRecordNeverMarshals(t *testing.T) {
	tok := ApiTokenRecord{Prefix: "abcd1234", TokenHash: "$2b$12$secret"}
	if _, err := json.Marshal(tok); err == nil {
		t.Fatal("expected json.Marshal(ApiTokenRecord) to fail")
	}
	if _, err := json.Marshal(&tok); err == nil {
		t.Fatal("expected json.Marshal(*ApiTokenRecord) to fail")
	}
}

// C8: LocalRole has no documented Rust default (unlike Engine/
// AuditDecision), so a LocalUserView with a zero-value Role must fail to
// marshal rather than emit contract-invalid `"role":""`.
func TestLocalUserViewRefusesToMarshalZeroValueRole(t *testing.T) {
	v := LocalUserView{Username: "alice"} // Role left unset
	if _, err := json.Marshal(v); err == nil {
		t.Fatal("expected json.Marshal(LocalUserView with zero-value Role) to fail")
	}
}

// A LocalUserView with a real Role still marshals normally.
func TestLocalUserViewMarshalsWithRoleSet(t *testing.T) {
	v := LocalUserView{Username: "alice", Role: LocalRoleViewer, CreatedAt: 1}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"role":"viewer"`) {
		t.Fatalf("expected role in JSON: %s", b)
	}
}
