package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandonrc/bifrost/internal/controller"
)

func TestNewServesHealthzAndVersion(t *testing.T) {
	a, err := New(Config{Store: controller.NewMemoryStore(), AllowUnauthenticated: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(a.Handler)
	defer srv.Close()
	for _, p := range []string{"/healthz", "/api/v1/version"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with nil Store returned no error")
	}
}
