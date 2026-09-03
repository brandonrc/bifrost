// Package api is the generated + hand-written HTTP surface of the Bifrost
// control plane, spec-first from the contract at openapi.json — which is
// the source of truth for the Bifrost REST API (ADR-0006). It is edited
// here, in the same PR as the handler that implements the change, and
// published downstream to github.com/brandonrc/bifrost-api (the SDK
// pipeline's home) by .github/workflows/sync-api.yml on every push to main.
//
// zz_generated_api.go is produced by oapi-codegen from openapi.json and
// MUST NOT be hand-edited; regenerate with `go generate ./internal/api/...`
// and commit the result. CI diffs a fresh regeneration against the
// committed file — drift fails the build (see ADR-0002).
package api

import _ "embed"

//go:generate go tool oapi-codegen -generate types,std-http,strict-server -package api -o zz_generated_api.go openapi.json

//go:embed openapi.json
var contractJSON []byte
