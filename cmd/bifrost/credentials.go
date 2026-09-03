package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// credentialsEnvDir overrides the credentials directory, ported from
// the predecessor CLI's config-dir variable (renamed for product identity).
const credentialsEnvDir = "BIFROST_CONFIG_DIR"

// Credentials is the CLI's on-disk login state
// (~/.config/bifrost/credentials.json, mode 0600), ported field-for-field
// from the predecessor CLI's Credentials struct.
type Credentials struct {
	AccessToken string `json:"access_token"`
	// RefreshToken is set for a device-code login when the IdP issued
	// one; nil for local-auth logins and IdPs that don't support refresh.
	RefreshToken *string `json:"refresh_token,omitempty"`
	// Issuer is the OIDC issuer for a device-code login, or the
	// control-plane URL for a local-auth login (login_local reuses this
	// field for both — see runLoginLocal).
	Issuer string `json:"issuer"`
	// ClientID is the OAuth client id used at login, needed for the
	// refresh_token grant. nil for local logins and credential files
	// written before this field existed.
	ClientID *string `json:"client_id,omitempty"`
}

// credentialsPath resolves ~/.config/bifrost/credentials.json, or
// $BIFROST_CONFIG_DIR/credentials.json when that env var is set.
func credentialsPath() (string, error) {
	if dir := os.Getenv(credentialsEnvDir); dir != "" {
		return filepath.Join(dir, "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("HOME is not set; set %s", credentialsEnvDir)
	}
	return filepath.Join(home, ".config", "bifrost", "credentials.json"), nil
}

// saveCredentials writes creds to credentialsPath(), creating its parent
// directory as needed and restricting the file to 0600 — it carries a
// bearer-equivalent secret.
func saveCredentials(creds Credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// loadCredentialsFile reads the stored credentials, naming the expected
// path in its error when none exist (a `bifrost login` nudge).
func loadCredentialsFile() (Credentials, error) {
	var creds Credentials
	path, err := credentialsPath()
	if err != nil {
		return creds, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return creds, fmt.Errorf("no stored credentials at %s — run `bifrost login`", path)
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return creds, err
	}
	return creds, nil
}
