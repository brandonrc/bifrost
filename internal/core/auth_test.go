package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// Ported from mobula-core/src/auth.rs #[cfg(test)] mod tests.

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
