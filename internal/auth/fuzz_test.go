package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
)

// Fuzz entry points for the auth parsing/validation boundary (security
// audit): the hand-rolled JWK→RSA conversion, claim sanitization, and the
// full Validate token pipeline against a static in-memory key (no network:
// refreshCooldown is set so refreshJWKS always returns early).

func fuzzRSAKey(f *testing.F) *rsa.PrivateKey {
	f.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatalf("generate rsa key: %v", err)
	}
	return priv
}

// FuzzRSAPublicKeyFromJWK feeds arbitrary base64url strings (and arbitrary
// byte strings) into the JWK n/e conversion. Invariants: no panic; an error
// is always a clean rejection; a success always yields a positive exponent
// in uint32 range and a non-nil modulus.
func FuzzRSAPublicKeyFromJWK(f *testing.F) {
	priv := fuzzRSAKey(f)
	validN := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	validE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes())
	for _, s := range [][2]string{
		{validN, validE},
		{validN, "AQAB"}, // 65537
		{validN, "Aw"},   // 3
		{validN, "AA"},   // e = 0: out of range
		{validN, ""},
		{"", validE},
		{"", ""},
		{"!!!not-base64!!!", validE},
		{validN, "!!!!"},
		{validN + "A", validE},
		{validN, base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 33).Bytes())}, // e = 2^33
		{base64.RawURLEncoding.EncodeToString([]byte{0}), validE},                                   // n = 0
		{base64.RawURLEncoding.EncodeToString(make([]byte, 4096)), validE},                          // oversized modulus
		{"AQAB", "AQAB"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, nB64, eB64 string) {
		key, err := rsaPublicKeyFromJWK(nB64, eB64)
		if err != nil {
			return
		}
		if key.N == nil {
			t.Fatal("nil modulus on success")
		}
		if key.E <= 0 || int64(key.E) > int64(^uint32(0)) {
			t.Fatalf("exponent %d out of range on success", key.E)
		}
	})
}

// FuzzSanitizeClaim checks the audit-log forgery guard (#34): the output
// must contain no control characters, must preserve rune count (replacement
// is 1:1), and must be idempotent.
func FuzzSanitizeClaim(f *testing.F) {
	for _, s := range []string{
		"user-123",
		"user\nFORGED-LOG-LINE",
		"a\tb\rc",
		"null\x00byte",
		"unicode é 用户",
		"\x1b[31mansi-escape",
		"",
		"\n\r\t\x00\x1b",
		"overlong \xff\xfe bytes",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := sanitizeClaim(s)
		if utf8.RuneCountInString(out) != utf8.RuneCountInString(s) {
			t.Fatalf("rune count changed: %q -> %q", s, out)
		}
		for _, r := range out {
			if unicode.IsControl(r) {
				t.Fatalf("control rune %U survived sanitization in %q", r, out)
			}
		}
		if twice := sanitizeClaim(out); twice != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, twice)
		}
	})
}

// FuzzIdentityFromClaims exercises the claim-mapping/sanitization plumbing
// directly with JSON-decoded claim maps (the exact shapes a parser produces:
// strings, float64s, bools, nested []interface{}/map[string]interface{}).
// Invariants: no panic; Subject and Username never carry control runes.
func FuzzIdentityFromClaims(f *testing.F) {
	v := &Validator{config: AuthConfig{
		Issuer:      "https://issuer.example.com",
		Audience:    "bifrost",
		GroupsClaim: "groups",
		Roles:       RoleMappings{Developer: []string{"/ml-eng"}},
	}}
	for _, s := range []string{
		`{"sub":"user-123","groups":["/ml-eng"],"email":"u@example.com"}`,
		`{"sub":"user\nforged","preferred_username":"a\tb"}`,
		`{"sub":123}`,
		`{"sub":""}`,
		`{"groups":"/ml-eng /sre"}`,
		`{"groups":[1,true,"/ok",null]}`,
		`{"groups":{"a":1}}`,
		`{"sub":null}`,
		`{}`,
		`{"sub":"\x00\x1b[2J"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}
		var claims jwt.MapClaims
		if err := json.Unmarshal(data, &claims); err != nil {
			return
		}
		id := v.identityFromClaims(claims)
		for _, r := range id.Subject {
			if unicode.IsControl(r) {
				t.Fatalf("control rune %U in Subject %q", r, id.Subject)
			}
		}
		if id.Username != nil {
			for _, r := range *id.Username {
				if unicode.IsControl(r) {
					t.Fatalf("control rune %U in Username %q", r, *id.Username)
				}
			}
		}
	})
}

// FuzzValidateToken runs the full Validate pipeline (parse, kid lookup,
// RS256 verify, required-claim checks, identity mapping) against a static
// in-memory key. jwksURI is never hit: refreshCooldown is an hour and
// lastRefresh is now, so an unknown kid resolves to AuthErrUnknownKeyID
// without a network fetch.
func FuzzValidateToken(f *testing.F) {
	priv := fuzzRSAKey(f)
	v := &Validator{
		config: AuthConfig{
			Issuer:      "https://issuer.example.com",
			Audience:    "bifrost",
			GroupsClaim: "groups",
			Roles:       RoleMappings{Developer: []string{"/ml-eng"}},
		},
		client:          IdpClient(),
		jwksURI:         "https://issuer.example.com/jwks",
		keys:            map[string]*rsa.PublicKey{"fuzz-key": &priv.PublicKey},
		lastRefresh:     time.Now(),
		refreshCooldown: time.Hour,
	}
	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "fuzz-key"
		s, err := tok.SignedString(priv)
		if err != nil {
			f.Fatalf("sign seed: %v", err)
		}
		return s
	}
	now := time.Now()
	valid := jwt.MapClaims{
		"sub": "user-123", "iss": "https://issuer.example.com", "aud": "bifrost",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"groups": []string{"/ml-eng"},
	}
	// A second key produces well-formed tokens with a bad signature.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatalf("generate rsa key: %v", err)
	}
	tokOther := jwt.NewWithClaims(jwt.SigningMethodRS256, valid)
	tokOther.Header["kid"] = "fuzz-key"
	wrongKey, err := tokOther.SignedString(other)
	if err != nil {
		f.Fatalf("sign seed: %v", err)
	}

	seeds := []string{
		sign(valid),
		sign(jwt.MapClaims{
			"sub": "user\nforged-line", "preferred_username": "a\tb",
			"iss": "https://issuer.example.com", "aud": "bifrost",
			"exp": now.Add(5 * time.Minute).Unix(), "groups": "/ml-eng /sre",
		}),
		sign(jwt.MapClaims{ // expired
			"sub": "u", "iss": "https://issuer.example.com", "aud": "bifrost",
			"exp": now.Add(-5 * time.Minute).Unix(),
		}),
		sign(jwt.MapClaims{ // wrong audience
			"sub": "u", "iss": "https://issuer.example.com", "aud": "not-bifrost",
			"exp": now.Add(5 * time.Minute).Unix(),
		}),
		sign(jwt.MapClaims{ // no sub
			"iss": "https://issuer.example.com", "aud": "bifrost",
			"exp": now.Add(5 * time.Minute).Unix(),
		}),
		wrongKey,
		"not-a-jwt",
		"",
		"a.b.c",
		"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.sig",
		strings.Repeat("a", 4096),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, tokenString string) {
		if len(tokenString) > 1<<16 {
			tokenString = tokenString[:1<<16]
		}
		id, err := v.Validate(context.Background(), tokenString)
		if err != nil {
			var ae AuthError
			if !errors.As(err, &ae) {
				t.Fatalf("Validate returned non-AuthError %T: %v", err, err)
			}
			return
		}
		if id.Subject == "" {
			t.Fatal("accepted a token with an empty subject")
		}
		for _, r := range id.Subject {
			if unicode.IsControl(r) {
				t.Fatalf("control rune %U in accepted Subject %q", r, id.Subject)
			}
		}
	})
}
