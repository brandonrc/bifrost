package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/brandonrc/bifrost/internal/controller"
)

// storeKind is the --store backend selector.
type storeKind string

const (
	storeMemory   storeKind = "memory"
	storeSqlite   storeKind = "sqlite"
	storePostgres storeKind = "postgres"
)

// parseStoreKind validates the --store flag value.
func parseStoreKind(s string) (storeKind, error) {
	switch storeKind(strings.ToLower(s)) {
	case storeMemory:
		return storeMemory, nil
	case storeSqlite:
		return storeSqlite, nil
	case storePostgres:
		return storePostgres, nil
	default:
		return "", fmt.Errorf("unknown --store %q: want memory, sqlite, or postgres", s)
	}
}

// openStore constructs the selected backend and returns its Store plus a
// close func (always non-nil; a no-op for the in-memory backend, which
// owns no external resource). --store defaults to "memory", so every
// deployment gets a concrete Store even when neither --db nor
// --local-auth nor --namespace names a reason to persist — server.go's
// Server.Store is the one dependency every real operation reaches
// through, so leaving it nil is a footgun this CLI simply never creates.
//
// db_target's TOML-era prefix-sniffing ("postgres://... picks Postgres,
// anything else is a SQLite path") is deliberately NOT ported: T15 makes
// the backend an explicit --store flag instead, matching this task's own
// brief ("a flag (e.g. --store memory|sqlite|postgres + a DSN/path)")
// rather than mobula-cli's implicit one.
func openStore(ctx context.Context, kind storeKind, db string) (controller.Store, func() error, error) {
	switch kind {
	case storeMemory:
		return controller.NewMemoryStore(), func() error { return nil }, nil
	case storeSqlite:
		if db == "" {
			return nil, nil, fmt.Errorf("--store sqlite requires --db <path>")
		}
		s, err := controller.NewSqliteStore(ctx, db)
		if err != nil {
			return nil, nil, err
		}
		return s, s.Close, nil
	case storePostgres:
		if db == "" {
			return nil, nil, fmt.Errorf("--store postgres requires --db <postgres:// URL>")
		}
		s, err := controller.NewPostgresStore(ctx, db)
		if err != nil {
			return nil, nil, err
		}
		return s, s.Close, nil
	default:
		return nil, nil, fmt.Errorf("unknown store kind %q", kind)
	}
}
