package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// ErrorResponse is the canonical JSON error body for the Bifrost control
// plane. The Rust predecessor never formalized one struct for this — every handler
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
// WriteError. It is a value type deliberately: every HTTPError in this
// package (the sentinels below and every literal constructed inline) is
// copied into the `error` interface at the point it's returned, so no two
// callers — including concurrent requests hitting the same sentinel —
// can ever alias, and therefore mutate, the same underlying struct.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e HTTPError) Error() string {
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
var ErrNotImplemented = HTTPError{
	Status:  http.StatusNotImplemented,
	Code:    "not_implemented",
	Message: "this operation is not yet implemented",
}

// WriteError renders err as the canonical JSON ErrorResponse envelope. An
// HTTPError contributes its own status/code/message verbatim — those are
// always fixed, developer-authored strings, safe to hand a client. Any
// other error is NOT rendered into the response: its text may carry
// backend detail (a DB DSN, a file path, a dependency's internal error
// string) a client has no business seeing, so it is logged server-side
// instead (with the request's method/path for correlation) and the
// client gets a fixed, generic 500 body with no message at all. Wired as
// the strict-server's ResponseErrorHandlerFunc (see NewHandler) and
// reusable directly by future handlers that want to hand back a typed
// failure.
//
// Matches both the value HTTPError this package now sentinels
// (ErrNotImplemented et al.) AND a *HTTPError: HTTPError was pointer-typed
// until shortly before this package's first review round, so a habitual
// `&HTTPError{...}` from a handler ported against that earlier shape is
// exactly the kind of call site T11/T12's fan-out will keep writing —
// errors.As with only a value target would silently miss it and flatten a
// deliberate, typed status+message into a generic 500.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var httpErr HTTPError
	var ptrErr *HTTPError
	switch {
	case errors.As(err, &httpErr):
		renderHTTPError(w, httpErr)
	case errors.As(err, &ptrErr) && ptrErr != nil:
		renderHTTPError(w, *ptrErr)
	default:
		attrs := []any{"error", err}
		if r != nil {
			attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
		}
		slog.Error("api: unhandled error", attrs...)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Error: "internal_error"})
	}
}

func renderHTTPError(w http.ResponseWriter, httpErr HTTPError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpErr.Status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: httpErr.Code, Message: httpErr.Message})
}
