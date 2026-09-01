package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonrc/bifrost/internal/auth"
)

func newTokenCmd() *cobra.Command {
	var issuer, clientID, clientSecret, scope string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print a bearer token: the stored login token by default, or a fresh service-account token with --issuer/--client-id/--client-secret",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var scopePtr *string
			if scope != "" {
				scopePtr = &scope
			}
			return runToken(cmd.Context(), issuer, clientID, clientSecret, scopePtr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&issuer, "issuer", "", "Service account: OIDC issuer URL")
	f.StringVar(&clientID, "client-id", "", "Service account: confidential client id")
	f.StringVar(&clientSecret, "client-secret", envOr("BIFROST_CLIENT_SECRET", ""), "Service account: client secret (or set BIFROST_CLIENT_SECRET)")
	f.StringVar(&scope, "scope", "", "Optional scope for the client-credentials grant")
	return cmd
}

// runToken mirrors mobula-cli's Token dispatch: a full service-account
// triple (issuer/client-id/client-secret) runs a client-credentials grant;
// otherwise it prints (refreshing if needed) the stored login token.
func runToken(ctx context.Context, issuer, clientID, clientSecret string, scope *string) error {
	if issuer != "" && clientID != "" && clientSecret != "" {
		return serviceToken(ctx, issuer, clientID, clientSecret, scope)
	}
	creds, err := loadCredentialsFile()
	if err != nil {
		return err
	}
	switch storedTokenAction(creds, uint64(time.Now().Unix())) {
	case tokenActionValid:
		fmt.Println(creds.AccessToken)
		return nil
	case tokenActionExpiredNoRefresh:
		return errors.New("token expired, run bifrost login")
	default: // tokenActionRefresh
		return refreshStoredToken(ctx, creds)
	}
}

func serviceToken(ctx context.Context, issuer, clientID, clientSecret string, scope *string) error {
	client := auth.IdpClient()
	meta, err := auth.DiscoverMetadata(ctx, client, issuer)
	if err != nil {
		return err
	}
	if meta.TokenEndpoint == nil {
		return errors.New("issuer does not advertise a token_endpoint")
	}
	tok, err := auth.ClientCredentials(ctx, client, *meta.TokenEndpoint, clientID, clientSecret, scope)
	if err != nil {
		return err
	}
	// Print only the token so it composes: $(bifrost token --issuer ...)
	fmt.Println(tok.AccessToken)
	return nil
}

// tokenAction is what `bifrost token` should do with the stored access
// token (#18), ported from mobula-cli's StoredTokenAction.
type tokenAction int

const (
	// tokenActionValid: print it as-is.
	tokenActionValid tokenAction = iota
	// tokenActionRefresh: expired JWT with a refresh token — attempt a
	// refresh grant.
	tokenActionRefresh
	// tokenActionExpiredNoRefresh: expired JWT, no way to refresh — the
	// user must re-login.
	tokenActionExpiredNoRefresh
)

// storedTokenAction is the client-side expiry decision for the stored
// token. Opaque local-auth tokens (`mob_…`) carry no exp — the server
// enforces their lifetime, so they pass through valid, as do undecodable
// tokens (the server validates for real; this is display-only hygiene).
// Ported from mobula-cli's stored_token_action.
func storedTokenAction(creds Credentials, now uint64) tokenAction {
	exp, ok := jwtExp(creds.AccessToken)
	if !ok || exp > now {
		return tokenActionValid
	}
	if creds.RefreshToken != nil {
		return tokenActionRefresh
	}
	return tokenActionExpiredNoRefresh
}

// jwtExp decodes the `exp` claim from a JWT payload WITHOUT verifying the
// signature — client-side display only; the server is the validator.
// false for opaque tokens, non-JWT strings, and payloads without `exp`.
// Ported from mobula-cli's jwt_exp (using encoding/base64.RawURLEncoding
// in place of its hand-rolled base64url decoder — the standard library
// already has RFC 4648 §5 covered).
func jwtExp(token string) (uint64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp *uint64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil {
		return 0, false
	}
	return *claims.Exp, true
}

// refreshStoredToken performs a refresh grant (RFC 6749 §6) against the
// stored issuer, persisting the new tokens (0600) and printing the fresh
// access token. Any failure — discovery, transport, grant rejected —
// means re-login (#18), ported from mobula-cli's refresh_stored_token.
func refreshStoredToken(ctx context.Context, creds Credentials) error {
	reLogin := errors.New("token expired, run bifrost login")
	if creds.RefreshToken == nil {
		return reLogin
	}
	client := auth.IdpClient()
	meta, err := auth.DiscoverMetadata(ctx, client, creds.Issuer)
	if err != nil {
		return reLogin
	}
	if meta.TokenEndpoint == nil {
		return reLogin
	}
	clientID := "bifrost-cli"
	if creds.ClientID != nil {
		clientID = *creds.ClientID
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {*creds.RefreshToken},
		"client_id":     {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return reLogin
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return reLogin
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return reLogin
	}
	var tok auth.TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return reLogin
	}

	// Providers may rotate refresh tokens; keep the old one when the
	// response omits a replacement.
	refresh := creds.RefreshToken
	if tok.RefreshToken != nil {
		refresh = tok.RefreshToken
	}
	if err := saveCredentials(Credentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: refresh,
		Issuer:       creds.Issuer,
		ClientID:     creds.ClientID,
	}); err != nil {
		return err
	}
	fmt.Println(tok.AccessToken)
	return nil
}
