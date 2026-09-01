package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// --- fakeLocalStore: a minimal in-memory LocalUserStore for tests. ---
//
// The canonical Store implementation (internal/controller, Task 1) lands
// independently of this task; this fake exists purely so local_test.go
// can port every mobula-auth/src/local.rs test without a hard dependency
// on that package's landing order. Its lockout arithmetic mirrors the
// values Task 1's Store uses (5 failures / 300s lock — see
// mobula-controller/src/store.rs:339-352's next_login_failure_state),
// duplicated here only as test scaffolding, not a second production
// implementation.
type fakeLocalStore struct {
	mu     sync.Mutex
	users  map[string]core.LocalUserRecord
	tokens map[string]core.ApiTokenRecord
}

const (
	fakeLockoutThreshold uint32 = 5
	fakeLockoutSecs      uint64 = 300
)

func newFakeLocalStore() *fakeLocalStore {
	return &fakeLocalStore{
		users:  map[string]core.LocalUserRecord{},
		tokens: map[string]core.ApiTokenRecord{},
	}
}

func (s *fakeLocalStore) createLocalUser(username string, email *string, passwordHash string, role core.LocalRole) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[username] = core.LocalUserRecord{
		Username:     username,
		Email:        email,
		PasswordHash: passwordHash,
		Role:         role,
		CreatedAt:    nowUnix(),
	}
}

func (s *fakeLocalStore) setDisabled(username string, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[username]
	u.Disabled = disabled
	s.users[username] = u
}

func (s *fakeLocalStore) setRole(username string, role core.LocalRole) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[username]
	u.Role = role
	s.users[username] = u
}

func (s *fakeLocalStore) revokeToken(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tokens[prefix]
	t.Revoked = true
	s.tokens[prefix] = t
}

func (s *fakeLocalStore) GetLocalUser(_ context.Context, username string) (*core.LocalUserRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	if !ok {
		return nil, nil
	}
	cp := u
	return &cp, nil
}

func (s *fakeLocalStore) RecordLoginFailure(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	if !ok {
		return nil
	}
	failed := u.FailedLogins + 1
	if failed >= fakeLockoutThreshold {
		locked := nowUnix() + fakeLockoutSecs
		u.FailedLogins = 0
		u.LockedUntil = &locked
	} else {
		u.FailedLogins = failed
	}
	s.users[username] = u
	return nil
}

func (s *fakeLocalStore) RecordLoginSuccess(_ context.Context, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[username]
	if !ok {
		return nil
	}
	u.FailedLogins = 0
	u.LockedUntil = nil
	s.users[username] = u
	return nil
}

func (s *fakeLocalStore) CreateApiToken(_ context.Context, record core.ApiTokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[record.Prefix] = record
	return nil
}

func (s *fakeLocalStore) GetApiTokenByPrefix(_ context.Context, prefix string) (*core.ApiTokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[prefix]
	if !ok {
		return nil, nil
	}
	cp := t
	return &cp, nil
}

func (s *fakeLocalStore) TouchApiToken(_ context.Context, prefix string, now uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[prefix]
	if !ok {
		return nil
	}
	t.LastUsedAt = &now
	s.tokens[prefix] = t
	return nil
}

var _ LocalUserStore = (*fakeLocalStore)(nil)

func authenticatorWithUser(t *testing.T, username, password string, role core.LocalRole) (*LocalAuthenticator, *fakeLocalStore) {
	t.Helper()
	store := newFakeLocalStore()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	store.createLocalUser(username, nil, hash, role)
	return NewLocalAuthenticator(store, 3600, 90), store
}

// --- Ported from mobula-auth/src/local.rs:355-364 ---

func TestPasswordHashRoundTripAndMalformedHashFailsClosed(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash[:2] != "$2" {
		t.Fatalf("expected bcrypt hash, got %q", hash)
	}
	if !VerifyPassword("correct horse", hash) {
		t.Fatal("expected correct password to verify")
	}
	if VerifyPassword("wrong", hash) {
		t.Fatal("expected wrong password to fail")
	}
	// A corrupt stored hash verifies false rather than erroring.
	if VerifyPassword("correct horse", "not-a-hash") {
		t.Fatal("expected malformed hash to fail closed")
	}
}

// --- Ported from mobula-auth/src/local.rs:366-394 ---

func TestMintedTokenFormatAndPrefixParsing(t *testing.T) {
	prefix, token, err := MintTokenParts()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(prefix) != 8 {
		t.Fatalf("expected 8-char prefix, got %q", prefix)
	}
	for i := 0; i < len(prefix); i++ {
		if !isASCIIAlphanumeric(prefix[i]) {
			t.Fatalf("prefix %q not alphanumeric", prefix)
		}
	}
	// <scheme><8>_<32 hex>, read from the constant rather than respelled, so a
	// future scheme change cannot leave this test asserting the old one.
	if token[:len(TokenScheme)+8+1] != TokenScheme+prefix+"_" {
		t.Fatalf("unexpected token shape: %q", token)
	}
	if len(token) != 4+8+1+32 {
		t.Fatalf("unexpected token length: %d", len(token))
	}
	gotPrefix, ok := TokenPrefix(token)
	if !ok || gotPrefix != prefix {
		t.Fatalf("TokenPrefix(%q) = (%q, %v), want (%q, true)", token, gotPrefix, ok, prefix)
	}

	// Rejects non-scheme shapes.
	if _, ok := TokenPrefix("bfr_short_hex"); ok {
		t.Fatal("expected rejection of malformed token")
	}
	if _, ok := TokenPrefix("nope_abcd1234_0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("expected rejection of wrong scheme")
	}
	if _, ok := TokenPrefix("bfr_abcd1234_0123456789abcdef0123456789abcdeg"); ok {
		t.Fatal("expected rejection of non-hex suffix")
	}
	if _, ok := TokenPrefix("bfr_abcd!234_0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("expected rejection of non-alphanumeric prefix")
	}

	// The retired mobula scheme. Renaming the prefix was a hard cutover with no
	// compatibility window: an old token must not parse, so it can never reach
	// the store lookup, let alone the bcrypt compare.
	if _, ok := TokenPrefix("mob_abcd1234_0123456789abcdef0123456789abcdef"); ok {
		t.Fatal("a retired mob_ token must not be accepted")
	}

	// And the scheme itself is the new one — asserted directly so a revert of
	// the constant fails here with an explicit message rather than only as a
	// shape mismatch above.
	if TokenScheme != "bfr_" {
		t.Fatalf("TokenScheme = %q, want %q", TokenScheme, "bfr_")
	}

	// Two random mints never collide.
	_, other, err := MintTokenParts()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if token == other {
		t.Fatal("expected distinct tokens")
	}
}

// --- Ported from mobula-auth/src/local.rs:396-408 ---

func TestUnknownUserTakesTheVerifyPath(t *testing.T) {
	auth, _ := authenticatorWithUser(t, "alice", "pw", core.LocalRoleAdmin)
	// Not a timing test: the contract is that unknown users get the same
	// error (and the same bcrypt cost, via the dummy hash) as a wrong
	// password on a real account.
	_, unknown := auth.Login(context.Background(), "ghost", "pw")
	var unknownErr LocalAuthError
	if !errors.As(unknown, &unknownErr) || unknownErr.Kind != LocalAuthErrInvalidCredentials {
		t.Fatalf("expected invalid credentials, got %v", unknown)
	}
	_, wrongPw := auth.Login(context.Background(), "alice", "nope")
	var wrongErr LocalAuthError
	if !errors.As(wrongPw, &wrongErr) || wrongErr.Kind != LocalAuthErrInvalidCredentials {
		t.Fatalf("expected invalid credentials, got %v", wrongPw)
	}
	if unknown.Error() != wrongPw.Error() {
		t.Fatalf("expected identical wire error, got %q vs %q", unknown.Error(), wrongPw.Error())
	}
}

// --- Ported from mobula-auth/src/local.rs:410-429 ---

func TestLockoutStateMachine(t *testing.T) {
	auth, store := authenticatorWithUser(t, "alice", "pw", core.LocalRoleAdmin)
	ctx := context.Background()
	for i := uint32(0); i < fakeLockoutThreshold; i++ {
		_, err := auth.Login(ctx, "alice", "wrong")
		var lae LocalAuthError
		if !errors.As(err, &lae) || lae.Kind != LocalAuthErrInvalidCredentials {
			t.Fatalf("attempt %d: expected invalid credentials, got %v", i, err)
		}
	}
	// The 6th attempt is refused as locked — even with the CORRECT
	// password — and no failure is recorded while locked.
	_, err := auth.Login(ctx, "alice", "pw")
	var lae LocalAuthError
	if !errors.As(err, &lae) || lae.Kind != LocalAuthErrLocked {
		t.Fatalf("expected locked, got %v", err)
	}
	user, gErr := store.GetLocalUser(ctx, "alice")
	if gErr != nil || user == nil {
		t.Fatalf("get user: %v", gErr)
	}
	if user.LockedUntil == nil || *user.LockedUntil <= nowUnix() {
		t.Fatalf("expected LockedUntil in the future, got %v", user.LockedUntil)
	}
	if user.FailedLogins != 0 {
		t.Fatalf("expected counter reset when the lock tripped, got %d", user.FailedLogins)
	}

	// Clearing the counters (admin unlock) restores access.
	if err := store.RecordLoginSuccess(ctx, "alice"); err != nil {
		t.Fatalf("record success: %v", err)
	}
	if _, err := auth.Login(ctx, "alice", "pw"); err != nil {
		t.Fatalf("expected login to succeed after unlock: %v", err)
	}
}

// --- Ported from mobula-auth/src/local.rs:431-457 ---

func TestSuccessfulLoginClearsFailuresAndIssuesAWorkingToken(t *testing.T) {
	auth, store := authenticatorWithUser(t, "alice", "pw", core.LocalRoleOperator)
	ctx := context.Background()
	if _, err := auth.Login(ctx, "alice", "wrong"); err == nil {
		t.Fatal("expected failure")
	}
	outcome, err := auth.Login(ctx, "alice", "pw")
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	user, _ := store.GetLocalUser(ctx, "alice")
	if user.FailedLogins != 0 {
		t.Fatalf("expected failures cleared, got %d", user.FailedLogins)
	}
	if outcome.Identity.Subject != "alice" {
		t.Fatalf("unexpected subject: %q", outcome.Identity.Subject)
	}
	if len(outcome.Identity.Roles) != 1 || outcome.Identity.Roles[0] != RoleOperator {
		t.Fatalf("unexpected roles: %v", outcome.Identity.Roles)
	}
	// The issued token authenticates.
	id := auth.AuthenticateToken(ctx, outcome.Token.Token)
	if id == nil || id.Subject != "alice" {
		t.Fatalf("expected authentication to succeed, got %v", id)
	}
	// Garbage and truncated tokens do not.
	if auth.AuthenticateToken(ctx, "bfr_garbage") != nil {
		t.Fatal("expected garbage token to fail")
	}
	if auth.AuthenticateToken(ctx, outcome.Token.Token[:20]) != nil {
		t.Fatal("expected truncated token to fail")
	}
}

// --- Ported from mobula-auth/src/local.rs:459-471 ---

func TestDisabledUserCannotLoginAndExistingTokensDie(t *testing.T) {
	auth, store := authenticatorWithUser(t, "alice", "pw", core.LocalRoleViewer)
	ctx := context.Background()
	outcome, err := auth.Login(ctx, "alice", "pw")
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	store.setDisabled("alice", true)
	_, err = auth.Login(ctx, "alice", "pw")
	var lae LocalAuthError
	if !errors.As(err, &lae) || lae.Kind != LocalAuthErrDisabled {
		t.Fatalf("expected disabled, got %v", err)
	}
	if auth.AuthenticateToken(ctx, outcome.Token.Token) != nil {
		t.Fatal("expected existing token to stop authenticating once disabled")
	}
}

// --- Ported from mobula-auth/src/local.rs:473-493 ---

func TestIssueTokenCapsTTLAndRequiresALiveUser(t *testing.T) {
	auth, _ := authenticatorWithUser(t, "alice", "pw", core.LocalRoleViewer)
	ctx := context.Background()

	_, _, err := auth.IssueToken(ctx, "alice", "ci", 91)
	var lae LocalAuthError
	if !errors.As(err, &lae) || lae.Kind != LocalAuthErrTTLTooLong {
		t.Fatalf("expected ttl too long, got %v", err)
	}
	_, _, err = auth.IssueToken(ctx, "alice", "ci", 0)
	if !errors.As(err, &lae) || lae.Kind != LocalAuthErrTTLTooLong {
		t.Fatalf("expected ttl too long for 0 days, got %v", err)
	}
	_, _, err = auth.IssueToken(ctx, "ghost", "ci", 30)
	if !errors.As(err, &lae) || lae.Kind != LocalAuthErrUnknownUser {
		t.Fatalf("expected unknown user, got %v", err)
	}

	minted, record, err := auth.IssueToken(ctx, "alice", "ci", 30)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if minted.Prefix != record.Prefix {
		t.Fatalf("prefix mismatch: %q vs %q", minted.Prefix, record.Prefix)
	}
	if record.ExpiresAt <= nowUnix()+29*86_400 {
		t.Fatalf("expected ~30 day expiry, got %d", record.ExpiresAt)
	}
	if auth.AuthenticateToken(ctx, minted.Token) == nil {
		t.Fatal("expected minted token to authenticate")
	}
}

// --- Ported from mobula-auth/src/local.rs:495-525 ---

func TestExpiredAndRevokedTokensAreRejected(t *testing.T) {
	auth, store := authenticatorWithUser(t, "alice", "pw", core.LocalRoleViewer)
	ctx := context.Background()

	// Expired: insert a record whose expiry is in the past.
	prefix, plaintext, err := MintTokenParts()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	hash, err := HashToken(plaintext)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := store.CreateApiToken(ctx, core.ApiTokenRecord{
		Prefix:    prefix,
		TokenHash: hash,
		Username:  "alice",
		Label:     "old",
		CreatedAt: 1,
		ExpiresAt: 2, // long past
		Revoked:   false,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if auth.AuthenticateToken(ctx, plaintext) != nil {
		t.Fatal("expected expired token to fail")
	}

	// Revoked.
	minted, _, err := auth.IssueToken(ctx, "alice", "ci", 30)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	store.revokeToken(minted.Prefix)
	if auth.AuthenticateToken(ctx, minted.Token) != nil {
		t.Fatal("expected revoked token to fail")
	}
}

// --- Ported from mobula-auth/src/local.rs:527-542 ---

func TestRoleChangesApplyLive(t *testing.T) {
	// ADR-0011: roles are a column, resolved per request — a token minted
	// as viewer picks up an admin promotion without re-login.
	auth, store := authenticatorWithUser(t, "alice", "pw", core.LocalRoleViewer)
	ctx := context.Background()
	outcome, err := auth.Login(ctx, "alice", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	id := auth.AuthenticateToken(ctx, outcome.Token.Token)
	if id == nil || len(id.Roles) != 1 || id.Roles[0] != RoleViewer {
		t.Fatalf("expected viewer role, got %v", id)
	}
	store.setRole("alice", core.LocalRoleAdmin)
	id = auth.AuthenticateToken(ctx, outcome.Token.Token)
	if id == nil || len(id.Roles) != 1 || id.Roles[0] != RoleAdmin {
		t.Fatalf("expected admin role after promotion, got %v", id)
	}
}

// --- Additional Go-specific coverage ---

func TestLocalAuthErrorMessagesAreStable(t *testing.T) {
	cases := []struct {
		err  LocalAuthError
		want string
	}{
		{LocalAuthError{Kind: LocalAuthErrInvalidCredentials}, "invalid credentials"},
		{LocalAuthError{Kind: LocalAuthErrLocked}, "account is locked"},
		{LocalAuthError{Kind: LocalAuthErrDisabled}, "account is disabled"},
		{LocalAuthError{Kind: LocalAuthErrUnknownUser}, "no such user"},
		{LocalAuthError{Kind: LocalAuthErrTTLTooLong}, "token ttl exceeds the configured maximum"},
		{LocalAuthError{Kind: LocalAuthErrBackend, Message: "boom"}, "backend error: boom"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestLocalAuthErrorUnwrap(t *testing.T) {
	source := errors.New("underlying")
	err := LocalAuthError{Kind: LocalAuthErrBackend, Message: source.Error(), Source: source}
	if !errors.Is(err, source) {
		t.Fatal("expected errors.Is to reach Source via Unwrap")
	}
}

func TestAuthenticateTokenBackendErrorFailsClosed(t *testing.T) {
	auth := NewLocalAuthenticator(&erroringStore{}, 3600, 90)
	if id := auth.AuthenticateToken(context.Background(), "bfr_abcdefgh_0123456789abcdef0123456789abcdef"); id != nil {
		t.Fatal("expected backend errors to fail closed (nil identity)")
	}
}

// erroringStore is a LocalUserStore whose every method fails, exercising
// AuthenticateToken's fail-closed posture on backend errors.
type erroringStore struct{}

func (erroringStore) GetLocalUser(context.Context, string) (*core.LocalUserRecord, error) {
	return nil, errors.New("backend down")
}
func (erroringStore) RecordLoginFailure(context.Context, string) error {
	return errors.New("backend down")
}
func (erroringStore) RecordLoginSuccess(context.Context, string) error {
	return errors.New("backend down")
}
func (erroringStore) CreateApiToken(context.Context, core.ApiTokenRecord) error {
	return errors.New("backend down")
}
func (erroringStore) GetApiTokenByPrefix(context.Context, string) (*core.ApiTokenRecord, error) {
	return nil, errors.New("backend down")
}
func (erroringStore) TouchApiToken(context.Context, string, uint64) error {
	return errors.New("backend down")
}

func TestIdentityOfNeverMarshalsAndRoleMapping(t *testing.T) {
	email := "alice@example.com"
	user := &core.LocalUserRecord{Username: "alice", Email: &email, Role: core.LocalRoleAuditor}
	id := identityOf(user)
	if id.Roles[0] != RoleAuditor {
		t.Fatalf("expected auditor role, got %v", id.Roles)
	}
	if _, err := json.Marshal(id); err == nil {
		t.Fatal("expected Identity to refuse marshal")
	}
}
