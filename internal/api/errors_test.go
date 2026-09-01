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

// TestPointerHTTPErrorStillRenders is the fix-round-2 regression test:
// HTTPError was pointer-typed (&HTTPError{...}) until shortly before this
// package's first review round, so a handler written against that earlier
// shape — exactly what T11/T12's fan-out is about to do at scale — must
// still get its own status and message, not a flattened generic 500 from
// errors.As missing a *HTTPError in the chain when only a value target is
// checked.
func TestPointerHTTPErrorStillRenders(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), &HTTPError{
		Status:  http.StatusNotFound,
		Code:    "cluster_not_found",
		Message: "no cluster with that id",
	})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a *HTTPError must not flatten to 500)", rec.Code)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "cluster_not_found" {
		t.Errorf("error code = %q, want cluster_not_found", body.Error)
	}
	if body.Message != "no cluster with that id" {
		t.Errorf("message = %q, want %q", body.Message, "no cluster with that id")
	}
}

// TestWrappedPointerHTTPErrorStillRenders is the same property one level
// deeper: a *HTTPError reached through an Unwrap chain (fmt.Errorf's %w)
// must be found too, since errors.As walks the chain regardless of which
// form — value or pointer — sits at the point a handler wrapped it.
func TestWrappedPointerHTTPErrorStillRenders(t *testing.T) {
	inner := &HTTPError{Status: http.StatusConflict, Code: "already_suspended", Message: "cluster is already suspended"}
	wrapped := fmt.Errorf("suspend cluster c-1: %w", inner)

	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), wrapped)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "already_suspended" {
		t.Errorf("error code = %q, want already_suspended", body.Error)
	}
}
