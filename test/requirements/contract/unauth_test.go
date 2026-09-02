package contract

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestEveryNonPublicOperationRequiresAToken(t *testing.T) {
	tgt := target.Get(t).As("anon")
	req.Covers(t, 3, "deny-by-default: every non-public operation answers 401 without a token")

	doc := Load(t)
	seen := 0
	for _, op := range Operations(doc) {
		if op.Public {
			continue
		}
		op := op
		t.Run(fmt.Sprintf("%s_%s", op.Method, op.ID), func(t *testing.T) {
			body := ""
			if op.Body != nil {
				body = "{}"
			}
			resp := Do(t, tgt.BaseURL(), nil, op.Method, FillPath(op.Path), body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with no token = %d, want 401", op.Method, op.Path, resp.StatusCode)
			}
		})
		seen++
	}
	if seen < 40 {
		t.Fatalf("only %d non-public operations exercised; the contract has 47 total", seen)
	}
}
