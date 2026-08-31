package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ErrorResponse is the canonical JSON error body for the Bifrost control
// plane. mobula-api never formalized one struct for this — every handler
// built its own `serde_json::json!({"error": "<code>", ...})` literal
// (e.g. clusters.rs's "illegal_state_transition", local_auth.rs's
// "invalid_credentials") — but they all agree on the same core shape: a
// short machine-readable "error" code, optionally elaborated. This type
// is that shape, formalized once so every response — the 501 stubs here
// and the real handlers Wave 1 T11/T12 add — emits it consistently.
type ErrorResponse struct {
	// Error is a short, stable, machine-readable snake_case code (e.g.
	// "not_implemented", "invalid_credentials").
	Error string `json:"error"`
	// Message is an optional human-readable elaboration. Omitted when
	// the code is self-explanatory.
	Message string `json:"message,omitempty"`
}

// HTTPError is an error that knows the status code and canonical body it
// should render as. Handlers return one (instead of writing to the
// ResponseWriter directly) so the rendering stays centralized in
// WriteError.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// ErrNotImplemented is returned by every strict-server operation this
// wave hasn't ported a real handler for yet. Wave 1 T11/T12 burn this
// list down to zero, method by method; until then the generated
// interface still compiles complete (ADR-0002) and every unimplemented
// route answers 501 with the canonical envelope rather than a bare
// panic or an inconsistent ad hoc body.
var ErrNotImplemented = &HTTPError{
	Status:  http.StatusNotImplemented,
	Code:    "not_implemented",
	Message: "this operation is not yet implemented",
}

// WriteError renders err as the canonical JSON ErrorResponse envelope.
// A *HTTPError contributes its own status/code/message; any other error
// renders as 500 "internal_error" with err.Error() as the message so
// nothing is ever silently swallowed. Wired as the strict-server's
// ResponseErrorHandlerFunc (see NewHandler) and reusable directly by
// future handlers that want to hand back a typed failure.
func WriteError(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusInternalServerError
	body := ErrorResponse{Error: "internal_error", Message: err.Error()}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Status
		body = ErrorResponse{Error: httpErr.Code, Message: httpErr.Message}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
