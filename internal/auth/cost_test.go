package auth

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/brandonrc/bifrost/internal/core"
)

// --- Cost-knob tests (spec §3: the L2 requirement-test lane needs a
// cheaper bcrypt cost than production; these prove the knob is exposed
// safely and production behavior is untouched.) ---

// TestNewLocalAuthenticatorStillHashesAtProductionCost proves the
// zero-config constructor (NewLocalAuthenticator, used in production) still
// issues tokens hashed at the pinned production cost (12), even after the
// cost knob is introduced.
func TestNewLocalAuthenticatorStillHashesAtProductionCost(t *testing.T) {
	store := newFakeLocalStore()
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	store.createLocalUser("alice", nil, hash, core.LocalRoleViewer)
	a := NewLocalAuthenticator(store, 3600, 90)

	outcome, err := a.Login(context.Background(), "alice", "correct horse")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	rec, err := store.GetApiTokenByPrefix(context.Background(), outcome.Token.Prefix)
	if err != nil || rec == nil {
		t.Fatalf("issued token record not found: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(rec.TokenHash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != bcryptCost {
		t.Fatalf("issued token hash cost = %d, want %d (production pin)", cost, bcryptCost)
	}
}

// TestNewLocalAuthenticatorWithCostRejectsOutOfRangeCost proves the
// constructor validates its cost argument against bcrypt's supported range
// instead of silently accepting (and later panicking inside bcrypt on) a
// bogus value.
func TestNewLocalAuthenticatorWithCostRejectsOutOfRangeCost(t *testing.T) {
	store := newFakeLocalStore()
	if _, err := NewLocalAuthenticatorWithCost(store, 3600, 90, bcrypt.MinCost-1); err == nil {
		t.Fatal("expected error for cost below bcrypt.MinCost")
	}
	if _, err := NewLocalAuthenticatorWithCost(store, 3600, 90, bcrypt.MaxCost+1); err == nil {
		t.Fatal("expected error for cost above bcrypt.MaxCost")
	}
}

// TestHashPasswordWithCostProducesAVerifiableHashAtMinCost proves the
// cheap-cost path (what the L2 requirement lane uses to stay under its
// budget) produces a hash CompareHashAndPassword genuinely accepts, not
// just a hash tagged with the requested cost.
func TestHashPasswordWithCostProducesAVerifiableHashAtMinCost(t *testing.T) {
	hash, err := HashPasswordWithCost("correct horse", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost: %v", err)
	}
	if cost != bcrypt.MinCost {
		t.Fatalf("hash cost = %d, want %d", cost, bcrypt.MinCost)
	}
	if !VerifyPassword("correct horse", hash) {
		t.Fatal("expected correct password to verify against a MinCost hash")
	}
	if VerifyPassword("wrong", hash) {
		t.Fatal("expected wrong password to fail against a MinCost hash")
	}
}
