// Package api is the generated + hand-written HTTP surface of the Bifrost
// control plane, spec-first from the frozen contract vendored at
// openapi.json (github.com/brandonrc/bifrost-api).
//
// zz_generated_api.go is produced by oapi-codegen from openapi.json and
// MUST NOT be hand-edited; regenerate with `go generate ./internal/api/...`
// and commit the result. CI diffs a fresh regeneration against the
// committed file — drift fails the build (see ADR-0002).
package api

//go:generate go tool oapi-codegen -generate types,std-http,strict-server -package api -o zz_generated_api.go openapi.json
