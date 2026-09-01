package main

import (
	"strings"
	"testing"
)

const leakTestSecret = "leaktest123"

func TestResolveClientSecret(t *testing.T) {
	t.Setenv(clientSecretEnv, "")
	if got := resolveClientSecret("from-flag"); got != "from-flag" {
		t.Fatalf("resolveClientSecret(explicit flag) = %q, want %q", got, "from-flag")
	}

	t.Setenv(clientSecretEnv, leakTestSecret)
	if got := resolveClientSecret(""); got != leakTestSecret {
		t.Fatalf("resolveClientSecret(unset flag) = %q, want env value %q", got, leakTestSecret)
	}
	// The flag, when explicitly set, still wins over the env var.
	if got := resolveClientSecret("from-flag"); got != "from-flag" {
		t.Fatalf("resolveClientSecret(flag set, env also set) = %q, want flag value", got)
	}
}

// TestClientSecretHelpDoesNotLeakEnv is the regression test for the MEDIUM
// finding: --client-secret must never be wired as a pflag default sourced
// from BIFROST_CLIENT_SECRET, because pflag prints any non-empty default
// inline in --help/usage text. With the env var set, neither `token
// --help` nor `exchange --help` may contain the secret value anywhere in
// their usage output.
func TestClientSecretHelpDoesNotLeakEnv(t *testing.T) {
	t.Setenv(clientSecretEnv, leakTestSecret)

	for _, cmd := range []struct {
		name string
		new  func() interface{ UsageString() string }
	}{
		{"token", func() interface{ UsageString() string } { return newTokenCmd() }},
		{"exchange", func() interface{ UsageString() string } { return newExchangeCmd() }},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			usage := cmd.new().UsageString()
			if strings.Contains(usage, leakTestSecret) {
				t.Fatalf("%s --help leaks BIFROST_CLIENT_SECRET into usage output:\n%s", cmd.name, usage)
			}
		})
	}
}

// TestClientSecretFlagDefaultIsEmpty pins the DefValue directly (belt and
// suspenders alongside the UsageString check above, which is what an
// actual `--help` invocation renders).
func TestClientSecretFlagDefaultIsEmpty(t *testing.T) {
	t.Setenv(clientSecretEnv, leakTestSecret)

	tokenFlag := newTokenCmd().Flags().Lookup("client-secret")
	if tokenFlag == nil {
		t.Fatal("token: no --client-secret flag registered")
	}
	if tokenFlag.DefValue != "" {
		t.Fatalf("token --client-secret DefValue = %q, want empty (env must not become the pflag default)", tokenFlag.DefValue)
	}

	exchangeFlag := newExchangeCmd().Flags().Lookup("client-secret")
	if exchangeFlag == nil {
		t.Fatal("exchange: no --client-secret flag registered")
	}
	if exchangeFlag.DefValue != "" {
		t.Fatalf("exchange --client-secret DefValue = %q, want empty (env must not become the pflag default)", exchangeFlag.DefValue)
	}
}
