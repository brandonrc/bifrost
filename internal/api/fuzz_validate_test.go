package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/internal/api"
)

// FuzzValidateRequests drives the OpenAPI request-validation middleware
// (kin-openapi) with arbitrary method/path/query/body combinations.
// Invariants: no panic; the verdict is always a clean allow (the request
// reaches the inner handler, 200) or a 400-class refusal — never a 5xx,
// never a hang. Sizes are capped to keep per-iteration cost low.
func FuzzValidateRequests(f *testing.F) {
	const allowed = http.StatusOK
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(allowed)
	})
	h := api.ValidateRequests(next)

	const validBody = `{"id":"ok-1","spec":{"name":"ok-1","project":"p","image":"i","ray_version":"2.56.0","head_cpu":"1","head_memory":"2Gi","worker_groups":[]}}`
	for _, s := range [][5]string{
		{http.MethodPost, "/api/v1/clusters", "", "application/json", validBody},
		{http.MethodPost, "/api/v1/clusters", "", "application/json; charset=utf-8", validBody},
		{http.MethodPost, "/api/v1/clusters", "", "text/plain", validBody},
		{http.MethodPost, "/api/v1/clusters", "", "", `{"name":"x","engine":"ray","workers":1}`},
		{http.MethodPost, "/api/v1/clusters", "", "application/json", `{"id":"Not_Valid!","spec":{}}`},
		{http.MethodPost, "/api/v1/clusters", "", "application/json", `{`},
		{http.MethodPost, "/api/v1/clusters", "", "application/json", ``},
		{http.MethodGet, "/api/v1/clusters", "", "", ""},
		{http.MethodGet, "/api/v1/clusters/ok-1", "", "", ""},
		{http.MethodDelete, "/api/v1/clusters/ok-1", "", "", ""},
		{http.MethodGet, "/api/v1/clusters", "limit=1&cursor=abc", "", ""},
		{http.MethodGet, "/api/v1/clusters", "limit=notanumber", "", ""},
		{http.MethodGet, api.SpecPath, "", "", ""},
		{http.MethodGet, "/docs", "", "", ""},
		{http.MethodGet, "/no/such/route", "", "", ""},
		{http.MethodGet, "/api/v1/clusters/%2e%2e", "", "", ""},
		{"\x00\x01", "/api/v1/clusters", "", "", ""},
		{http.MethodPost, "/api/v1/clusters", "", "application/json", strings.Repeat("{}", 32)},
		{http.MethodGet, "//api/v1//clusters", "", "", ""},
		{http.MethodGet, "/api/v1/clusters", "\x00=%ff&%ff=\x00", "", ""},
	} {
		f.Add(s[0], s[1], s[2], s[3], s[4])
	}

	f.Fuzz(func(t *testing.T, method, path, rawQuery, contentType, body string) {
		// Bound per-iteration cost.
		if len(path) > 512 {
			path = path[:512]
		}
		if len(rawQuery) > 1024 {
			rawQuery = rawQuery[:1024]
		}
		if len(body) > 4096 {
			body = body[:4096]
		}
		u := &url.URL{Path: path, RawQuery: rawQuery}
		// Let percent-encoded inputs reach the router in encoded form.
		if dec, err := url.PathUnescape(path); err == nil && dec != path {
			u.Path = dec
			u.RawPath = path
		}
		req := &http.Request{
			Method: method,
			URL:    u,
			Header: http.Header{},
			Body:   io.NopCloser(strings.NewReader(body)),
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != allowed && (rec.Code < 400 || rec.Code >= 500) {
			t.Fatalf("verdict %d for %q %q (want allow=%d or 400-class)", rec.Code, method, path, allowed)
		}
	})
}
