package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// fakeJWT builds a syntactically-valid-enough JWT (header.payload.signature,
// none of it verified) carrying only the given exp claim, for jwtExp tests.
func fakeJWT(t *testing.T, exp *uint64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := map[string]any{}
	if exp != nil {
		claims["exp"] = *exp
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return header + "." + payload + ".sig"
}

func TestJwtExp(t *testing.T) {
	exp := uint64(1234567890)
	if got, ok := jwtExp(fakeJWT(t, &exp)); !ok || got != exp {
		t.Fatalf("jwtExp = (%d, %v), want (%d, true)", got, ok, exp)
	}
	if _, ok := jwtExp(fakeJWT(t, nil)); ok {
		t.Fatal("jwtExp should be false when the exp claim is absent")
	}
	if _, ok := jwtExp("bfr_notajwt"); ok {
		t.Fatal("jwtExp should be false for an opaque (non-JWT) token")
	}
	if _, ok := jwtExp("only.two"); ok {
		t.Fatal("jwtExp should be false for a token with the wrong number of segments")
	}
}

func TestStoredTokenAction(t *testing.T) {
	now := uint64(1_000_000)
	future := now + 3600
	past := now - 3600
	refresh := "r"

	// Opaque local-auth token (no exp claim decodable) — always valid
	// client-side; the server enforces its lifetime.
	if got := storedTokenAction(Credentials{AccessToken: "bfr_abc123"}, now); got != tokenActionValid {
		t.Errorf("opaque token: got %v, want valid", got)
	}
	// Unexpired JWT — valid.
	if got := storedTokenAction(Credentials{AccessToken: fakeJWT(t, &future)}, now); got != tokenActionValid {
		t.Errorf("unexpired JWT: got %v, want valid", got)
	}
	// Expired JWT with a refresh token — refresh.
	if got := storedTokenAction(Credentials{AccessToken: fakeJWT(t, &past), RefreshToken: &refresh}, now); got != tokenActionRefresh {
		t.Errorf("expired JWT with refresh: got %v, want refresh", got)
	}
	// Expired JWT with no refresh token — expired, no refresh.
	if got := storedTokenAction(Credentials{AccessToken: fakeJWT(t, &past)}, now); got != tokenActionExpiredNoRefresh {
		t.Errorf("expired JWT without refresh: got %v, want expiredNoRefresh", got)
	}
}
