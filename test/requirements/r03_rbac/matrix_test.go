package r03_rbac

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/brandonrc/bifrost/test/requirements/contract"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

//go:embed permissions.yaml
var permissionsYAML []byte

type matrix struct {
	Operations map[string]map[string]string `yaml:"operations"`
}

// TestPermissionMatrix is the spec's "generated role x operation matrix":
// permissions.yaml states, for every non-public operation in the frozen
// contract, what each seeded principal may do, and this test asserts the
// server agrees. Drift in either direction fails: an operation the file
// does not list, a new principal outcome the file did not predict, or an
// authorization change nobody wrote down here.
func TestPermissionMatrix(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 3, "every non-public operation answers each role exactly as permissions.yaml states; drift in either direction fails")
	ctx := context.Background()
	var m matrix
	if err := yaml.Unmarshal(permissionsYAML, &m); err != nil {
		t.Fatal(err)
	}

	// Fixtures the path parameters and bodies point at: a cluster owned by
	// team-a, a pool, a user and an assignment principal, all run-prefixed.
	clusterID := req.Name("mx")
	fixture.MustCreate(t, tgt, "admin", clusterID, "team-a")
	poolName := req.Name("mxpool")
	adminAPI := tgt.As("admin").API()
	if r, err := adminAPI.CreatePoolWithResponse(ctx, poolBody(poolName)); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("fixture pool: err=%v status=%v body=%s", err, codeOf(r), bodyOf(r))
	}
	t.Cleanup(func() { _, _ = adminAPI.DeletePoolWithResponse(context.Background(), poolName) })
	userName := req.Name("mxuser")
	if r, err := adminAPI.CreateUserWithResponse(ctx, userBody(userName)); err != nil || r.StatusCode()/100 != 2 {
		t.Fatalf("fixture user: err=%v status=%v", err, codeOf(r))
	}
	t.Cleanup(func() {
		_, _ = adminAPI.UpdateUserWithResponse(context.Background(), userName, disableBody())
		_, _ = adminAPI.DeleteAssignmentWithResponse(context.Background(), userName, nil)
	})

	// Path parameters: the contract's placeholders → this run's objects.
	// `{id}` is a cluster id on /clusters/... and a job id on /jobs/...; the
	// job is never created (submit_job is destructive-if-allowed), so its
	// id resolves to 404 for every principal that gets past authorization —
	// "not 401/403" for allow and dev-a, Denied for dev-b.
	jobID := req.Name("mxjob")
	fill := func(p string) string {
		if strings.HasPrefix(p, "/api/v1/jobs/") {
			p = strings.ReplaceAll(p, "{id}", jobID)
		}
		p = strings.ReplaceAll(p, "{id}", clusterID)
		p = strings.ReplaceAll(p, "{name}", poolName)
		p = strings.ReplaceAll(p, "{project}", "team-a")
		p = strings.ReplaceAll(p, "{principal}", userName)
		p = strings.ReplaceAll(p, "{username}", userName)
		p = strings.ReplaceAll(p, "{prefix}", "nosuchprefix")
		p = contract.FillPath(p)
		if strings.HasSuffix(p, "/access/assignments/"+userName) {
			// delete_assignment requires ?role=&scope=; without them validation
			// answers 400 before authorization gets a say.
			p += "?role=viewer&scope=*"
		}
		return p
	}
	// Bodies for operations that take one: well-formed, so the answer is
	// authorization, never validation.
	bodies := map[string]func() string{
		"create_cluster": func() string {
			b, _ := json.Marshal(fixture.ClusterBody(req.Name("mxc"), "team-a", nil))
			return string(b)
		},
		"create_token":      func() string { return `{"label":"matrix","expires_in_days":1}` },
		"create_user":       func() string { b, _ := json.Marshal(userBody(req.Name("mxu2"))); return string(b) },
		"update_user":       func() string { return `{"disabled":false}` },
		"upsert_assignment": func() string { return `{"role":"viewer","scope":"*"}` },
		"create_pool":       func() string { b, _ := json.Marshal(poolBody(req.Name("mxp2"))); return string(b) },
		"put_allocation": func() string {
			return `{"namespace":"bifrost","nominal":{"cpu":"1"},"borrowing_limit":{},"lending_limit":{}}`
		},
		"update_policy":  func() string { return `{}` },
		"deploy_service": func() string { return serviceBody(req.Name("mxsvc")) },
		"submit_job":     func() string { return jobBody(req.Name("mxj2")) },
	}
	// Operations whose success would mutate shared state in a way another
	// test would notice are not exercised against a principal expected to be
	// allowed; the deny side still is.
	destructiveIfAllowed := map[string]bool{"delete_cluster": true, "suspend_cluster": true, "resume_cluster": true, "logout": true, "delete_pool": true, "delete_allocation": true, "delete_service": true, "submit_job": true, "create_pool": true, "create_cluster": true, "deploy_service": true, "create_user": true, "upsert_assignment": true, "update_policy": true, "revoke_token": true, "update_user": true, "delete_assignment": true}

	doc := contract.Load(t)
	seen := map[string]bool{}
	for _, op := range contract.Operations(doc) {
		if op.Public {
			continue
		}
		expect, ok := m.Operations[op.ID]
		if !ok {
			t.Errorf("permissions.yaml has no row for %s (%s %s): every non-public operation must be classified", op.ID, op.Method, op.Path)
			continue
		}
		seen[op.ID] = true
		for _, principal := range []string{"admin", "operator", "dev-a", "dev-b"} {
			want := expect[principal]
			if want == "" {
				t.Errorf("permissions.yaml: %s has no outcome for %s", op.ID, principal)
				continue
			}
			if want == "allow" && destructiveIfAllowed[op.ID] {
				continue
			}
			body := ""
			if op.Body != nil {
				if mk, ok := bodies[op.ID]; ok {
					body = mk()
				} else {
					body = "{}"
				}
			}
			p := tgt.As(principal)
			resp := contract.Do(t, tgt.BaseURL(), p.Authorize, op.Method, fill(op.Path), body)
			_ = resp.Body.Close()
			got := resp.StatusCode
			switch want {
			case "allow":
				if got == http.StatusUnauthorized || got == http.StatusForbidden {
					t.Errorf("%s as %s = %d, permissions.yaml says allow", op.ID, principal, got)
				}
			case "deny":
				if got != http.StatusForbidden {
					t.Errorf("%s as %s = %d, permissions.yaml says deny (403)", op.ID, principal, got)
				}
			case "scoped":
				// team-a's cluster: dev-a is in scope, dev-b is not.
				if principal == "dev-a" && (got == http.StatusUnauthorized || got == http.StatusForbidden) {
					t.Errorf("%s as dev-a (own project) = %d, permissions.yaml says scoped", op.ID, got)
				}
				if principal == "dev-b" && !fixture.Denied(got) {
					t.Errorf("%s as dev-b (other project) = %d, permissions.yaml says scoped (403/404)", op.ID, got)
				}
			default:
				t.Errorf("permissions.yaml: %s/%s has unknown outcome %q", op.ID, principal, want)
			}
		}
	}
	for id := range m.Operations {
		if !seen[id] {
			t.Errorf("permissions.yaml lists %s, which the contract does not have", id)
		}
	}
	if len(seen) < 40 {
		t.Fatalf("only %d operations classified; the contract has 51 (3 public)", len(seen))
	}
}

func codeOf(r interface{ StatusCode() int }) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}

func bodyOf(r interface{ GetBody() []byte }) string {
	if r == nil {
		return ""
	}
	return string(r.GetBody())
}

func serviceBody(name string) string {
	return fmt.Sprintf(`{"name":%q,"spec":{"name":%q,"project":"team-a","ray_version":"2.56.0","image":"rayproject/ray:2.56.0",
	  "serve_config_v2":"applications: []\n","head_cpu":"1","head_memory":"2Gi","worker_replicas":0,"worker_cpu":"1","worker_memory":"2Gi","upgrade":"in_place"}}`, name, name)
}

func jobBody(id string) string {
	return fmt.Sprintf(`{"id":%q,"spec":{"project":"team-a","entrypoint":"python -c 1","image":%q}}`, id, fixture.RayImage())
}
