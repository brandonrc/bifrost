// Package cluster is the L3 target: a live Bifrost deployment reached over
// HTTP plus the Kubernetes cluster it provisions into. `kind` and `grace`
// are configurations of this one implementation (targets.yaml); a test
// never learns which.
//
// Configuration (environment):
//
//	REQ_TARGET               kind | grace — selects the targets.yaml entry
//	BIFROST_URL              API root, e.g. http://127.0.0.1:8484
//	BIFROST_ADMIN_USER       local admin username (default admin)
//	BIFROST_ADMIN_PASSWORD   local admin password (required)
//	BIFROST_INSECURE_TLS     1 = skip TLS verification (grace's self-signed CA)
//	KUBECONFIG               resolved by controller-runtime; optional — without
//	                         it K8s() reports ok=false and k8s tests skip
//	REQ_NAMESPACE            overrides targets.yaml namespace
//	REQ_CONTROL_PLANE_SELECTOR  label selector for the control-plane pods
//	                         (default app.kubernetes.io/name=bifrost-pack)
//
// Unlike inproc, one target is shared by every test in the process (spec
// §1): seeding principals and preflight run once. Everything a test
// creates carries req.RunID(), and Cleanup — registered per test — deletes
// by that prefix and then refuses to return until Kubernetes agrees
// nothing is left (spec §6 postflight, per test rather than per run).
package cluster

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
)

//go:embed targets.yaml
var targetsYAML []byte

type targetDef struct {
	Namespace      string   `yaml:"namespace"`
	ProbeNamespace string   `yaml:"probeNamespace"`
	Contexts       []string `yaml:"contexts"`
	Capabilities   []string `yaml:"capabilities"`
}

type principal struct {
	user    string // "" = the configured admin
	role    string // local role column
	project string // "" = no project-scoped grant
}

// Seeded principals — the same names inproc seeds (spec §1.1). dev-a and
// dev-b are local `developer`s (read-only on clusters, rbac.go) holding a
// project-scoped `operator` grant: that grant is what makes self-serve
// cluster lifecycle possible for a non-admin, and is the shape a
// data-science-pack user has on grace.
var principals = map[string]principal{
	"admin":    {},
	"operator": {user: "req-operator", role: "operator"},
	"dev-a":    {user: "req-dev-a", role: "developer", project: "team-a"},
	"dev-b":    {user: "req-dev-b", role: "developer", project: "team-b"},
}

// seedPassword is per run: seeded users persist on a real deployment
// (there is no delete_user operation), so every run resets their password
// to its own value and no run can be authenticated with a stale one.
func seedPassword() string { return "pw-" + req.RunID() + "-0123456789" }

// leakedPrefix matches ids from any run of this framework.
var leakedPrefix = regexp.MustCompile(`^t[0-9a-f]+-`)

type shared struct {
	name      string
	def       targetDef
	base      string
	ns        string
	probeNS   string
	cpSel     string
	http      *http.Client
	adminUser string
	adminPW   string
	tokens    sync.Map // principal -> bearer
	k8s       *k8sHandle
	caps      map[string]bool
	err       error
}

var (
	once sync.Once
	sh   *shared
)

type target struct {
	s         *shared
	principal string
	t         testing.TB
}

// New returns the process-wide cluster target bound to admin, building it
// on first use. Every failure of that build fails every test that asks —
// a misconfigured lane must be loud, not a wall of skips.
func New(t testing.TB, name string) req.Target {
	t.Helper()
	once.Do(func() { sh = build(name) })
	if sh.err != nil {
		t.Fatalf("cluster target %q unavailable: %v", name, sh.err)
		return nil
	}
	if sh.name != name {
		t.Fatalf("cluster target already built for %q; REQ_TARGET=%q in the same process", sh.name, name)
	}
	tg := &target{s: sh, principal: "admin", t: t}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), postflightBudget()+2*time.Minute)
		defer cancel()
		if err := tg.Cleanup(ctx); err != nil {
			t.Errorf("postflight: %v", err)
		}
	})
	return tg
}

func build(name string) *shared {
	s := &shared{name: name, caps: map[string]bool{}}
	fail := func(err error) *shared { s.err = err; return s }

	defs := map[string]targetDef{}
	if err := yaml.Unmarshal(targetsYAML, &defs); err != nil {
		return fail(fmt.Errorf("targets.yaml: %w", err))
	}
	def, ok := defs[name]
	if !ok {
		return fail(fmt.Errorf("no targets.yaml entry for %q", name))
	}
	s.def = def
	for _, c := range def.Capabilities {
		s.caps[c] = true
	}
	s.ns = envOr("REQ_NAMESPACE", def.Namespace)
	s.probeNS = envOr("REQ_PROBE_NAMESPACE", def.ProbeNamespace)
	s.cpSel = envOr("REQ_CONTROL_PLANE_SELECTOR", "app.kubernetes.io/name=bifrost-pack")
	s.base = strings.TrimRight(os.Getenv("BIFROST_URL"), "/")
	if s.base == "" {
		return fail(errors.New("BIFROST_URL is required"))
	}
	s.adminUser = envOr("BIFROST_ADMIN_USER", "admin")
	s.adminPW = os.Getenv("BIFROST_ADMIN_PASSWORD")
	if s.adminPW == "" {
		return fail(errors.New("BIFROST_ADMIN_PASSWORD is required"))
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fail(errors.New("http.DefaultTransport is not an *http.Transport"))
	}
	tr = tr.Clone()
	if os.Getenv("BIFROST_INSECURE_TLS") == "1" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // grace's self-signed CA, opt-in
	}
	s.http = &http.Client{Transport: tr, Timeout: 90 * time.Second}

	// grace refuses any namespace but the one it is deployed in (spec §6).
	if name == "grace" && s.ns != def.Namespace {
		return fail(fmt.Errorf("grace runs are scoped to namespace %q, got %q", def.Namespace, s.ns))
	}

	k, err := newK8s(s.ns, def.Contexts)
	if err != nil {
		return fail(err)
	}
	s.k8s = k // nil when no kubeconfig resolves: K8s() reports ok=false

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.preflight(ctx); err != nil {
		return fail(err)
	}
	if err := s.seed(ctx); err != nil {
		return fail(fmt.Errorf("seeding principals: %w", err))
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// preflight: the API answers, admin can log in, and no earlier run leaked.
func (s *shared) preflight(ctx context.Context) error {
	resp, err := s.http.Get(s.base + "/healthz")
	if err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz = %d", resp.StatusCode)
	}
	api, err := s.clientFor(ctx, "admin")
	if err != nil {
		return err
	}
	list, err := api.ListClustersWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	if list.StatusCode() != http.StatusOK {
		return fmt.Errorf("list clusters as admin = %d %s", list.StatusCode(), list.Body)
	}
	var items []struct {
		Id      string `json:"id"`
		Desired string `json:"desired"`
	}
	_ = json.Unmarshal(list.Body, &items)
	mine := req.RunID() + "-"
	var leaked []string
	for _, it := range items {
		if leakedPrefix.MatchString(it.Id) && !strings.HasPrefix(it.Id, mine) && it.Desired != "terminated" {
			leaked = append(leaked, it.Id)
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("a previous run leaked clusters %v — a human cleans these up, not the next run (spec §6)", leaked)
	}
	return nil
}

// seed creates (or re-keys) the non-admin principals and their grants
// through the API, so the seeding itself exercises the access path.
func (s *shared) seed(ctx context.Context) error {
	api, err := s.clientFor(ctx, "admin")
	if err != nil {
		return err
	}
	pw := seedPassword()
	for name, p := range principals {
		if p.user == "" {
			continue
		}
		var create client.CreateUserJSONRequestBody
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q,"role":%q}`, p.user, pw, p.role)), &create)
		cr, err := api.CreateUserWithResponse(ctx, create)
		if err != nil {
			return fmt.Errorf("create %s: %w", p.user, err)
		}
		switch cr.StatusCode() {
		case http.StatusCreated, http.StatusOK:
		case http.StatusConflict:
			// Exists from an earlier run: re-key and re-enable, so this run's
			// password is the only one that works.
			var upd client.UpdateUserJSONRequestBody
			_ = json.Unmarshal([]byte(fmt.Sprintf(`{"password":%q,"disabled":false,"role":%q}`, pw, p.role)), &upd)
			ur, err := api.UpdateUserWithResponse(ctx, p.user, upd)
			if err != nil || ur.StatusCode()/100 != 2 {
				return fmt.Errorf("re-key %s: err=%v status=%v", p.user, err, codeOf(ur))
			}
		default:
			return fmt.Errorf("create %s = %d %s", p.user, cr.StatusCode(), cr.Body)
		}
		if p.project != "" {
			var body client.UpsertAssignmentJSONRequestBody
			_ = json.Unmarshal([]byte(fmt.Sprintf(`{"role":"operator","scope":"project:%s"}`, p.project)), &body)
			ar, err := api.UpsertAssignmentWithResponse(ctx, p.user, body)
			if err != nil || ar.StatusCode()/100 != 2 {
				return fmt.Errorf("assign %s operator@%s: err=%v status=%v", p.user, p.project, err, codeOf(ar))
			}
		}
		_ = name
	}
	return nil
}

type statusCoder interface{ StatusCode() int }

func codeOf(r statusCoder) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}

// token logs the principal in on first use and caches the bearer.
func (s *shared) token(ctx context.Context, principal string) (string, error) {
	if principal == "anon" {
		return "", nil
	}
	if v, ok := s.tokens.Load(principal); ok {
		if tok, ok := v.(string); ok {
			return tok, nil
		}
	}
	p, ok := principals[principal]
	if !ok {
		return "", fmt.Errorf("unknown principal %q", principal)
	}
	user, pw := s.adminUser, s.adminPW
	if p.user != "" {
		user, pw = p.user, seedPassword()
	}
	c, err := client.NewClientWithResponses(s.base, client.WithHTTPClient(s.http))
	if err != nil {
		return "", err
	}
	var body client.LoginJSONRequestBody
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pw)), &body)
	resp, err := c.LoginWithResponse(ctx, body)
	if err != nil {
		return "", fmt.Errorf("login %s: %w", user, err)
	}
	if resp.StatusCode() != http.StatusOK {
		return "", fmt.Errorf("login %s = %d %s", user, resp.StatusCode(), resp.Body)
	}
	var m struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(resp.Body, &m)
	if m.Token == "" {
		return "", fmt.Errorf("login %s: empty token", user)
	}
	s.tokens.Store(principal, m.Token)
	return m.Token, nil
}

func (s *shared) clientFor(ctx context.Context, principal string) (*client.ClientWithResponses, error) {
	tok, err := s.token(ctx, principal)
	if err != nil {
		return nil, err
	}
	return client.NewClientWithResponses(s.base, client.WithHTTPClient(s.http),
		client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			if tok != "" {
				r.Header.Set("Authorization", "Bearer "+tok)
			}
			return nil
		}))
}

// --- req.Target -------------------------------------------------------

func (tg *target) Name() string      { return tg.s.name }
func (tg *target) Namespace() string { return tg.s.ns }
func (tg *target) Clock() req.FakeClock {
	return nil // real time on a real cluster (spec §1.1)
}
func (tg *target) Has(capability string) bool { return tg.s.caps[capability] }
func (tg *target) BaseURL() string            { return tg.s.base }

// HTTPClient exposes the target's transport (TLS posture) for raw requests.
func (tg *target) HTTPClient() *http.Client { return tg.s.http }

func (tg *target) K8s() (ctrlclient.Client, bool) {
	if tg.s.k8s == nil {
		return nil, false
	}
	return tg.s.k8s.guarded, true
}

func (tg *target) As(p string) req.Target {
	if _, ok := principals[p]; !ok && p != "anon" {
		panic("cluster: unknown principal " + p)
	}
	cp := *tg
	cp.principal = p
	return &cp
}

func (tg *target) Authorize(r *http.Request) {
	tg.t.Helper()
	tok, err := tg.s.token(context.Background(), tg.principal)
	if err != nil {
		tg.t.Fatalf("cluster: %v", err)
		return
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
}

func (tg *target) API() *client.ClientWithResponses {
	tg.t.Helper()
	c, err := tg.s.clientFor(context.Background(), tg.principal)
	if err != nil {
		tg.t.Fatalf("cluster: %v", err)
		return nil
	}
	return c
}

// Cleanup deletes every cluster of this run through the API, reaps probe
// objects, then waits for Kubernetes to show nothing left with the run's
// labels. A non-empty leftover is an error: the run leaked (spec §6).
func (tg *target) Cleanup(ctx context.Context) error {
	api, err := tg.s.clientFor(ctx, "admin")
	if err != nil {
		return err
	}
	list, err := api.ListClustersWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	var items []struct {
		Id      string `json:"id"`
		Desired string `json:"desired"`
	}
	_ = json.Unmarshal(list.Body, &items)
	prefix := req.RunID() + "-"
	for _, it := range items {
		if !strings.HasPrefix(it.Id, prefix) {
			continue
		}
		if _, err := api.DeleteClusterWithResponse(ctx, it.Id, nil); err != nil {
			return fmt.Errorf("delete %s: %w", it.Id, err)
		}
	}
	if tg.s.k8s == nil {
		return nil
	}
	return tg.s.k8s.postflight(ctx, prefix, tg.s.probeNS)
}

// --- req.Restarter ----------------------------------------------------

// RestartControlPlane deletes the control-plane pods and waits for a fresh
// pod to be Ready and /healthz to answer. The deployment's ReplicaSet
// brings the pod back; this exercises the same crash path a node
// eviction would.
func (tg *target) RestartControlPlane(ctx context.Context) error {
	if tg.s.k8s == nil {
		return errors.New("no Kubernetes client; cannot restart the control plane")
	}
	if err := tg.s.k8s.restart(ctx, tg.s.cpSel); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := tg.s.http.Get(tg.s.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("control plane did not answer /healthz within 3m of restart")
}

// --- req.PodRunner ----------------------------------------------------

func (tg *target) ProbeNamespace() string { return tg.s.probeNS }
func (tg *target) RayImage() string       { return envOr("REQ_RAY_IMAGE", "rayproject/ray:2.56.0") }

func (tg *target) RunPod(ctx context.Context, spec req.PodSpec) (req.PodResult, error) {
	if tg.s.k8s == nil {
		return req.PodResult{}, errors.New("no Kubernetes client; cannot run probe pods")
	}
	if spec.Namespace == "" {
		spec.Namespace = tg.s.probeNS
	}
	if spec.Timeout == 0 {
		spec.Timeout = req.EventuallyTimeout(tg)
	}
	return tg.s.k8s.runPod(ctx, spec)
}

var (
	_ req.Target    = (*target)(nil)
	_ req.Restarter = (*target)(nil)
	_ req.PodRunner = (*target)(nil)
)
