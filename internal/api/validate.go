package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

var (
	contractOnce sync.Once
	contractDoc  *openapi3.T
	contractRt   routers.Router
)

// ContractDocument is the embedded frozen contract, loaded once. Servers is
// cleared so route matching ignores host.
func ContractDocument() *openapi3.T {
	loadContract()
	return contractDoc
}

func loadContract() {
	contractOnce.Do(func() {
		doc, err := openapi3.NewLoader().LoadFromData(contractJSON)
		if err != nil {
			panic("api: embedded openapi.json does not load: " + err.Error())
		}
		doc.Servers = nil
		rt, err := gorillamux.NewRouter(doc)
		if err != nil {
			panic("api: contract router: " + err.Error())
		}
		contractDoc, contractRt = doc, rt
	})
}

// ValidateRequests enforces the contract — `required`, types, enums, path
// and query parameters — for every operation the contract defines, in one
// place, before any handler runs. Paths the contract does not define (the
// spec document itself, /docs, gateway hosts) pass through untouched: the
// mux behind us owns their 404s.
//
// This exists because on 2026-09-02 a create with no `id` and no `spec`
// returned 201 and persisted an empty-id record that no route could delete:
// Go's encoding/json zero-fills missing fields, and `required` was enforced
// only where a handler happened to hand-check.
func ValidateRequests(next http.Handler) http.Handler {
	loadContract()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, params, err := contractRt.FindRoute(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		input := &openapi3filter.RequestValidationInput{
			Request:    r,
			PathParams: params,
			Route:      route,
			Options: &openapi3filter.Options{
				// Auth is RequireAuth's job, one layer out; do not re-check here.
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		}
		if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
			WriteError(w, r, HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()})
			return
		}
		next.ServeHTTP(w, r)
	})
}
