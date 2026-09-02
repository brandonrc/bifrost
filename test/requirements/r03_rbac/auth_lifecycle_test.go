package r03_rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func poolBody(name string) client.CreatePoolJSONRequestBody {
	var b client.CreatePoolJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"spec":{"name":%q,"cohort":"req","fair_sharing_weight":1,"elastic":false,
		"flavors":[{"name":"default","resources":{"cpu":"8","memory":"32Gi"},"node_labels":{},"taints":[]}]}}`, name)), &b)
	return b
}

func userBody(name string) client.CreateUserJSONRequestBody {
	var b client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":"pw-%s-0123456789","role":"viewer"}`, name, req.RunID())), &b)
	return b
}

func disableBody() client.UpdateUserJSONRequestBody {
	var b client.UpdateUserJSONRequestBody
	_ = json.Unmarshal([]byte(`{"disabled":true}`), &b)
	return b
}

// Personal access tokens: minted by the caller, usable as a bearer, listed
// with their label, dead the moment they are revoked. This is what
// bifrost-jupyter and CI use to talk to the API.
func TestPersonalAccessTokenLifecycle(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a personal access token authenticates as its owner until revoked, then is 401")
	ctx := context.Background()
	api := tgt.As("dev-a").API()
	label := req.Name("pat")
	var body client.CreateTokenJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"label":%q,"expires_in_days":1}`, label)), &body)
	created, err := api.CreateTokenWithResponse(ctx, body)
	if err != nil || created.StatusCode()/100 != 2 {
		t.Fatalf("create token: err=%v status=%v body=%s", err, codeOf(created), bodyOf(created))
	}
	var tok struct {
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	_ = json.Unmarshal(created.Body, &tok)
	if tok.Token == "" || tok.Prefix == "" {
		t.Fatalf("token response lacks token/prefix: %s", created.Body)
	}
	st, b := fixture.Do(t, tgt, tok.Token, http.MethodGet, "/api/v1/identity", "")
	if st != http.StatusOK {
		t.Fatalf("PAT identity = %d %s", st, b)
	}
	var id struct {
		Subject string `json:"subject"`
	}
	_ = json.Unmarshal(b, &id)
	if id.Subject != fixture.Subject(t, tgt, "dev-a") {
		t.Fatalf("PAT authenticates as %q, want its owner %q", id.Subject, fixture.Subject(t, tgt, "dev-a"))
	}
	list, err := api.ListTokensWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK || !fixture.Contains(string(list.Body), label) {
		t.Fatalf("list tokens: err=%v status=%v body=%s", err, codeOf(list), bodyOf(list))
	}
	if fixture.Contains(string(list.Body), tok.Token) {
		t.Fatal("list_tokens must never echo a token value")
	}
	rev, err := api.RevokeTokenWithResponse(ctx, tok.Prefix)
	if err != nil || rev.StatusCode()/100 != 2 {
		t.Fatalf("revoke: err=%v status=%v", err, codeOf(rev))
	}
	if st, _ := fixture.Do(t, tgt, tok.Token, http.MethodGet, "/api/v1/identity", ""); st != http.StatusUnauthorized {
		t.Fatalf("revoked PAT identity = %d, want 401", st)
	}
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "logout ends the session: the bearer is 401 afterwards")
	// A fresh login so the cached principal token stays valid for other tests.
	user := req.Name("lo")
	pw := "pw-" + req.RunID() + "-logout"
	var create client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":"viewer"}`, user, pw)), &create)
	if r, err := tgt.As("admin").API().CreateUserWithResponse(context.Background(), create); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("create user: err=%v status=%v", err, codeOf(r))
	}
	t.Cleanup(func() { _, _ = tgt.As("admin").API().UpdateUserWithResponse(context.Background(), user, disableBody()) })
	tok, st := fixture.Login(t, tgt, user, pw)
	if tok == "" {
		t.Fatalf("login = %d", st)
	}
	if st, _ := fixture.Do(t, tgt, tok, http.MethodGet, "/api/v1/identity", ""); st != http.StatusOK {
		t.Fatalf("identity before logout = %d", st)
	}
	if st, b := fixture.Do(t, tgt, tok, http.MethodPost, "/api/v1/auth/logout", ""); st/100 != 2 {
		t.Fatalf("logout = %d %s", st, b)
	}
	if st, _ := fixture.Do(t, tgt, tok, http.MethodGet, "/api/v1/identity", ""); st != http.StatusUnauthorized {
		t.Fatalf("identity after logout = %d, want 401", st)
	}
}

func TestDisabledUserCannotLogInAndWrongPasswordIs401(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "a disabled local user cannot log in; a wrong password is 401 without revealing which")
	user := req.Name("dis")
	pw := "pw-" + req.RunID() + "-disabled"
	var create client.CreateUserJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":"viewer"}`, user, pw)), &create)
	if r, err := tgt.As("admin").API().CreateUserWithResponse(context.Background(), create); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("create user: err=%v status=%v", err, codeOf(r))
	}
	if _, st := fixture.Login(t, tgt, user, "definitely-not-"+pw); st != http.StatusUnauthorized {
		t.Fatalf("wrong password login = %d, want 401", st)
	}
	if _, st := fixture.Login(t, tgt, "no-such-user-"+req.RunID(), pw); st != http.StatusUnauthorized {
		t.Fatalf("unknown user login = %d, want 401 (same answer as a wrong password)", st)
	}
	if r, err := tgt.As("admin").API().UpdateUserWithResponse(context.Background(), user, disableBody()); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("disable: err=%v status=%v", err, codeOf(r))
	}
	if _, st := fixture.Login(t, tgt, user, pw); st != http.StatusUnauthorized && st != http.StatusForbidden {
		t.Fatalf("disabled user login = %d, want 401/403", st)
	}
}
