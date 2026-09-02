package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/api"
	"github.com/brandonrc/bifrost/internal/api/apitest"
	"github.com/brandonrc/bifrost/internal/controller"
)

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The body that returned 201 on Grace on 2026-09-02 and wedged the
// reconciler with an empty id. It has no `id` and no `spec`.
func TestCreateClusterWithoutRequiredFieldsIs400(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"name":"x","engine":"ray","workers":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var e api.ErrorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if !strings.Contains(e.Message, `"id"`) && !strings.Contains(e.Message, "id") {
		t.Errorf("400 body should name the missing field; got %q", e.Message)
	}
	// Nothing must have been persisted.
	clusters, err := s.Store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Fatalf("a rejected create persisted %d cluster(s)", len(clusters))
	}
}

// Validation must not eat the body: a valid request still reaches the handler.
func TestValidRequestBodyReachesHandler(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"id":"ok-1","spec":{"name":"ok-1","project":"p","image":"i","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[]}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// Non-contract paths are not the validator's business: whatever the mux
// behind it answers (200 for the spec itself, 404 for /docs — this Go
// server mounts no swagger UI; /docs is carried in the auth allowlist only
// for a reverse-proxied one, per server.go's SpecPath doc comment), the
// validator itself must never turn a path it doesn't own into a 400.
func TestValidatorPassesThroughNonContractPaths(t *testing.T) {
	h, _ := apitest.NewServer()
	for _, p := range []string{api.SpecPath, "/docs"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("GET %s = %d; the validator must pass non-contract paths through", p, rec.Code)
		}
	}
}

// The domain guard is independent of the middleware: an id that passes the
// schema (a string) but is not a valid Kubernetes name is still refused.
func TestCreateClusterInvalidK8sNameIs400(t *testing.T) {
	s := api.NewServer()
	s.Store = controller.NewMemoryStore()
	h := apitest.NewHandler(s)
	rec := post(t, h, "/api/v1/clusters", `{"id":"Not_Valid!","spec":{"name":"x","project":"p","image":"i","ray_version":"2.56.0","head_cpu":"1","head_memory":"1Gi","worker_groups":[]}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
