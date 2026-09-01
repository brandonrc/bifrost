package main

import (
	"context"
	"testing"
)

func TestParseStoreKind(t *testing.T) {
	cases := []struct {
		in      string
		want    storeKind
		wantErr bool
	}{
		{"memory", storeMemory, false},
		{"MEMORY", storeMemory, false},
		{"sqlite", storeSqlite, false},
		{"postgres", storePostgres, false},
		{"", "", true},
		{"mongo", "", true},
	}
	for _, tc := range cases {
		got, err := parseStoreKind(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseStoreKind(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseStoreKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenStoreMemory(t *testing.T) {
	store, closeFn, err := openStore(context.Background(), storeMemory, "")
	if err != nil {
		t.Fatalf("openStore(memory): %v", err)
	}
	if store == nil {
		t.Fatal("openStore(memory) returned a nil store")
	}
	if err := closeFn(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOpenStoreSqliteRequiresDB(t *testing.T) {
	if _, _, err := openStore(context.Background(), storeSqlite, ""); err == nil {
		t.Fatal("expected an error for --store sqlite without --db")
	}
}

func TestOpenStorePostgresRequiresDB(t *testing.T) {
	if _, _, err := openStore(context.Background(), storePostgres, ""); err == nil {
		t.Fatal("expected an error for --store postgres without --db")
	}
}

func TestOpenStoreSqliteFile(t *testing.T) {
	dir := t.TempDir()
	store, closeFn, err := openStore(context.Background(), storeSqlite, dir+"/state.db")
	if err != nil {
		t.Fatalf("openStore(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	if _, err := store.List(context.Background()); err != nil {
		t.Fatalf("List on a freshly opened sqlite store: %v", err)
	}
}
