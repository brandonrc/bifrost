package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonrc/bifrost/internal/api"
	"github.com/brandonrc/bifrost/internal/auth"
)

func newLoginCmd() *cobra.Command {
	var (
		issuer        string
		clientID      string
		scope         string
		local         bool
		username      string
		passwordStdin bool
		server        string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in and store the token: OIDC device-code flow by default, or local username/password auth with --local",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if local {
				if username == "" {
					return errors.New("--local requires --username")
				}
				return runLoginLocal(cmd.Context(), server, username, passwordStdin)
			}
			if issuer == "" {
				return errors.New("--issuer is required unless --local is set")
			}
			return runLogin(cmd.Context(), issuer, clientID, scope)
		},
	}
	f := cmd.Flags()
	f.StringVar(&issuer, "issuer", "", "OIDC issuer URL (e.g. https://keycloak.example/realms/bifrost)")
	f.StringVar(&clientID, "client-id", "bifrost-cli", "Public OAuth client id registered for the Bifrost CLI")
	f.StringVar(&scope, "scope", "openid profile email", "Requested scopes")
	f.BoolVar(&local, "local", false, "Local auth: username/password login against the control plane")
	f.StringVar(&username, "username", "", "Local auth username")
	f.BoolVar(&passwordStdin, "password-stdin", false, "Local auth: read the password from stdin (one line)")
	f.StringVar(&server, "server", envOr("BIFROST_SERVER", "http://127.0.0.1:8484"), "Control-plane URL for local login")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out: revoke the stored token server-side when it is a local PAT, then delete the stored credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogout(cmd.Context())
		},
	}
}

// runLogin runs the OIDC device-code flow (RFC 8628) against issuer and
// stores the resulting credentials. Ported from the predecessor CLI's login.
func runLogin(ctx context.Context, issuer, clientID, scope string) error {
	client := auth.IdpClient()
	meta, err := auth.DiscoverMetadata(ctx, client, issuer)
	if err != nil {
		return err
	}
	if meta.DeviceAuthorizationEndpoint == nil {
		return errors.New("issuer does not advertise a device_authorization_endpoint")
	}
	if meta.TokenEndpoint == nil {
		return errors.New("issuer does not advertise a token_endpoint")
	}

	da, err := auth.DeviceAuthorize(ctx, client, *meta.DeviceAuthorizationEndpoint, clientID, scope)
	if err != nil {
		return err
	}
	verification := da.VerificationURI
	if da.VerificationURIComplete != nil {
		verification = *da.VerificationURIComplete
	}
	fmt.Printf("To sign in, open:\n\n    %s\n\nand enter code: %s\n\n", verification, da.UserCode)

	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	interval := da.Interval
	if interval == 0 {
		interval = 1
	}
	var token *auth.TokenResponse
	for {
		if time.Now().After(deadline) {
			return errors.New("device code expired before approval")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
		poll, err := auth.PollDeviceToken(ctx, client, *meta.TokenEndpoint, clientID, da.DeviceCode)
		if err != nil {
			return err
		}
		if poll.Ready {
			token = poll.Token
			break
		}
		if poll.SlowDown {
			interval += 5 // RFC 8628 §3.5
		}
	}

	cid := clientID
	if err := saveCredentials(Credentials{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Issuer:       issuer,
		ClientID:     &cid,
	}); err != nil {
		return err
	}
	if token.ExpiresIn != nil {
		fmt.Printf("Logged in. Token expires in %ds.\n", *token.ExpiresIn)
	} else {
		fmt.Println("Logged in.")
	}
	fmt.Println("Attach it to Ray jobs with:")
	fmt.Println(`  export RAY_JOB_HEADERS="{\"Authorization\": \"Bearer $(bifrost token)\"}"`)
	return nil
}

// runLoginLocal signs in against the control plane's local (IdP-free)
// auth (ADR-0011): POST /api/v1/auth/login, store the opaque `bfr_…`
// token like a device-flow token (0600). Ported from the predecessor CLI's
// login_local; reuses internal/api's LoginRequest/LoginResponse DTOs
// rather than re-declaring the wire shape.
func runLoginLocal(ctx context.Context, server, username string, passwordStdin bool) error {
	if !passwordStdin {
		return errors.New("no interactive password prompt is available; pipe the password with --password-stdin")
	}
	password, err := readLineFromStdin()
	if err != nil {
		return err
	}

	body, err := json.Marshal(api.LoginRequest{Username: username, Password: password})
	if err != nil {
		return err
	}
	url := strings.TrimRight(server, "/") + "/api/v1/auth/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := auth.IdpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed: %s", resp.Status)
	}
	var out api.LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}

	if err := saveCredentials(Credentials{AccessToken: out.Token, Issuer: server}); err != nil {
		return err
	}
	fmt.Printf("Logged in as %s (roles: %s).\n", out.Identity.Subject, strings.Join(out.Identity.Roles, ", "))
	return nil
}

// runLogout revokes the stored token server-side when it is a local PAT
// (recognised by auth.TokenScheme), best-effort, then always deletes the
// local credentials file. OIDC JWTs are stateless — nothing to revoke
// server-side.
func runLogout(ctx context.Context) error {
	creds, err := loadCredentialsFile()
	if err != nil {
		return err
	}
	if strings.HasPrefix(creds.AccessToken, auth.TokenScheme) {
		url := strings.TrimRight(creds.Issuer, "/") + "/api/v1/auth/logout"
		if req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil); err == nil {
			req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
			resp, err := auth.IdpClient().Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Printf("Logged out; removed %s\n", path)
	return nil
}
