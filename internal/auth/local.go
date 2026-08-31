// Local (IdP-free) authentication (ADR-0011): username/password login and
// personal access tokens backed by the Bifrost store.
//
// Opaque tokens only — no JWT minting. Bifrost stores credentials, it
// never signs them. Tokens are `mob_<8-char prefix>_<32 hex>` random
// strings (RULING: the `mob_` prefix is kept verbatim — wire/UX
// compatibility with existing tokens and the contract; renaming it is a
// someday-decision, not made here), bcrypt-hashed at rest, looked up by
// prefix. Roles are a column on the user row and resolved per request, so
// role changes apply live.
//
// Brute-force posture (mirroring artifact-keeper's local half, ported from
// mobula-auth/src/local.rs):
//   - unknown usernames run a constant-time dummy bcrypt verify so login
//     timing does not enumerate accounts;
//   - 5 consecutive failures lock the account for 5 minutes (the store's
//     LoginLockoutThreshold / LockoutSecs, Task 1);
//   - every failure mode returns the same LocalAuthErrInvalidCredentials
//     error to the caller — lockout/disablement is visible only in the
//     audit trail.
//
// LocalUserStore below is a consumer-defined interface scoped to exactly
// what LocalAuthenticator calls, NOT internal/controller's full Store
// trait (Task 1, ported separately and landing independently of this
// task). This keeps internal/auth compiling and fully unit-testable
// regardless of landing order; internal/controller.Store satisfies this
// interface structurally once it exists, since its local-auth methods are
// ctx-first with the same names/signatures (ported from the same Rust
// store.rs Store trait this file's Rust sibling, local.rs, consumes as
// `Arc<dyn Store>`).
package auth

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/brandonrc/bifrost/internal/core"
)

// bcryptCost is the bcrypt work factor. Pinned to 12 (the Rust reference's
// bcrypt::DEFAULT_COST — NOT golang.org/x/crypto/bcrypt.DefaultCost, which
// is 10) so a token/password hashed by either implementation verifies
// identically and the wire-compatibility ruling on the `mob_` prefix holds
// all the way down to the hash cost.
//
// Reference: mobula-auth/src/local.rs:28 (COST).
const bcryptCost = 12

// dummyHash is a pre-computed bcrypt hash (cost 12) of a dummy password.
// Unknown usernames verify against this so a login attempt always costs
// one bcrypt — the user-exists oracle via response timing stays closed.
// Reused verbatim from the Rust reference: the hash format is a portable
// standard, not implementation-specific.
//
// Reference: mobula-auth/src/local.rs:33 (DUMMY_HASH).
const dummyHash = "$2b$12$dcjUjjUwxXC4Z9wsZzBD3.8Ec1/3r8C.XkqTVfQsgyrNz9sJGUt.K"

// HashPassword hashes password with bcrypt at bcryptCost.
//
// The Rust reference runs this on tokio::task::spawn_blocking because
// tokio's cooperative scheduler would otherwise let one bcrypt hash (a
// deliberately slow, CPU-bound operation) stall every other task sharing
// that worker thread. Go's scheduler doesn't have that failure mode: since
// Go 1.14 goroutines are asynchronously preemptible, and the runtime
// multiplexes goroutines across GOMAXPROCS OS threads (Ms) rather than
// running everything on one — a bare call in a handler goroutine is
// already "blocking-ok" here. No worker-pool/semaphore port is needed (the
// Rust code has no additional semaphore limiting concurrent hashes to
// port either — spawn_blocking's own thread pool is the only mechanism,
// and Go's goroutine scheduler is that mechanism's structural equivalent).
//
// Reference: mobula-auth/src/local.rs:35-42 (hash_password).
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", LocalAuthError{Kind: LocalAuthErrBackend, Message: fmt.Sprintf("bcrypt: %s", err), Source: err}
	}
	return string(hash), nil
}

// VerifyPassword verifies password against a bcrypt hash. A malformed
// stored hash returns false, not an error — a corrupt row must fail
// closed, not 500.
//
// Reference: mobula-auth/src/local.rs:44-54 (verify_password).
func VerifyPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// HashToken hashes a token for storage (same bcrypt posture as
// passwords).
//
// Reference: mobula-auth/src/local.rs:118-121 (hash_token).
func HashToken(token string) (string, error) { return HashPassword(token) }

// VerifyToken verifies a presented token against its stored bcrypt hash.
//
// Reference: mobula-auth/src/local.rs:123-126 (verify_token).
func VerifyToken(storedHash, presented string) bool { return VerifyPassword(presented, storedHash) }

// tokenPrefixLen is the length of a token's lookup prefix.
const tokenPrefixLen = 8

const alphanumericAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomAlphanumeric returns n cryptographically random alphanumeric
// characters, unbiased via rejection sampling (the naive `byte % 62`
// reduction is biased since 256 isn't a multiple of 62 — this mirrors the
// rand crate's Alphanumeric distribution, which rejection-samples for the
// same reason).
func randomAlphanumeric(n int) (string, error) {
	const rejectAt = 256 - (256 % len(alphanumericAlphabet))
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := cryptorand.Read(buf); err != nil {
			return "", err
		}
		if int(buf[0]) >= rejectAt {
			continue
		}
		out[i] = alphanumericAlphabet[int(buf[0])%len(alphanumericAlphabet)]
		i++
	}
	return string(out), nil
}

// MintTokenParts mints the random parts of a token (no hashing — see
// HashToken). Split so tests can exercise the format without paying a
// bcrypt. The scheme is `mob_` + 8 url-safe chars + `_` + 32 hex chars (16
// random bytes). The prefix is the store lookup key — not secret; the
// 128-bit suffix carries the entropy.
//
// Reference: mobula-auth/src/local.rs:68-88 (TOKEN_PREFIX_LEN,
// mint_token_parts).
func MintTokenParts() (prefix, token string, err error) {
	prefix, err = randomAlphanumeric(tokenPrefixLen)
	if err != nil {
		return "", "", err
	}
	raw := make([]byte, 16)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", "", err
	}
	token = "mob_" + prefix + "_" + hex.EncodeToString(raw)
	return prefix, token, nil
}

// RandomPassword returns a random url-safe password of length alphanumeric
// characters — used by the CLI to bootstrap the first local admin
// (ADR-0011).
//
// Reference: mobula-auth/src/local.rs:90-98 (random_password).
func RandomPassword(length int) (string, error) {
	return randomAlphanumeric(length)
}

func isASCIIAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isASCIIHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// TokenPrefix extracts the lookup prefix from a presented token, checking
// the scheme. ok is false for anything not shaped like a Bifrost token.
//
// Reference: mobula-auth/src/local.rs:100-116 (token_prefix).
func TokenPrefix(presented string) (prefix string, ok bool) {
	rest, hasPrefix := strings.CutPrefix(presented, "mob_")
	if !hasPrefix {
		return "", false
	}
	if len(rest) != tokenPrefixLen+1+32 {
		return "", false
	}
	prefix, suffix := rest[:tokenPrefixLen], rest[tokenPrefixLen:]
	for i := 0; i < len(prefix); i++ {
		if !isASCIIAlphanumeric(prefix[i]) {
			return "", false
		}
	}
	hexPart, hasUnderscore := strings.CutPrefix(suffix, "_")
	if !hasUnderscore {
		return "", false
	}
	for i := 0; i < len(hexPart); i++ {
		if !isASCIIHexDigit(hexPart[i]) {
			return "", false
		}
	}
	return prefix, true
}

// nowUnix returns the current unix time in whole seconds. Duplicated here
// (rather than imported from internal/controller, Task 1) deliberately:
// see the package doc comment on LocalUserStore for why this package
// doesn't depend on internal/controller.
func nowUnix() uint64 { return uint64(time.Now().Unix()) }

// MintedToken is a freshly minted opaque token. Token is the plaintext,
// shown to the caller exactly once; only its bcrypt hash (TokenHash) is
// ever stored. MarshalJSON refuses to serialize this type — the
// never-marshal guard used throughout this codebase for secret-bearing
// types — so a handler returning a minted token to the wire must build its
// own explicit one-time response DTO rather than serializing this
// directly, exactly as core.LocalUserRecord/core.ApiTokenRecord require a
// View().
//
// Reference: mobula-auth/src/local.rs:56-66 (MintedToken).
type MintedToken struct {
	// Prefix is the 8-character lookup prefix (the `<prefix>` in
	// `mob_<prefix>_…`).
	Prefix string
	// Token is the full plaintext token.
	Token string
	// TokenHash is the bcrypt hash of Token, for core.ApiTokenRecord.TokenHash.
	TokenHash string
}

// MarshalJSON always fails: Token is the plaintext token, shown to the
// caller exactly once and never stored — it must never leak into a log or
// a response built by accidentally serializing this type wholesale.
func (MintedToken) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: MintedToken must never be marshaled — Token is the one-time plaintext; build an explicit response DTO")
}

// LoginOutcome is what a successful login returns. It embeds MintedToken
// and Identity, both of which already refuse to marshal — LoginOutcome
// inherits that guard through them rather than duplicating it.
//
// Reference: mobula-auth/src/local.rs:128-136 (LoginOutcome).
type LoginOutcome struct {
	// Token is the plaintext token (shown once).
	Token MintedToken
	// ExpiresAt is unix seconds after which the token no longer
	// authenticates.
	ExpiresAt uint64
	Identity  Identity
}

// LocalAuthErrorKind discriminates LocalAuthError failures. Every login
// failure kind maps to the SAME 401 on the wire
// (LocalAuthErrInvalidCredentials, "invalid_credentials") — the distinct
// kinds exist only so the audit trail can tell them apart (ADR-0011: no
// user enumeration, lockout visible only in audit). Callers building a
// wire response must collapse everything except LocalAuthErrTTLTooLong
// (a distinct, non-enumerating 400) to that one message; see
// local_test.go for the parity assertion this depends on.
type LocalAuthErrorKind int

const (
	LocalAuthErrInvalidCredentials LocalAuthErrorKind = iota
	LocalAuthErrLocked
	LocalAuthErrDisabled
	LocalAuthErrUnknownUser
	LocalAuthErrTTLTooLong
	LocalAuthErrBackend
)

// LocalAuthError is local auth's error type: value-typed with an Unwrap
// chain to the underlying cause, mirroring Rust's thiserror source field
// on the Backend variant.
//
// Reference: mobula-auth/src/local.rs:141-155 (LocalAuthError).
type LocalAuthError struct {
	Kind LocalAuthErrorKind
	// Message carries the detail text for Backend.
	Message string
	// Source is the wrapped underlying error, if any (for errors.Unwrap).
	Source error
}

func (e LocalAuthError) Error() string {
	switch e.Kind {
	case LocalAuthErrInvalidCredentials:
		return "invalid credentials"
	case LocalAuthErrLocked:
		return "account is locked"
	case LocalAuthErrDisabled:
		return "account is disabled"
	case LocalAuthErrUnknownUser:
		return "no such user"
	case LocalAuthErrTTLTooLong:
		return "token ttl exceeds the configured maximum"
	case LocalAuthErrBackend:
		return fmt.Sprintf("backend error: %s", e.Message)
	}
	return "local auth error"
}

// Unwrap exposes the wrapped Source error, mirroring Rust's thiserror
// #[source] field, so errors.Is/errors.As can reach it.
func (e LocalAuthError) Unwrap() error { return e.Source }

// LocalUserStore is the persistence surface LocalAuthenticator needs for
// local username/password + PAT authentication (ADR-0011). See the package
// doc comment for why this is a small consumer-defined interface rather
// than a direct dependency on internal/controller.Store.
//
// Reference: the subset of mobula-controller::Store that local.rs's
// LocalAuthenticator calls (get_local_user, record_login_failure/success,
// create_api_token, get_api_token_by_prefix, touch_api_token).
type LocalUserStore interface {
	GetLocalUser(ctx context.Context, username string) (*core.LocalUserRecord, error)
	RecordLoginFailure(ctx context.Context, username string) error
	RecordLoginSuccess(ctx context.Context, username string) error
	CreateApiToken(ctx context.Context, record core.ApiTokenRecord) error
	GetApiTokenByPrefix(ctx context.Context, prefix string) (*core.ApiTokenRecord, error)
	TouchApiToken(ctx context.Context, prefix string, now uint64) error
}

// localRoleToRole maps core.LocalRole to the auth package's Role. Every
// known LocalRole variant is enumerated explicitly (exhaustive lint,
// default-signifies-exhaustive); the trailing return handles a value
// outside the closed set the same defensive way
// Role.AsStr/LocalRole.AsStr do — LocalRole's own UnmarshalJSON already
// rejects anything else at ingress, so this is unreachable in practice.
func localRoleToRole(r core.LocalRole) Role {
	switch r {
	case core.LocalRoleViewer:
		return RoleViewer
	case core.LocalRoleDeveloper:
		return RoleDeveloper
	case core.LocalRoleOperator:
		return RoleOperator
	case core.LocalRoleAdmin:
		return RoleAdmin
	case core.LocalRoleAuditor:
		return RoleAuditor
	}
	return RoleViewer
}

// identityOf projects a LocalUserRecord to an Identity. Local auth has no
// IdP groups, so no group->project-role automation (#103); scoped grants
// for local users come only from explicit role_assignments (handled at
// the API layer over the Store, same as OIDC identities).
//
// Reference: mobula-auth/src/local.rs:173-185 (identity_of).
func identityOf(user *core.LocalUserRecord) Identity {
	username := user.Username
	return Identity{
		Subject:      user.Username,
		Username:     &username,
		Email:        user.Email,
		Roles:        []Role{localRoleToRole(user.Role)},
		ProjectRoles: nil,
	}
}

// LocalAuthenticator is local username/password + PAT authentication
// against the store (ADR-0011).
//
// Reference: mobula-auth/src/local.rs:189-333 (LocalAuthenticator).
type LocalAuthenticator struct {
	store LocalUserStore
	// loginTTLSecs is the lifetime of a token issued by Login.
	loginTTLSecs uint64
	// tokenMaxDays is the maximum lifetime of a user-minted PAT, in days.
	tokenMaxDays uint64
}

// NewLocalAuthenticator builds a LocalAuthenticator over store.
func NewLocalAuthenticator(store LocalUserStore, loginTTLSecs, tokenMaxDays uint64) *LocalAuthenticator {
	return &LocalAuthenticator{store: store, loginTTLSecs: loginTTLSecs, tokenMaxDays: tokenMaxDays}
}

// Store returns the backing LocalUserStore.
func (a *LocalAuthenticator) Store() LocalUserStore { return a.store }

// Login authenticates a username/password. Enforces disabled -> locked ->
// password, in that order; unknown users run the dummy-hash verify so
// every failure path costs one bcrypt. On success the lockout counters
// clear and a login token (TTL loginTTLSecs) is stored.
//
// Reference: mobula-auth/src/local.rs:210-252 (LocalAuthenticator::login).
func (a *LocalAuthenticator) Login(ctx context.Context, username, password string) (*LoginOutcome, error) {
	user, err := a.store.GetLocalUser(ctx, username)
	if err != nil {
		return nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}
	if user == nil {
		// Constant-time dummy verify: unknown users cost the same bcrypt
		// as known ones (no user-exists timing oracle).
		_ = VerifyPassword(password, dummyHash)
		return nil, LocalAuthError{Kind: LocalAuthErrInvalidCredentials}
	}
	if user.Disabled {
		// Still pay the bcrypt — disabled users are indistinguishable
		// from wrong passwords on the wire and in timing.
		_ = VerifyPassword(password, user.PasswordHash)
		return nil, LocalAuthError{Kind: LocalAuthErrDisabled}
	}
	now := nowUnix()
	if user.LockedUntil != nil && *user.LockedUntil > now {
		// Refuse without verifying: the lock short-circuits, and no
		// failure is recorded while locked (the store's counter reset
		// when the lock tripped).
		return nil, LocalAuthError{Kind: LocalAuthErrLocked}
	}
	if !VerifyPassword(password, user.PasswordHash) {
		if err := a.store.RecordLoginFailure(ctx, username); err != nil {
			return nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
		}
		return nil, LocalAuthError{Kind: LocalAuthErrInvalidCredentials}
	}
	if err := a.store.RecordLoginSuccess(ctx, username); err != nil {
		return nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}

	expiresAt := now + a.loginTTLSecs
	token, err := a.storeToken(ctx, username, "login", expiresAt)
	if err != nil {
		return nil, err
	}
	return &LoginOutcome{Token: *token, ExpiresAt: expiresAt, Identity: identityOf(user)}, nil
}

// AuthenticateToken authenticates a presented bearer as an opaque token.
// nil for anything that doesn't fully check out (bad scheme, unknown
// prefix, hash mismatch, revoked, expired, disabled/deleted user, or any
// backend error) — every failure collapses to the same "not authenticated"
// outcome, mirroring the Rust reference's `Option<Identity>` return (every
// fallible step there is `.ok()??`, so a store error is indistinguishable
// from an absent token; this is a deliberate fail-closed hot-path
// decision, not an oversight). Role is read from the user row per request
// — role changes apply live.
//
// Reference: mobula-auth/src/local.rs:254-275 (authenticate_token).
func (a *LocalAuthenticator) AuthenticateToken(ctx context.Context, presented string) *Identity {
	prefix, ok := TokenPrefix(presented)
	if !ok {
		return nil
	}
	record, err := a.store.GetApiTokenByPrefix(ctx, prefix)
	if err != nil || record == nil {
		return nil
	}
	now := nowUnix()
	if record.Revoked || record.ExpiresAt <= now {
		return nil
	}
	if !VerifyToken(record.TokenHash, presented) {
		return nil
	}
	user, err := a.store.GetLocalUser(ctx, record.Username)
	if err != nil || user == nil {
		return nil
	}
	if user.Disabled {
		return nil
	}
	// Best-effort last-used stamp; never fails the authentication.
	_ = a.store.TouchApiToken(ctx, prefix, now)
	id := identityOf(user)
	return &id
}

// IssueToken issues a personal access token for username, capped at
// tokenMaxDays. Returns the minted token (plaintext shown once) and the
// stored record.
//
// Reference: mobula-auth/src/local.rs:279-306 (issue_token).
func (a *LocalAuthenticator) IssueToken(ctx context.Context, username, label string, ttlDays uint64) (*MintedToken, *core.ApiTokenRecord, error) {
	if ttlDays == 0 || ttlDays > a.tokenMaxDays {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrTTLTooLong}
	}
	// Issuing a token for a nonexistent or disabled user is a store-level
	// error, not a credential failure — the caller is already authed.
	user, err := a.store.GetLocalUser(ctx, username)
	if err != nil {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}
	if user == nil {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrUnknownUser}
	}
	if user.Disabled {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrDisabled}
	}

	expiresAt := nowUnix() + ttlDays*86_400
	minted, err := a.storeToken(ctx, username, label, expiresAt)
	if err != nil {
		return nil, nil, err
	}
	record, err := a.store.GetApiTokenByPrefix(ctx, minted.Prefix)
	if err != nil {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}
	if record == nil {
		return nil, nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: "token vanished after create"}
	}
	return minted, record, nil
}

// storeToken mints, hashes, and persists a token, returning the minted
// (plaintext-bearing) value.
//
// Reference: mobula-auth/src/local.rs:308-332 (store_token).
func (a *LocalAuthenticator) storeToken(ctx context.Context, username, label string, expiresAt uint64) (*MintedToken, error) {
	prefix, plaintext, err := MintTokenParts()
	if err != nil {
		return nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}
	tokenHash, err := HashToken(plaintext)
	if err != nil {
		return nil, err
	}
	record := core.ApiTokenRecord{
		Prefix:    prefix,
		TokenHash: tokenHash,
		Username:  username,
		Label:     label,
		CreatedAt: nowUnix(),
		ExpiresAt: expiresAt,
		Revoked:   false,
	}
	if err := a.store.CreateApiToken(ctx, record); err != nil {
		return nil, LocalAuthError{Kind: LocalAuthErrBackend, Message: err.Error(), Source: err}
	}
	return &MintedToken{Prefix: prefix, Token: plaintext, TokenHash: tokenHash}, nil
}
