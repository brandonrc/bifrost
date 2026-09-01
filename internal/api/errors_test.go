package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteError_UnwrappedErrorNeverLeaksIntoBody is the fix-round-1 I1
// regression test: a plain (non-HTTPError) error's text must never reach
// the client — it may carry backend detail (here, a fake DSN with
// credentials) — but MUST be visible server-side in the log, with
// request context for correlation.
func TestWriteError_UnwrappedErrorNeverLeaksIntoBody(t *testing.T) {
	buf := captureLogs(t)
	const secret = "dsn=postgres://user:hunter2@db.internal/prod"
	err := fmt.Errorf("connect: %s", secret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	rec := httptest.NewRecorder()
	WriteError(rec, req, err)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response body leaked error detail: %s", rec.Body.String())
	}
	var body ErrorResponse
	if decErr := json.Unmarshal(rec.Body.Bytes(), &body); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if body.Error != "internal_error" {
		t.Errorf("error code = %q, want internal_error", body.Error)
	}
	if body.Message != "" {
		t.Errorf("message = %q, want empty — no detail should reach the client", body.Message)
	}

	logged := buf.String()
	if !strings.Contains(logged, secret) {
		t.Errorf("log output should contain the error detail, got: %s", logged)
	}
	if !strings.Contains(logged, "/api/v1/clusters") {
		t.Errorf("log output should contain the request path, got: %s", logged)
	}
}

// TestWriteError_HTTPErrorMessageIsNotSuppressed confirms the fix didn't
// overcorrect: an HTTPError's own (developer-authored, always-safe)
// message still renders in the body.
func TestWriteError_HTTPErrorMessageIsNotSuppressed(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), ErrNotImplemented)

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Message == "" {
		t.Error("HTTPError message should still be rendered")
	}
}
