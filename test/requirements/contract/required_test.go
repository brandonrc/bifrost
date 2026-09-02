package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// dummy builds a value satisfying the schema's TYPE with every required
// property present, recursively. It is not a semantically valid object —
// it only needs to be shaped so that omitting one required field is the
// sole reason the request can be rejected for "missing".
func dummy(s *openapi3.SchemaRef) any {
	if s == nil || s.Value == nil {
		return "x"
	}
	v := s.Value
	if len(v.Enum) > 0 {
		return v.Enum[0]
	}
	switch {
	case v.Type.Is("object") || len(v.Properties) > 0:
		m := map[string]any{}
		for _, name := range v.Required {
			m[name] = dummy(v.Properties[name])
		}
		return m
	case v.Type.Is("array"):
		return []any{}
	case v.Type.Is("integer"), v.Type.Is("number"):
		if v.Min != nil {
			return *v.Min
		}
		return 1
	case v.Type.Is("boolean"):
		return true
	default:
		return "x"
	}
}

// missingPropertyReasons returns the exact reason phrasings
// openapi3filter emits for a missing required property. Two shapes were
// observed across this contract's body schemas — both bind to the field
// name precisely enough that no OTHER required field's name can satisfy
// either substring:
//
//   - a single SchemaError naming the property inline, e.g. CreateUserRequest,
//     CreateCluster, CreatePool, DeployService — `Error at "/role": property
//     "role" is missing`. These responses also carry a full JSON dump of the
//     enclosing schema (every property name, and the full `required` list) —
//     which is why a bare `strings.Contains(raw, field)` was vacuous for
//     these operations: it passed for a 400 caused by omitting ANY of the
//     schema's required fields, not specifically the one this subtest
//     omitted.
//   - a MultiError-wrapped case, e.g. LoginRequest, CreateTokenRequest,
//     UpsertAssignment, PutAllocation — `validation failed due to: at ”:
//     missing property 'role'` (single-quoted, reversed wording, no schema
//     dump — "Schema:\n  null").
func missingPropertyReasons(field string) (dumped, wrapped string) {
	return fmt.Sprintf("property %q is missing", field), fmt.Sprintf("missing property '%s'", field)
}

func TestEveryRequiredFieldIsEnforced(t *testing.T) {
	tgt := target.Get(t).As("admin")
	req.Covers(t, 3, "the contract's required fields are enforced before any handler runs")
	req.Covers(t, 18, "input validation is uniform across the API surface (NIST baseline)")

	doc := Load(t)
	checked := 0
	for _, op := range Operations(doc) {
		if op.Body == nil || op.Body.Value == nil || len(op.Body.Value.Required) == 0 {
			continue
		}
		full, _ := dummy(op.Body).(map[string]any)
		for _, field := range op.Body.Value.Required {
			field := field
			t.Run(fmt.Sprintf("%s_%s_omit_%s", op.Method, op.ID, field), func(t *testing.T) {
				partial := map[string]any{}
				for k, v := range full {
					if k != field {
						partial[k] = v
					}
				}
				body, _ := json.Marshal(partial)
				resp := Do(t, tgt.BaseURL(), tgt.Authorize, op.Method, FillPath(op.Path), string(body))
				defer func() { _ = resp.Body.Close() }()
				raw, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s %s without %q = %d, want 400; body=%s", op.Method, op.Path, field, resp.StatusCode, raw)
				}
				// The response is JSON: its "message" string is
				// backslash-escaped (e.g. \"role\"), so match against the
				// DECODED message, not the raw bytes — a literal-quote
				// pattern can never hit an escaped byte sequence.
				var envelope struct {
					Message string `json:"message"`
				}
				_ = json.Unmarshal(raw, &envelope)
				dumped, wrapped := missingPropertyReasons(field)
				if !strings.Contains(envelope.Message, dumped) && !strings.Contains(envelope.Message, wrapped) {
					snippet := raw
					if len(snippet) > 300 {
						snippet = snippet[:300]
					}
					t.Errorf("400 body should give openapi3filter's missing-property reason for %q (want %q or %q); got: %s", field, dumped, wrapped, snippet)
				}
			})
			checked++
		}
	}
	// The vendored contract currently declares 18 required-field cases
	// across 10 body operations (verified against internal/api/openapi.json
	// directly, not just this package's own walk of it). The floor below
	// sits under that so ordinary contract growth doesn't need a bump, but
	// still catches Operations() or dummy() silently losing most of the
	// contract's body operations or their required fields.
	if checked < 15 {
		t.Fatalf("only %d required-field cases generated; the contract has 18 — Operations() or dummy() lost some", checked)
	}
}
