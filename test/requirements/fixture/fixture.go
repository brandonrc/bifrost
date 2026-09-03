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
	"net/url"
	"os"
	"strconv"
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

// WorkerReplicas is how many workers the canonical cluster asks for:
// REQ_WORKER_REPLICAS, default 1. The kind lane sets 0 — a 4-vCPU runner
// cannot fit two head+worker clusters at 1 CPU each next to Calico,
// KubeRay and Kueue, and the requirements under test are about the head
// (its Service, its NetworkPolicy, its Jobs API), not about workers.
func WorkerReplicas() int {
	if v := os.Getenv("REQ_WORKER_REPLICAS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			panic("fixture: REQ_WORKER_REPLICAS must be a non-negative integer, got " + v)
		}
		return n
	}
	return 1
}

// HeadMemory is the head's memory request/limit for every canonical body.
// REQ_HEAD_MEMORY overrides the 2Gi default: on the head-only kind layout
// (REQ_WORKER_REPLICAS=0) the head also runs the job supervisor and Ray's
// memory monitor OOM-kills it at 95% of a 2Gi cgroup, so the kind lane
// asks for more.
func HeadMemory() string {
	if v := os.Getenv("REQ_HEAD_MEMORY"); v != "" {
		return v
	}
	return "2Gi"
}

// ClusterBody is the canonical minimal cluster: one 1-CPU head and
// WorkerReplicas() 1-CPU workers. ttl nil = no max-age reaper.
func ClusterBody(id, project string, ttl *int64) client.CreateClusterJSONRequestBody {
	return ClusterBodyWithImage(id, project, RayImage(), ttl)
}

// ClusterBodyWithImage is ClusterBody with an explicit image, for tests
// about what the image does or does not ship (defect 2: no wget).
func ClusterBodyWithImage(id, project, image string, ttl *int64) client.CreateClusterJSONRequestBody {
	ttlJSON := "null"
	if ttl != nil {
		ttlJSON = fmt.Sprint(*ttl)
	}
	raw := fmt.Sprintf(`{"id":%q,"spec":{"name":%q,"project":%q,"ray_version":"2.56.0","image":%q,
		"head_cpu":"1","head_memory":%q,"ttl_seconds":%s,
		"worker_groups":[{"name":"w","cpu":"1","memory":"2Gi","gpu":null,"min_replicas":%[7]d,"max_replicas":%[7]d,"replicas":%[7]d}]}}`,
		id, id, project, image, HeadMemory(), ttlJSON, WorkerReplicas())
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

// EmptyPolicyUpdate is a PUT /api/v1/settings/policy body that changes
// nothing — for authorization tests that must not alter platform policy.
func EmptyPolicyUpdate() client.UpdatePolicyJSONRequestBody {
	var body client.UpdatePolicyJSONRequestBody
	_ = json.Unmarshal([]byte(`{}`), &body)
	return body
}

// --- Services (requirement 1) ---

// ServeConfigV2 is the Serve application every service test deploys: one
// app whose import path is inside the Ray wheel itself
// (ray.serve._private.test_utils exports get_pid_entrypoint, a bound
// GetPID deployment answering GET / with the replica's pid), so a head
// with no egress (the tenant NetworkPolicy allows DNS and intra-cluster
// only) can import it without a working_dir download. Verified present in
// rayproject/ray:2.56.0. An empty `applications: []` never reaches Ready
// on KubeRay 1.7 (zero serve endpoints), which is why this is not the
// sample config the RBAC matrix uses.
func ServeConfigV2() string {
	return "applications:\n  - name: pid\n    import_path: ray.serve._private.test_utils:get_pid_entrypoint\n    route_prefix: /\n"
}

// ServiceBody builds a deploy_service body for name in project with the
// lane's Ray image and worker count and ServeConfigV2 as the app.
func ServiceBody(name, project string) client.DeployServiceJSONRequestBody {
	var body client.DeployServiceJSONRequestBody
	spec := map[string]any{
		"name": name, "project": project, "ray_version": "2.56.0", "image": RayImage(),
		"serve_config_v2": ServeConfigV2(), "head_cpu": "1", "head_memory": HeadMemory(),
		"worker_replicas": WorkerReplicas(), "worker_cpu": "1", "worker_memory": "2Gi", "upgrade": "in_place",
	}
	raw, _ := json.Marshal(map[string]any{"name": name, "spec": spec})
	_ = json.Unmarshal(raw, &body)
	return body
}

// Deploy deploys as principal and returns (status, body).
func Deploy(t req.T, tgt req.Target, principal string, body client.DeployServiceJSONRequestBody) (int, []byte) {
	t.Helper()
	resp, err := tgt.As(principal).API().DeployServiceWithResponse(context.Background(), body)
	if err != nil {
		t.Fatalf("deploy %s as %s: %v", body.Name, principal, err)
	}
	return resp.StatusCode(), resp.Body
}

// GetService fetches a service view as principal. view is nil unless
// status is 200.
func GetService(t req.T, tgt req.Target, principal, name string) (int, map[string]any) {
	t.Helper()
	resp, err := tgt.As(principal).API().GetServiceWithResponse(context.Background(), name)
	if err != nil {
		t.Fatalf("get service %s as %s: %v", name, principal, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return resp.StatusCode(), nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("get service %s: unmarshal: %v", name, err)
	}
	return resp.StatusCode(), m
}

// WaitService polls until the service's state == want within the lane
// budget and returns the matching view.
func WaitService(t req.T, tgt req.Target, principal, name, want string) map[string]any {
	t.Helper()
	var view map[string]any
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := GetService(t, tgt, principal, name)
		if st != http.StatusOK {
			return false, fmt.Sprintf("get=%d", st)
		}
		state, _ := v["state"].(string)
		view = v
		return state == want, "state=" + state
	})
	return view
}

// DeleteService deletes as principal and returns the status.
func DeleteService(t req.T, tgt req.Target, principal, name string) int {
	t.Helper()
	resp, err := tgt.As(principal).API().DeleteServiceWithResponse(context.Background(), name)
	if err != nil {
		t.Fatalf("delete service %s as %s: %v", name, principal, err)
	}
	return resp.StatusCode()
}

// --- Ephemeral jobs (requirement 5) ---

// SubmitJobBody is the canonical minimal job: entrypoint on RayImage() with
// the contract's default head shape and no workers (the requirement is
// about the cluster's lifetime, not its size). ttl is
// ttl_seconds_after_finished; nil = the server default.
func SubmitJobBody(id, project, entrypoint string, ttl *int32) client.SubmitJobJSONRequestBody {
	ttlJSON := "null"
	if ttl != nil {
		ttlJSON = fmt.Sprint(*ttl)
	}
	raw := fmt.Sprintf(`{"id":%q,"spec":{"project":%q,"entrypoint":%q,"image":%q,"head_memory":%q,"ttl_seconds_after_finished":%s}}`,
		id, project, entrypoint, RayImage(), HeadMemory(), ttlJSON)
	var body client.SubmitJobJSONRequestBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		panic("fixture: SubmitJobBody: " + err.Error())
	}
	return body
}

// SubmitJob posts body as principal and returns the status and body. A
// job the server accepted is deleted as admin at test cleanup — the
// contract has no list operation for jobs, so a target's Cleanup cannot
// find them by run prefix the way it finds clusters.
func SubmitJob(t req.T, tgt req.Target, principal string, body client.SubmitJobJSONRequestBody) (int, []byte) {
	t.Helper()
	resp, err := tgt.As(principal).API().SubmitJobWithResponse(context.Background(), body)
	if err != nil {
		t.Fatalf("submit job as %s: %v", principal, err)
	}
	if resp.StatusCode() == http.StatusCreated {
		var view struct {
			Id string `json:"id"`
		}
		_ = json.Unmarshal(resp.Body, &view)
		t.Cleanup(func() {
			_, _ = tgt.As("admin").API().DeleteJobWithResponse(context.Background(), view.Id, nil)
		})
	}
	return resp.StatusCode(), resp.Body
}

// MustSubmitJob is SubmitJob asserting 201 and returning the view.
func MustSubmitJob(t req.T, tgt req.Target, principal string, body client.SubmitJobJSONRequestBody) map[string]any {
	t.Helper()
	st, b := SubmitJob(t, tgt, principal, body)
	if st != http.StatusCreated {
		t.Fatalf("submit job as %s = %d %s, want 201", principal, st, b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("submit job: unmarshal: %v", err)
	}
	return m
}

// GetJob fetches a job view as principal. view is nil unless status is 200.
func GetJob(t req.T, tgt req.Target, principal, id string) (int, map[string]any) {
	t.Helper()
	resp, err := tgt.As(principal).API().GetJobWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s as %s: %v", id, principal, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return resp.StatusCode(), nil
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("get job %s: unmarshal: %v", id, err)
	}
	return resp.StatusCode(), m
}

// WaitJob polls until the job's Ray status is want (PENDING | RUNNING |
// SUCCEEDED | FAILED | STOPPED) and returns that view. A job that reaches a
// different terminal status first fails the test at once — waiting out the
// lane budget for a SUCCEEDED that can no longer come hides the real
// failure.
func WaitJob(t req.T, tgt req.Target, principal, id, want string) map[string]any {
	t.Helper()
	var view map[string]any
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := GetJob(t, tgt, principal, id)
		if st != http.StatusOK {
			return false, fmt.Sprintf("get=%d", st)
		}
		status, _ := v["status"].(string)
		dep, _ := v["deployment_status"].(string)
		if status == want {
			view = v
			return true, status
		}
		msg, _ := v["message"].(string)
		if status != want && (status == "SUCCEEDED" || status == "FAILED" || status == "STOPPED") {
			t.Fatalf("job %s ended %s (%s) while waiting for %s: %s", id, status, dep, want, msg)
		}
		// KubeRay gave up before the job ran (cluster never ready, submitter
		// failed, spec rejected): Failed with an empty job status is terminal,
		// so say why now instead of polling out the budget.
		if dep == "Failed" && status == "" {
			t.Fatalf("job %s deployment failed before the job ran while waiting for %s: %s", id, want, msg)
		}
		return false, fmt.Sprintf("status=%q deployment_status=%q message=%q", status, dep, msg)
	})
	return view
}

// GatewayHost is the Host header a client presents to reach a registered
// cluster or job through the gateway, read from the view's gateway_url
// (the server is the authority on its own hostname scheme). "" while the
// view carries none.
func GatewayHost(view map[string]any) string {
	raw, _ := view["gateway_url"].(string)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// GatewayRequest sends GET path to the target's API address with Host set
// to host — the way a caller reaches a dynamically registered cluster
// without DNS for the gateway domain — authenticated as principal ("anon"
// = no bearer). Returns the status and body.
func GatewayRequest(t req.T, tgt req.Target, principal, host, path string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tgt.BaseURL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	tgt.As(principal).Authorize(r)
	resp, err := HTTPClient(tgt).Do(r)
	if err != nil {
		t.Fatalf("gateway GET %s%s: %v", host, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
