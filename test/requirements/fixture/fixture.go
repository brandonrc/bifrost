// Package fixture holds the small vocabulary requirement tests share for
// driving clusters through the public contract: a canonical ClusterSpec,
// create/get/wait helpers, and a raw-HTTP escape hatch for the few requests
// the typed client cannot express (a login as an ad-hoc user, a malformed
// body). Nothing here imports internal/: fixture speaks the same contract a
// user does.
package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
)

// RayImage is the image clusters are provisioned with. It must carry the
// same Ray version as ClusterBody's ray_version (2.56.0) — Ray Client
// refuses mismatched versions — and it must ship `wget`, because KubeRay's
// default probes shell out to it (docs/defects/2026-09-02-health-probes-assume-wget.md).
func RayImage() string {
	if v := os.Getenv("REQ_RAY_IMAGE"); v != "" {
		return v
	}
	return "rayproject/ray:2.56.0"
}

// ClusterBody is the canonical minimal cluster: one 1-CPU head, one 1-CPU
// worker. ttl nil = no max-age reaper.
func ClusterBody(id, project string, ttl *int64) client.CreateClusterJSONRequestBody {
	ttlJSON := "null"
	if ttl != nil {
		ttlJSON = fmt.Sprint(*ttl)
	}
	raw := fmt.Sprintf(`{"id":%q,"spec":{"name":%q,"project":%q,"ray_version":"2.56.0","image":%q,
		"head_cpu":"1","head_memory":"2Gi","ttl_seconds":%s,
		"worker_groups":[{"name":"w","cpu":"1","memory":"2Gi","gpu":null,"min_replicas":1,"max_replicas":1,"replicas":1}]}}`,
		id, id, project, RayImage(), ttlJSON)
	var body client.CreateClusterJSONRequestBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		panic("fixture: ClusterBody: " + err.Error())
	}
	return body
}

// Create posts ClusterBody as principal and returns the status and body.
func Create(t req.T, tgt req.Target, principal, id, project string, ttl *int64) (int, []byte) {
	t.Helper()
	resp, err := tgt.As(principal).API().CreateClusterWithResponse(context.Background(), ClusterBody(id, project, ttl))
	if err != nil {
		t.Fatalf("create %s as %s: %v", id, principal, err)
	}
	return resp.StatusCode(), resp.Body
}

// MustCreate is Create asserting 201.
func MustCreate(t req.T, tgt req.Target, principal, id, project string) {
	t.Helper()
	if st, body := Create(t, tgt, principal, id, project, nil); st != http.StatusCreated {
		t.Fatalf("create %s as %s = %d %s, want 201", id, principal, st, body)
	}
}

// Get fetches a cluster view as principal. view is nil unless status is 200.
func Get(t req.T, tgt req.Target, principal, id string) (int, map[string]any) {
	t.Helper()
	resp, err := tgt.As(principal).API().GetClusterWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s as %s: %v", id, principal, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return resp.StatusCode(), nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("get %s: unmarshal: %v", id, err)
	}
	return resp.StatusCode(), m
}

// State reads (desired, observed_state) from a cluster view; "" when unset.
func State(view map[string]any) (desired, observed string) {
	desired, _ = view["desired"].(string)
	observed, _ = view["observed_state"].(string)
	return desired, observed
}

// WaitObserved polls until observed_state == want within the lane budget.
func WaitObserved(t req.T, tgt req.Target, principal, id, want string) {
	t.Helper()
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := Get(t, tgt, principal, id)
		if st != http.StatusOK {
			return false, fmt.Sprintf("get=%d", st)
		}
		d, o := State(v)
		return o == want, fmt.Sprintf("desired=%s observed=%s", d, o)
	})
}

// WaitGone polls until the cluster is 404 or desired=terminated.
func WaitGone(t req.T, tgt req.Target, principal, id string) {
	t.Helper()
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := Get(t, tgt, principal, id)
		if st == http.StatusNotFound {
			return true, "404"
		}
		if st != http.StatusOK {
			return false, fmt.Sprintf("get=%d", st)
		}
		d, o := State(v)
		return d == "terminated", fmt.Sprintf("desired=%s observed=%s", d, o)
	})
}

// Delete deletes as principal and returns the status.
func Delete(t req.T, tgt req.Target, principal, id string) int {
	t.Helper()
	resp, err := tgt.As(principal).API().DeleteClusterWithResponse(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("delete %s as %s: %v", id, principal, err)
	}
	return resp.StatusCode()
}

// TTL is a max-age short enough to observe the reaper inside the lane
// budget: seconds on inproc (fake provisioner, 25 ms ticks), a minute on a
// real cluster.
func TTL(tgt req.Target) int64 {
	if _, ok := tgt.K8s(); ok {
		return 60
	}
	return 2
}

// Do sends a raw request to the target with an explicit bearer ("" = none).
// For the requests the typed client cannot express.
func Do(t req.T, tgt req.Target, token, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	r, err := http.NewRequestWithContext(context.Background(), method, tgt.BaseURL()+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := HTTPClient(tgt).Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// HTTPClient returns the target's transport when it exposes one (TLS
// posture, timeouts) and the default client otherwise.
func HTTPClient(tgt req.Target) *http.Client {
	if p, ok := tgt.(interface{ HTTPClient() *http.Client }); ok {
		return p.HTTPClient()
	}
	return http.DefaultClient
}

// Login authenticates a local user through POST /api/v1/auth/login and
// returns the bearer token, or "" with the status when refused.
func Login(t req.T, tgt req.Target, username, password string) (string, int) {
	t.Helper()
	st, b := Do(t, tgt, "", http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if st != http.StatusOK {
		return "", st
	}
	var m struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Token, st
}

// Subject returns the caller's resolved identity subject.
func Subject(t req.T, tgt req.Target, principal string) string {
	t.Helper()
	resp, err := tgt.As(principal).API().IdentityWithResponse(context.Background())
	if err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("identity as %s: err=%v status=%v", principal, err, statusOf(resp))
	}
	var m struct {
		Subject string `json:"subject"`
	}
	_ = json.Unmarshal(resp.Body, &m)
	return m.Subject
}

func statusOf(r *client.IdentityHTTPResponse) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}

// Denied reports whether a status is one of the two shapes a caller sees
// for a cluster they may not touch: 403 (forbidden) or 404 (existence
// hidden). Both are acceptable isolation; which one the server picks is
// its own contract.
func Denied(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// IDs extracts ids from a list_clusters body.
func IDs(body []byte) []string {
	var items []struct {
		Id string `json:"id"`
	}
	_ = json.Unmarshal(body, &items)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Id)
	}
	return out
}

// Contains is strings.Contains for readable assertions.
func Contains(hay, needle string) bool { return strings.Contains(hay, needle) }
