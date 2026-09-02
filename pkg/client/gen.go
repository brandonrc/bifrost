// Package client is the Go client for the Bifrost REST API, generated from
// the same vendored contract (internal/api/openapi.json) the server is
// generated from, so the two cannot disagree without CI's codegen-drift step
// noticing. It is public: requirement tests use it in place of any internal
// type, exactly as an external consumer would.
package client

//go:generate go tool oapi-codegen -generate client,types -package client -response-type-suffix HTTPResponse -o zz_generated_client.go ../../internal/api/openapi.json
