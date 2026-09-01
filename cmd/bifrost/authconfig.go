package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/brandonrc/bifrost/internal/auth"
)

// loadAuthConfig reads a JSON OIDC validator config (issuer, audience,
// groups_claim, role mappings) for --auth-config. auth.AuthConfig carries
// `json` tags (not `toml`), same format choice and rationale as
// registry.go's loadRegistry.
func loadAuthConfig(path string) (auth.AuthConfig, error) {
	var cfg auth.AuthConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid auth config %s: %w", path, err)
	}
	return cfg, nil
}
