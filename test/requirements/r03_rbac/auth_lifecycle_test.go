package r03_rbac

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

// A caller's identity names the projects they hold grants in. Requirement 3 is
// about knowing who may do what; this is the half a *client* needs, because a
// client that has to name a project and cannot ask will guess one — which is
// exactly how the JupyterLab extension shipped a hardcoded default and
// answered its users' first Start click with a 403 (bifrost-jupyter#3).
func TestIdentityNamesTheCallersProjects(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "the caller's identity names the projects they hold scoped grants in, so a client never has to guess one")

	st, body := fixture.Do(t, tgt, bearerFor(t, tgt, "dev-a"), http.MethodGet, "/api/v1/identity", "")
	if st != http.StatusOK {
		t.Fatalf("identity = %d %s", st, body)
	}
	var view struct {
		Subject  string   `json:"subject"`
		Roles    []string `json:"roles"`
		Projects []struct {
			Name  string   `json:"name"`
			Roles []string `json:"roles"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("identity body: %v (%s)", err, body)
	}
	// dev-a is seeded with operator on team-a, which is what lets it create
	// clusters there; identity has to say so.
	var found bool
	for _, p := range view.Projects {
		if p.Name != "team-a" {
			continue
		}
		found = true
		if !fixture.Contains(strings.Join(p.Roles, ","), "operator") {
			t.Errorf("team-a roles = %v, want operator among them", p.Roles)
		}
	}
	if !found {
		t.Fatalf("identity names %+v, want team-a among them (roles=%v)", view.Projects, view.Roles)
	}

	// And it is the caller's own scopes, not everyone's: dev-b's project is
	// not dev-a's to see.
	for _, p := range view.Projects {
		if p.Name == "team-b" {
			t.Errorf("dev-a's identity names team-b: %+v", view.Projects)
		}
	}

}

// bearerFor is the raw token for principal, for the routes the generated
// client does not model.
func bearerFor(t *testing.T, tgt req.Target, principal string) string {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tgt.BaseURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt.As(principal).Authorize(r)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
