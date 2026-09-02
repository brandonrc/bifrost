package r18_baseline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// No secret material in any response: password hashes, token values and
// registry tokens stay server-side. A user list, a token list, a registry
// list and the audit trail are the places they would leak.
func TestNoSecretMaterialInResponses(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 18, "no API response carries a password, a password hash, a token value or a registry token")
	ctx := context.Background()
	admin := tgt.As("admin").API()
	user := req.Name("sec")
	pw := "pw-" + req.RunID() + "-secret-material"
	var create client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":"viewer"}`, user, pw)), &create)
	cr, err := admin.CreateUserWithResponse(ctx, create)
	if err != nil || cr.StatusCode()/100 != 2 {
		t.Fatalf("create user: err=%v status=%v", err, cr.StatusCode())
	}
	t.Cleanup(func() {
		var d client.UpdateUserJSONRequestBody
		_ = json.Unmarshal([]byte(`{"disabled":true}`), &d)
		_, _ = admin.UpdateUserWithResponse(context.Background(), user, d)
	})
	var tok client.CreateTokenJSONRequestBody
	_ = json.Unmarshal([]byte(`{"label":"sec","expires_in_days":1}`), &tok)
	minted, err := admin.CreateTokenWithResponse(ctx, tok)
	if err != nil || minted.StatusCode()/100 != 2 {
		t.Fatalf("create token: err=%v status=%v", err, minted.StatusCode())
	}
	var m struct {
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	_ = json.Unmarshal(minted.Body, &m)
	t.Cleanup(func() { _, _ = admin.RevokeTokenWithResponse(context.Background(), m.Prefix) })

	bodies := map[string][]byte{}
	if r, err := admin.ListUsersWithResponse(ctx); err == nil {
		bodies["list_users"] = r.Body
	}
	if r, err := admin.ListTokensWithResponse(ctx); err == nil {
		bodies["list_tokens"] = r.Body
	}
	if r, err := admin.ListRegistryWithResponse(ctx); err == nil {
		bodies["list_registry"] = r.Body
	}
	if r, err := admin.ListAuditEventsWithResponse(ctx, nil); err == nil {
		bodies["list_audit_events"] = r.Body
	}
	bodies["create_user"] = cr.Body
	for op, b := range bodies {
		s := string(b)
		for _, needle := range []string{pw, m.Token, "$2a$", "$2b$", "password_hash", "\"auth_token\":\""} {
			if needle != "" && strings.Contains(s, needle) {
				t.Errorf("%s response carries secret material (%q)", op, needle[:min(8, len(needle))])
			}
		}
	}
	if !strings.Contains(string(bodies["list_tokens"]), `"prefix"`) {
		t.Errorf("list_tokens should identify tokens by prefix: %s", bodies["list_tokens"])
	}
}

// Defect 1's audit half: a create that persists nothing valid must not be
// audited as a 201. With validation in front of the handler the refused
// request leaves no allow row for that id at all.
func TestAuditStatusMatchesOutcome(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 18, "the audit trail never records a success for a request the server refused")
	ctx := context.Background()
	id := req.Name("audit-bad")
	st, _ := fixture.Do(t, tgt, bearer(t, tgt), http.MethodPost, "/api/v1/clusters", fmt.Sprintf(`{"id":%q}`, id))
	if st != http.StatusBadRequest {
		t.Fatalf("create without spec = %d, want 400", st)
	}
	list, err := tgt.As("admin").API().ListAuditEventsWithResponse(ctx, nil)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list audit: err=%v status=%v", err, list.StatusCode())
	}
	var wrapped struct {
		Items []struct {
			Cluster  *string `json:"cluster"`
			Decision string  `json:"decision"`
			Status   *int    `json:"status"`
			Action   *string `json:"action"`
		} `json:"items"`
	}
	_ = json.Unmarshal(list.Body, &wrapped)
	for _, e := range wrapped.Items {
		if e.Cluster != nil && *e.Cluster == id && e.Decision == "allow" && e.Status != nil && *e.Status/100 == 2 {
			t.Fatalf("audit claims success for a refused create: %+v", e)
		}
	}
}

func bearer(t *testing.T, tgt req.Target) string {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tgt.BaseURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt.As("admin").Authorize(r)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
