package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// localAdminPasswordEnv overrides the bootstrapped admin password
// (demos/tests only). Ported from the predecessor CLI's admin-password variable,
// renamed for product identity (Global Constraints).
const localAdminPasswordEnv = "BIFROST_LOCAL_ADMIN_PASSWORD"

// bootstrapLocalAdmin creates the first local "admin" user (ADR-0011) the
// first time --local-auth boots against an empty local_users table —
// idempotent: a no-op once any local user exists, mirroring the predecessor CLI's
// bootstrap_local_admin exactly. dbPath is the --db value ONLY when it
// names a SQLite file path; the generated password is written 0600 next
// to it so an operator can retrieve it without scraping logs.
func bootstrapLocalAdmin(ctx context.Context, store controller.Store, dbPath string) error {
	users, err := store.ListLocalUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}

	password := os.Getenv(localAdminPasswordEnv)
	fromEnv := password != ""
	if !fromEnv {
		p, err := auth.RandomPassword(20)
		if err != nil {
			return err
		}
		password = p
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if err := store.CreateLocalUser(ctx, "admin", nil, hash, core.LocalRoleAdmin); err != nil {
		return err
	}

	if fromEnv {
		slog.Warn("bootstrapped local 'admin' from " + localAdminPasswordEnv +
			" — demo use only; change the password and unset the variable")
		return nil
	}
	if dbPath != "" {
		dir := filepath.Dir(dbPath)
		pwPath := filepath.Join(dir, "local-admin-password")
		if err := os.WriteFile(pwPath, []byte(password+"\n"), 0o600); err != nil {
			return err
		}
		slog.Warn("local 'admin' password written (0600)", "path", pwPath)
	}
	slog.Warn(fmt.Sprintf("local auth bootstrap — admin password (shown once): %s", password))
	return nil
}
