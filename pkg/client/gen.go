// Package client is the Go client for the Bifrost REST API, generated from
// the same contract (internal/api/openapi.json, the source of truth per
// ADR-0006) the server is generated from, so the two cannot disagree without
// CI's codegen-drift step noticing. It is public: requirement tests use it in place of any internal
// type, exactly as an external consumer would.
package client

// The -response-type-suffix HTTPResponse flag is required to avoid name
// collisions between oapi-codegen's generated client response wrapper types
// and the schema types already defined in openapi.json. Without it,
// LoginResponse, ProvidersResponse, CreateTokenResponse, and IdentityResponse
// would be redeclared, causing compilation to fail. The suffix ensures client
// response wrappers are named LoginHTTPResponse, etc., distinct from the
// schema types.
//
//go:generate go tool oapi-codegen -generate client,types -package client -response-type-suffix HTTPResponse -o zz_generated_client.go ../../internal/api/openapi.json
