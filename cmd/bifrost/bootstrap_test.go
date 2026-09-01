package main

import (
	"context"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
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
