// Package apitest provides dev-mode constructors for internal/api that
// bypass authentication, mirroring the Rust predecessor's `#[cfg(any(test,
// feature = "test-util"))] build_router()`/`build_app()` family
// (lib.rs): "defaults validator = None, which bypasses auth, so it must
// never be reachable in a production build."
//
// Go has no cfg-gated compilation for this, so the guard is a lint
// instead: a depguard rule in .golangci.yml denies importing this
// package from any non-`_test.go` file. Import it ONLY from tests.
package apitest

import (
	"net/http"

	"github.com/brandonrc/bifrost/internal/api"
)

// NewHandler builds a Bifrost API http.Handler with no validator and no
// local authenticator configured, and AllowUnauthenticated forced on, so
// api.RequireAuth passes every request through and the fail-closed
// non-loopback guard is never installed. This is the auth bypass
// the Rust predecessor gated behind `test-util` (#45) — for tests exercising
// handlers, not the auth middleware itself.
func NewHandler(server api.StrictServerInterface) http.Handler {
	return api.NewHandler(server, api.HandlerOptions{AllowUnauthenticated: true})
}

// NewServer is a convenience wrapper for tests that just want the
// skeleton api.Server (healthz/version live, everything else 501) behind
// an unauthenticated handler.
func NewServer() (http.Handler, *api.Server) {
	s := api.NewServer()
	return NewHandler(s), s
}
