package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/core"
)

func TestBootstrapLocalAdminCreatesAdminOnce(t *testing.T) {
	ctx := context.Background()
	store := controller.NewMemoryStore()
	t.Setenv(localAdminPasswordEnv, "bootstrap-test-pw")

	if err := bootstrapLocalAdmin(ctx, store, ""); err != nil {
		t.Fatalf("bootstrapLocalAdmin: %v", err)
	}
	// Second call is a no-op (users table no longer empty).
	if err := bootstrapLocalAdmin(ctx, store, ""); err != nil {
		t.Fatalf("bootstrapLocalAdmin (second call): %v", err)
	}

	users, err := store.ListLocalUsers(ctx)
	if err != nil {
		t.Fatalf("ListLocalUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %d, want 1", len(users))
	}
	if users[0].Username != "admin" {
		t.Fatalf("username = %q, want admin", users[0].Username)
	}
	if users[0].Role != core.LocalRoleAdmin {
		t.Fatalf("role = %q, want admin", users[0].Role)
	}

	// The env password actually verifies, via the auth module.
	authenticator := auth.NewLocalAuthenticator(store, 3600, 90)
	if _, err := authenticator.Login(ctx, "admin", "bootstrap-test-pw"); err != nil {
		t.Fatalf("Login with the bootstrapped password: %v", err)
	}
}

// F4 regression: the bootstrapped admin password must never appear in the
// logs — generated passwords go to a 0600 file whose PATH is logged, and
// the env-var path keeps its distinct warning without echoing the value.
func TestBootstrapLocalAdminNeverLogsPassword(t *testing.T) {
	ctx := context.Background()

	t.Run("generated password written to file", func(t *testing.T) {
		buf := captureServeLogs(t)
		store := controller.NewMemoryStore()
		dbPath := filepath.Join(t.TempDir(), "bifrost.db")

		if err := bootstrapLocalAdmin(ctx, store, dbPath); err != nil {
			t.Fatalf("bootstrapLocalAdmin: %v", err)
		}

		// Recover the generated password from the 0600 file it was written
		// to, then assert the logs carry the path but never the value.
		pwPath := filepath.Join(filepath.Dir(dbPath), "local-admin-password")
		data, err := os.ReadFile(pwPath)
		if err != nil {
			t.Fatalf("reading the password file: %v", err)
		}
		password := strings.TrimSpace(string(data))
		if password == "" {
			t.Fatal("password file is empty")
		}
		if info, err := os.Stat(pwPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("password file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
		}
		if !strings.Contains(buf.String(), pwPath) {
			t.Errorf("expected the logs to name the password file %q, got %q", pwPath, buf.String())
		}
		if strings.Contains(buf.String(), password) {
			t.Errorf("the generated admin password appears in the logs: %q", buf.String())
		}
	})

	t.Run("env-var password keeps its distinct warning", func(t *testing.T) {
		buf := captureServeLogs(t)
		store := controller.NewMemoryStore()
		t.Setenv(localAdminPasswordEnv, "env-pw-must-not-be-logged")

		if err := bootstrapLocalAdmin(ctx, store, ""); err != nil {
			t.Fatalf("bootstrapLocalAdmin: %v", err)
		}
		if !strings.Contains(buf.String(), localAdminPasswordEnv) {
			t.Errorf("expected the distinct %s warning, got %q", localAdminPasswordEnv, buf.String())
		}
		if strings.Contains(buf.String(), "env-pw-must-not-be-logged") {
			t.Errorf("the env-var admin password appears in the logs: %q", buf.String())
		}
	})

	t.Run("generated password without a db path is not logged", func(t *testing.T) {
		buf := captureServeLogs(t)
		store := controller.NewMemoryStore()

		if err := bootstrapLocalAdmin(ctx, store, ""); err != nil {
			t.Fatalf("bootstrapLocalAdmin: %v", err)
		}
		if !strings.Contains(buf.String(), "NOT persisted") {
			t.Errorf("expected a warning that the password was not persisted, got %q", buf.String())
		}
		// The generated value is unknowable here; the structural guard is
		// that no log line claims to show it.
		if strings.Contains(buf.String(), "shown once") {
			t.Errorf("a 'shown once' password line remains in the logs: %q", buf.String())
		}
	})
}
