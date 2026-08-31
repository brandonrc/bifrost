package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Negative tests for the token-verification boundary: every one of these
// tokens is well-formed and carries claims that would pass validation, so
// the ONLY thing rejecting them is the property under test. They exist to
// keep a future refactor from silently loosening the boundary — a change to
// WithValidMethods, to the kid handling, or to the order of the parser
// options would leave the positive tests green and break every case here.
//
// Helpers (newTestIdp, discoverT, authErrKind, signRaw, signRawKid) come
// from validator_test.go.

// attackClaims is a claim set that is valid in every respect — correct
// issuer, correct audience, present subject, comfortably unexpired — so a
// rejection can only come from the signature or header property being
// tested.
func attackClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "attacker",
		"iss": issuer,
		"aud": "bifrost",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
}

// signAs signs attackClaims with an arbitrary method, key, and kid header,
// bypassing the IdP helpers so a test can forge any header it likes.
func signAs(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid == "" {
		delete(tok.Header, "kid")
	} else {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// The classic algorithm-confusion attack: the attacker takes the RSA public
// key they can freely read from the JWKS, presents it as an HMAC shared
// secret, and signs their own token with it. A validator that picks the key
// by kid without confining the algorithm would verify this successfully and
// hand the attacker any identity they asked for.
//
// Confinement must happen BEFORE the key lookup: golang-jwt checks
// WithValidMethods ahead of calling the Keyfunc, so the RSA public key is
// never even handed to an HMAC verifier.
func TestAlgorithmConfusionHS256IsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	// The public modulus is public data — anyone can fetch it from /jwks.
	secret := idp.priv.N.Bytes()
	tok := signAs(t, jwt.SigningMethodHS256, secret, idp.kid, attackClaims(idp.issuer))

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("HS256 algorithm-confusion token was accepted")
	} else if authErrKind(t, err) != AuthErrInvalidToken {
		t.Fatalf("expected AuthErrInvalidToken, got %v", err)
	}
}

// An unsigned token must never be accepted, however complete its claims.
func TestAlgNoneIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	tok := signAs(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType,
		idp.kid, attackClaims(idp.issuer))

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("alg=none token was accepted")
	} else if authErrKind(t, err) != AuthErrInvalidToken {
		t.Fatalf("expected AuthErrInvalidToken, got %v", err)
	}
}

// Confinement is to RS256 exactly, not to "the RSA family": a genuinely
// signed RS512 token under the IdP's own key is still refused, because the
// Rust reference pins Algorithm::RS256 and any widening here would be a
// silent divergence.
func TestNonRS256AlgorithmIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	for _, method := range []jwt.SigningMethod{
		jwt.SigningMethodRS512,
		jwt.SigningMethodPS256,
	} {
		t.Run(method.Alg(), func(t *testing.T) {
			tok := signAs(t, method, idp.priv, idp.kid, attackClaims(idp.issuer))
			if _, err := v.Validate(context.Background(), tok); err == nil {
				t.Fatalf("%s token was accepted under RS256-only confinement", method.Alg())
			}
		})
	}
}

// A token signed by a key the IdP never published, wearing a kid that IS in
// the JWKS. The kid selects the honest public key, so this fails at the
// signature check — the one property that makes the whole scheme work.
func TestForeignKeySignatureIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}
	tok := signAs(t, jwt.SigningMethodRS256, attacker, idp.kid, attackClaims(idp.issuer))

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("token signed by a foreign key was accepted")
	} else if authErrKind(t, err) != AuthErrInvalidToken {
		t.Fatalf("expected AuthErrInvalidToken, got %v", err)
	}
}

// No kid, or an empty kid, is refused outright rather than being carried
// into a key lookup — there is no "default key" fallback to select.
func TestMissingOrEmptyKidIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	t.Run("absent", func(t *testing.T) {
		tok := signAs(t, jwt.SigningMethodRS256, idp.priv, "", attackClaims(idp.issuer))
		_, err := v.Validate(context.Background(), tok)
		if err == nil {
			t.Fatal("token with no kid header was accepted")
		}
		if authErrKind(t, err) != AuthErrInvalidToken {
			t.Fatalf("expected AuthErrInvalidToken, got %v", err)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, attackClaims(idp.issuer))
		tok.Header["kid"] = ""
		s, err := tok.SignedString(idp.priv)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		_, err = v.Validate(context.Background(), s)
		if err == nil {
			t.Fatal("token with an empty kid was accepted")
		}
		if authErrKind(t, err) != AuthErrInvalidToken {
			t.Fatalf("expected AuthErrInvalidToken, got %v", err)
		}
	})
}

// An unknown kid must not be a free pass: after the (cooldown-gated)
// refresh fails to produce the key, the token is refused with the distinct
// UnknownKeyID kind.
func TestUnknownKidIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	tok := signAs(t, jwt.SigningMethodRS256, idp.priv, "no-such-key", attackClaims(idp.issuer))
	_, err := v.Validate(context.Background(), tok)
	if err == nil {
		t.Fatal("token with an unknown kid was accepted")
	}
	if authErrKind(t, err) != AuthErrUnknownKeyID {
		t.Fatalf("expected AuthErrUnknownKeyID, got %v", err)
	}
}

// A token minted by a different issuer, but signed by a key this validator
// trusts (the confused-deputy case where one IdP serves several issuers).
// The iss claim must be checked, not merely present.
func TestForeignIssuerTokenIsRejected(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	for _, iss := range []string{
		"https://evil.example.com",
		idp.issuer + ".evil.example.com", // prefix-confusion
		idp.issuer + "/../other-realm",   // path traversal in the issuer
	} {
		t.Run(iss, func(t *testing.T) {
			tok := idp.signRaw(t, attackClaims(iss))
			if _, err := v.Validate(context.Background(), tok); err == nil {
				t.Fatalf("token from issuer %q was accepted", iss)
			}
		})
	}
}

// The JWKS refresh cooldown must hold under concurrency, not just when
// called sequentially: a flood of unknown-kid tokens arriving at once must
// still produce at most one fetch. Run with -race — this is also the only
// test that exercises the keysMu/refreshMu pair from multiple goroutines.
func TestConcurrentUnknownKidCausesAtMostOneFetch(t *testing.T) {
	idp := newTestIdp(t)
	v := discoverT(t, idp, RoleMappings{Viewer: []string{"*"}})

	// Shrink the window and backdate the last refresh so exactly one
	// refresh is permitted for the burst below (same-package access to the
	// unexported fields keeps the test fast; production keeps the real 30s).
	v.refreshCooldown = 2 * time.Second
	v.lastRefresh = time.Now().Add(-v.refreshCooldown)
	before := idp.hits.Load()

	tok := signAs(t, jwt.SigningMethodRS256, idp.priv, "no-such-key", attackClaims(idp.issuer))

	const callers = 64
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = v.Validate(context.Background(), tok)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: unknown-kid token was accepted", i)
		}
	}
	if got := idp.hits.Load() - before; got > 1 {
		t.Fatalf("cooldown breached: %d concurrent unknown-kid validations caused %d JWKS fetches, want at most 1",
			callers, got)
	}
}
