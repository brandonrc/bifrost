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
				if !strings.Contains(string(raw), field) {
					t.Errorf("400 body should name the missing field %q; got %s", field, raw)
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
