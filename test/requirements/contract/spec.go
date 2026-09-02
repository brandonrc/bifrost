// Package contract holds the two generated tests: every operation, every
// required field omitted -> 400; every non-public operation, no token ->
// 401. Two tests, all 47 operations, no per-endpoint memory required.
package contract

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Public is the auth middleware's exact allowlist (internal/api's
// middleware_probes_test.go, "the Rust seven"). Anything else needs a token.
var Public = map[string]bool{
	"GET /healthz":               true,
	"GET /api/v1/version":        true,
	"GET /api/v1/auth/providers": true,
	"POST /api/v1/auth/login":    true,
}

// Op is one contract operation with what the tests need to drive it.
type Op struct {
	Method, Path, ID string
	Body             *openapi3.SchemaRef // nil when the op takes no JSON body
	Public           bool
}

// Load reads the vendored contract from the repo — the same file the server
// embeds — so these tests can never drift from what the server enforces.
func Load(t testing.TB) *openapi3.T {
	t.Helper()
	p := filepath.Join("..", "..", "..", "internal", "api", "openapi.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read contract %s: %v", p, err)
	}
	doc, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return doc
}

// Operations lists every operation, sorted for stable subtest names.
func Operations(doc *openapi3.T) []Op {
	var out []Op
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			o := Op{Method: strings.ToUpper(method), Path: path, ID: op.OperationID}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				if mt := op.RequestBody.Value.Content.Get("application/json"); mt != nil {
					o.Body = mt.Schema
				}
			}
			o.Public = Public[o.Method+" "+o.Path]
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method+out[i].Path < out[j].Method+out[j].Path })
	return out
}

// FillPath replaces {params} with a plausible value so routing succeeds.
func FillPath(p string) string {
	for _, seg := range []string{"{id}", "{name}", "{principal}", "{username}", "{prefix}", "{project}"} {
		p = strings.ReplaceAll(p, seg, "x1")
	}
	return p
}

// Do sends a raw request through the target's authenticated client base URL.
func Do(t testing.TB, base string, editor func(*http.Request), method, path, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, base+path, nil)
	} else {
		r, err = http.NewRequest(method, base+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatal(err)
	}
	if editor != nil {
		editor(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
