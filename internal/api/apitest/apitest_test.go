package apitest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewServerBypassesAuth(t *testing.T) {
	h, _ := NewServer()

	// No Authorization header at all — a real deployment would deny
	// this on any route but the public allowlist; apitest exists so
	// handler tests don't have to care.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: status = %d, want 200", rec.Code)
	}
}
