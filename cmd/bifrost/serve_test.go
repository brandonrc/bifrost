package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/api"
)

// TestBuildServerGatewayOnly is the smoke test the task brief asked for:
// serve builds a working handler without opening any socket (buildServer
// never listens — see serve.go's doc comment).
func TestBuildServerGatewayOnly(t *testing.T) {
	built, err := buildServer(context.Background(), serveOptions{StoreKind: "memory"})
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	if built.app.Handler == nil {
		t.Fatal("buildServer returned a nil handler")
	}
	if built.app.Store == nil {
		t.Fatal("buildServer returned a nil store (--store defaults to memory)")
	}
	t.Cleanup(func() { _ = built.closeStore() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	// httptest.NewRequest defaults RemoteAddr to a documented non-loopback
	// placeholder (192.0.2.1) — set it to a real loopback peer so this
	// request exercises the "dev caller on the default loopback bind"
	// path, distinct from TestServeHandlerFailsClosedOnNonLoopbackPeer.
	req.RemoteAddr = "127.0.0.1:54321"
	built.app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestServeHandlerFailsClosedOnNonLoopbackPeer exercises the router-level
// fail-closed guard (api.RefuseNonLoopback, spliced in by api.NewHandler
// when neither a validator nor local auth is configured and
// AllowUnauthenticated is false): a request from a non-loopback peer is
// refused even though nothing ever bound a real non-loopback listener —
// httptest-level, no real listen needed.
func TestServeHandlerFailsClosedOnNonLoopbackPeer(t *testing.T) {
	built, err := buildServer(context.Background(), serveOptions{StoreKind: "memory"})
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() { _ = built.closeStore() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.5:12345" // TEST-NET-3, definitely not loopback
	built.app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback peer, no auth configured: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestServeHandlerAllowUnauthenticatedLiftsTheGuard confirms
// --dev-allow-unauthenticated actually reaches api.NewHandler and lifts
// the router-level guard the previous test relies on by default.
func TestServeHandlerAllowUnauthenticatedLiftsTheGuard(t *testing.T) {
	built, err := buildServer(context.Background(), serveOptions{StoreKind: "memory", DevAllowUnauthenticated: true})
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() { _ = built.closeStore() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	built.app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("--dev-allow-unauthenticated: status = %d, want 200 (got body %q)", rec.Code, rec.Body.String())
	}
}

// TestCheckBindAllowedFailsClosed exercises the bind-time guard (the
// other fail-closed layer runServe calls before ListenAndServe) directly,
// mirroring the loopback/non-loopback cases the brief asked for.
func TestCheckBindAllowedFailsClosed(t *testing.T) {
	cases := []struct {
		name                 string
		bind                 string
		authConfigured       bool
		allowUnauthenticated bool
		wantErr              bool
	}{
		{"non-loopback, no auth, no override -> refused", "0.0.0.0:8484", false, false, true},
		{"loopback, no auth -> allowed", "127.0.0.1:8484", false, false, false},
		{"non-loopback, auth configured -> allowed", "0.0.0.0:8484", true, false, false},
		{"non-loopback, explicit override -> allowed", "0.0.0.0:8484", false, true, false},
		{"unparseable bind (no port) -> refused (not provably loopback)", "0.0.0.0", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := api.CheckBindAllowed(bindIPFor(tc.bind), tc.authConfigured, tc.allowUnauthenticated)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckBindAllowed(%q, %v, %v) = %v, wantErr %v", tc.bind, tc.authConfigured, tc.allowUnauthenticated, err, tc.wantErr)
			}
		})
	}
}

func TestBindIPFor(t *testing.T) {
	if ip := bindIPFor("127.0.0.1:8484"); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bindIPFor(127.0.0.1:8484) = %v, want loopback", ip)
	}
	if ip := bindIPFor(":8484"); ip != nil {
		t.Fatalf("bindIPFor(:8484) = %v, want nil (unspecified interface)", ip)
	}
	if ip := bindIPFor("not-an-address"); ip != nil {
		t.Fatalf("bindIPFor(not-an-address) = %v, want nil", ip)
	}
}

func TestBuildServerRejectsUnknownStore(t *testing.T) {
	if _, err := buildServer(context.Background(), serveOptions{StoreKind: "carrier-pigeon"}); err == nil {
		t.Fatal("expected an error for an unknown --store value")
	}
}

func TestBuildServerRejectsMissingRegistryFile(t *testing.T) {
	if _, err := buildServer(context.Background(), serveOptions{StoreKind: "memory", Registry: "/nonexistent/clusters.json"}); err == nil {
		t.Fatal("expected an error for a missing --registry file")
	}
}
