// Package internal_test holds the L1 guard that keeps the retired
// predecessor's product name out of this repository.
package internal_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// legacyIdentity is the retired product name of the Rust predecessor. The
// codebase is a line-by-line port and cites its heritage everywhere ("ported
// from the predecessor's auth crate, src/local.rs:28"); those citations were
// reworded to neutral vocabulary in one sweep (PR feat/rename-predecessor).
// This test is the gate that stops the name creeping back: any
// case-insensitive match outside the allowlist below fails the build.
//
// scripts/legacy-identity-sweep.sh is the narrower cross-repo triage tool for
// LIVE runtime strings (it skips comments and prose); this guard is the
// broader repo-wide wording check and runs under `go test ./...`.
const legacyIdentity = "mobula"

// allowedPathPrefixes are trees the sweep deliberately did not touch.
var allowedPathPrefixes = []string{
	// Dated records and plans: rewriting history would falsify provenance.
	"docs/superpowers/handoff/",
	"docs/superpowers/plans/",
	// The live-string sweep script names the term as its search default and
	// documents the earlier sweeps of it.
	"scripts/legacy-identity-sweep.sh",
	// This guard names the term it looks for.
	"internal/legacy_identity_test.go",
	// Owned by the contract package (A1); its text is published downstream.
	"internal/api/openapi.json",
	"go.sum",
}

// allowedIdentifiers are real external identifiers that still carry the name:
// renaming them is a deployment change, not a wording change. A line is only
// a finding if the name survives after every such identifier is removed.
var allowedIdentifiers = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`realms/mobula`,             // Keycloak realm (deploy/keycloak lockstep)
	`mobula-admins`,             // Keycloak group mapped to the admin role
	`localhost:32000/mobula\S*`, // image names in the internal registry
	`brandonrc/mobula\S*`,       // GitHub repos and the @brandonrc/mobula-client npm package
	`mobula-pack`,               // chart/repo name
}, "|"))

// skipDirs never hold product source.
var skipDirs = map[string]bool{
	".git": true, ".claude": true, ".l3": true, ".superpowers": true,
	"node_modules": true, "vendor": true, "dist": true, "l3-report": true,
}

func TestNoLegacyIdentityOutsideAllowlist(t *testing.T) {
	// Self-test first: a guard over a clean tree passes whether or not it
	// works, so prove the matcher can still see a planted finding and still
	// honours the identifier allowlist.
	if !lineOffends(`const K = "` + legacyIdentity + `.planted-key"`) {
		t.Fatal("self-test: the matcher did not flag a planted finding; a clean result would be meaningless")
	}
	if lineOffends("issuer := " + `"http://localhost:8090/realms/` + legacyIdentity + `"`) {
		t.Fatal("self-test: the matcher flagged an allowlisted identifier")
	}

	root := repoRoot(t)
	var findings []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || isAllowedPath(rel) {
			return nil
		}
		hits, scanErr := scanFile(path)
		if scanErr != nil {
			return scanErr
		}
		for _, h := range hits {
			findings = append(findings, rel+":"+h)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(findings) > 0 {
		t.Errorf("%d line(s) mention the retired predecessor name %q outside the allowlist; "+
			"reword them (see the vocabulary in docs/superpowers/plans, package J) or, for a real "+
			"external identifier, extend allowedIdentifiers with a reason:\n  %s",
			len(findings), legacyIdentity, strings.Join(findings, "\n  "))
	}
}

func isAllowedPath(rel string) bool {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, "zz_generated_") && strings.HasSuffix(base, ".go") {
		return true
	}
	// Local build outputs (.gitignore): never product source.
	switch base {
	case "coverage.txt", "l2.json", "bifrost":
		return true
	}
	if strings.HasSuffix(base, ".out") || (strings.HasPrefix(base, "l3-") && strings.HasSuffix(base, ".json")) {
		return true
	}
	for _, p := range allowedPathPrefixes {
		if rel == p || strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// scanFile returns "line:content" for every offending line. Binary files
// (a NUL byte in the first 8 KiB) are skipped.
func scanFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 8192)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var hits []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for ln := 1; sc.Scan(); ln++ {
		if lineOffends(sc.Text()) {
			hits = append(hits, strconv.Itoa(ln)+":"+strings.TrimSpace(sc.Text()))
		}
	}
	return hits, sc.Err()
}

func lineOffends(line string) bool {
	if !strings.Contains(strings.ToLower(line), legacyIdentity) {
		return false
	}
	return strings.Contains(strings.ToLower(allowedIdentifiers.ReplaceAllString(line, "")), legacyIdentity)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
