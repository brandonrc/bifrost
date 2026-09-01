package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return path
}

func TestLoadRegistryMissingFile(t *testing.T) {
	if _, err := loadRegistry("/nonexistent/clusters.json", false); err == nil {
		t.Fatal("expected an error for a missing registry file")
	}
}

func TestLoadRegistryInvalidJSON(t *testing.T) {
	path := writeTemp(t, "bad.json", `{"clusters": not json}`)
	if _, err := loadRegistry(path, false); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestLoadRegistryValidFile(t *testing.T) {
	path := writeTemp(t, "ok.json", `{"clusters": [
		{"id": "a", "hostname": "a.test", "api_base_url": "http://a:8265"}
	]}`)
	reg, err := loadRegistry(path, false)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	if len(reg.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(reg.Clusters))
	}
}

func TestLoadRegistryResolvesAuthTokenEnv(t *testing.T) {
	const envVar = "BIFROST_CLI_TEST_REGISTRY_TOKEN"
	// https:// so this test exercises only the auth_token_env resolution,
	// independent of Validate's separate cleartext-transport check
	// (covered by TestLoadRegistryRejectsCleartextTokenWithoutOverride).
	path := writeTemp(t, "env.json", `{"clusters": [
		{"id": "a", "hostname": "a.test", "api_base_url": "https://a:8265", "auth_token_env": "`+envVar+`"}
	]}`)

	_ = os.Unsetenv(envVar)
	if _, err := loadRegistry(path, false); err == nil {
		t.Fatal("expected an error when the env var naming the token is unset")
	}

	t.Setenv(envVar, "cli-env-secret")
	reg, err := loadRegistry(path, false)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	if reg.Clusters[0].AuthToken == nil || *reg.Clusters[0].AuthToken != "cli-env-secret" {
		t.Fatalf("auth_token = %v, want resolved cli-env-secret", reg.Clusters[0].AuthToken)
	}
	if reg.Clusters[0].AuthTokenEnv == nil || *reg.Clusters[0].AuthTokenEnv != envVar {
		t.Fatalf("auth_token_env should be retained as provenance, got %v", reg.Clusters[0].AuthTokenEnv)
	}
}

func TestLoadRegistryRejectsConflictingTokenSources(t *testing.T) {
	path := writeTemp(t, "both.json", `{"clusters": [
		{"id": "a", "hostname": "a.test", "api_base_url": "http://a:8265", "auth_token": "secret", "auth_token_env": "SOME_VAR"}
	]}`)
	if _, err := loadRegistry(path, false); err == nil {
		t.Fatal("expected an error for both auth_token and auth_token_env set")
	}
}

func TestLoadRegistryRejectsCleartextTokenWithoutOverride(t *testing.T) {
	path := writeTemp(t, "cleartext.json", `{"clusters": [
		{"id": "a", "hostname": "a.test", "api_base_url": "http://a:8265", "auth_token": "secret"}
	]}`)
	if _, err := loadRegistry(path, false); err == nil {
		t.Fatal("expected Validate to refuse a plaintext token over http:// without --allow-insecure-transport")
	}
	if _, err := loadRegistry(path, true); err != nil {
		t.Fatalf("with --allow-insecure-transport: %v", err)
	}
}

func TestLoadRegistryRejectsDuplicateIds(t *testing.T) {
	path := writeTemp(t, "dup.json", `{"clusters": [
		{"id": "a", "hostname": "a.test", "api_base_url": "http://a:8265"},
		{"id": "a", "hostname": "b.test", "api_base_url": "http://b:8265"}
	]}`)
	if _, err := loadRegistry(path, false); err == nil {
		t.Fatal("expected Validate to refuse a duplicate cluster id")
	}
}
