package api

// Cross-cutting security regression tests: adversarial scenarios that span
// SEVERAL of the hardening fixes at once (F3a control-plane body cap, F5
// login rate limit + timing-parity lockout, F6 southbound SSRF/registry
// hardening, #58/F1 GPU tenant isolation on jobs, T13 host-is-cluster
// gate). The per-fix unit tests pin each defense in isolation; these pin
// the INTERACTIONS — middleware ordering, which 4xx wins, and whether one
// defense's bypass surface is closed by another.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/core"
)

func securityHandler(t *testing.T) (http.Handler, controller.Store) {
	t.Helper()
	store := newMemStore(t)
	local := auth.NewLocalAuthenticator(store, 3600, 90)
	s := &Server{Store: store, Local: local}
	return NewHandler(s, HandlerOptions{Local: local, Store: store}), store
}

func serve(h http.Handler, remoteAddr, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, bytes200(rec.Body.Bytes()))
	}
	return envelope.Error
}

// F3a × F5 × public-allowlist: an oversized, UNAUTHENTICATED body to the
// public login route is refused by the 4 MiB control-plane cap (400
// bad_request from the decode, Content-Type set so the body is actually
// read — the cap, not a media-type rejection, does the work), while the
// same oversized body to a NON-public route without a token is refused
// earlier, at auth (401 — the body is never even read there).
func TestSecurityOversizedUnauthenticatedLogin(t *testing.T) {
	h, _ := securityHandler(t)
	big := "{" + strings.Repeat(" ", int(maxControlPlaneBodyBytes)+1024)

	rec := serve(h, "198.51.100.9:4000", http.MethodPost, "/api/v1/auth/login", big)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized unauthenticated login body = %d, want 400 (the 4MiB cap rejects before decode); body=%s",
			rec.Code, bytes200(rec.Body.Bytes()))
	}
	if got := errorCode(t, rec); got != "bad_request" {
		t.Errorf("error code = %q, want bad_request", got)
	}

	// The server is unharmed and the cap rejects by size, not by route:
	// a small, well-formed login body gets the route's normal answer.
	small := serve(h, "198.51.100.10:4000", http.MethodPost, "/api/v1/auth/login",
		`{"username":"ghost","password":"x"}`)
	if small.Code != http.StatusUnauthorized {
		t.Errorf("small login body = %d, want 401 invalid_credentials (cap must not blanket-refuse the route)", small.Code)
	}

	// A non-public route never reaches the cap unauthenticated: RequireAuth
	// (the outer layer) refuses first, so the 4MiB of body is never read
	// into the control plane at all.
	clusters := serve(h, "198.51.100.11:4000", http.MethodPost, "/api/v1/clusters", big)
	if clusters.Code != http.StatusUnauthorized {
		t.Errorf("oversized unauthenticated POST /api/v1/clusters = %d, want 401 (auth gates before the body cap)", clusters.Code)
	}
}

// F3a × F5 interaction, pinned: oversized login attempts DO count against
// the per-IP rate-limit bucket. This is the safe direction and it is
// structural — the limiter wraps the body cap (it runs before the body is
// read, so it cannot know the body is oversize), and charging every
// attempt a token means an attacker spraying 4 MiB+ bodies at the login
// route burns their own budget after the burst; a limiter that counted
// only well-formed attempts would let the memory-amplification probe run
// at unlimited rate.
func TestSecurityOversizedAttemptsConsumeLoginBudget(t *testing.T) {
	h, _ := securityHandler(t)
	attacker := "203.0.113.66:5555"
	big := "{" + strings.Repeat(" ", int(maxControlPlaneBodyBytes)+1024)

	for i := 0; i < defaultLoginBurst; i++ {
		rec := serve(h, attacker, http.MethodPost, "/api/v1/auth/login", big)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("oversized attempt %d = %d, want 400 (inside burst, refused by the cap not the limiter)", i+1, rec.Code)
		}
	}
	// The burst is spent on oversized junk: the very next attempt — even a
	// SMALL, well-formed one — is rate-limited.
	rec := serve(h, attacker, http.MethodPost, "/api/v1/auth/login",
		`{"username":"ghost","password":"x"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst attempt = %d, want 429 (oversized attempts must consume the bucket)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After — clients cannot tell when to retry")
	}
	if got := errorCode(t, rec); got != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", got)
	}
}

// F5 × lockout interaction order: an IP that has exhausted its bucket gets
// 429 BEFORE any credential check runs — even with a correct password for
// a locked account, the rate limiter wins (Retry-After present). And the
// lockout stays invisible on the wire under rate-limit pressure: a fresh
// IP gets byte-identical 401 bodies for (locked account, correct password)
// and (valid account, wrong password).
func TestSecurityRateLimitWinsOverLockedAccount(t *testing.T) {
	h, store := securityHandler(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("correct-horse-99")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLocalUser(ctx, "alice", nil, hash, core.LocalRoleDeveloper); err != nil {
		t.Fatal(err)
	}
	// Lock the account through the real lockout path: LoginLockoutThreshold
	// consecutive failures.
	for i := uint32(0); i < controller.LoginLockoutThreshold; i++ {
		if err := store.RecordLoginFailure(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
	}

	attacker := "203.0.113.77:5555"
	// Drain the bucket with malformed bodies — 400s that never reach the
	// credential path (no bcrypt paid), so this is fast.
	for i := 0; i < defaultLoginBurst; i++ {
		rec := serve(h, attacker, http.MethodPost, "/api/v1/auth/login", "{")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("drain attempt %d = %d, want 400", i+1, rec.Code)
		}
	}
	// Bucket empty: correct credentials for the locked account still get
	// 429 — the limiter short-circuits ahead of the login handler, so the
	// lockout is never even consulted.
	rec := serve(h, attacker, http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"correct-horse-99"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited login = %d, want 429 (limiter must precede the credential check)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	// From a fresh IP the locked account answers 401 — and the body is
	// byte-identical to a plain wrong-password 401 (no user/state
	// enumeration even when the lockout is under active pressure).
	locked := serve(h, "198.51.100.1:4000", http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"correct-horse-99"}`)
	wrong := serve(h, "198.51.100.2:4000", http.MethodPost, "/api/v1/auth/login",
		`{"username":"alice","password":"wrong-password-1"}`)
	if locked.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Fatalf("locked=%d wrong=%d, want both 401", locked.Code, wrong.Code)
	}
	if locked.Body.String() != wrong.Body.String() {
		t.Errorf("locked-account body %q differs from wrong-password body %q — account-state enumeration", locked.Body.String(), wrong.Body.String())
	}
	if got := errorCode(t, locked); got != "invalid_credentials" {
		t.Errorf("locked-account error code = %q, want invalid_credentials", got)
	}
}

// F3a × T13 host dispatch: Host-header seams between the control-plane
// stack (4 MiB cap, login limiter, public allowlist) and the gateway
// (64 MiB cap, allowlist suppressed). Middleware order is RequireAuth →
// HostGateway → everything control-plane, so the Host decision is made
// exactly once, at auth, and both layers agree on it (both read r.Host
// through registry.ByHostname).
func TestSecurityHostHeaderSeams(t *testing.T) {
	upstream, _, lastBody := newRecordingUpstream(t, http.StatusOK, []byte("ok"), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	local, token := newLocalRoleToken(t, "dev", core.LocalRoleDeveloper)
	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	defer srv.Close()

	do := func(host, method, path, body, bearer string) (int, http.Header) {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}

	// 1. Lookalike hostnames are NOT the cluster: the gateway must not
	// proxy them, so control-plane semantics apply (public /healthz → 200)
	// and — the security-relevant half — the 4 MiB cap still governs
	// their control-plane bodies. Checked FIRST, while the upstream has
	// seen nothing.
	for _, host := range []string{"ray.cluster.test.evil.com", "ray.cluster.test.", "not-ray.cluster.test"} {
		if code, _ := do(host, http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
			t.Errorf("Host %q GET /healthz = %d, want 200 (a lookalike host is plain control-plane traffic)", host, code)
		}
	}
	if lastBody() != nil {
		t.Error("a lookalike Host was proxied to the cluster upstream — Host-suffix confusion")
	}

	// 2. The registered hostname suppresses the public allowlist no matter
	// how the client spells it — case-insensitive match, port stripped.
	// Unauthenticated /healthz on a cluster host is 401, never 200.
	for _, host := range []string{"ray.cluster.test", "RAY.CLUSTER.TEST", "Ray.Cluster.Test:8443"} {
		if code, _ := do(host, http.MethodGet, "/healthz", "", ""); code != http.StatusUnauthorized {
			t.Errorf("Host %q GET /healthz without token = %d, want 401 (host-is-cluster suppresses the allowlist)", host, code)
		}
	}

	// 3. The same oversized body the control plane refuses at 4 MiB is
	// proxied intact on a cluster host — a control-plane PATH on a cluster
	// Host (POST /api/v1/auth/login!) never reaches the control-plane
	// stack: no 4 MiB cap, no login rate limit, no login handler. T13's
	// core invariant: a cluster hostname is never shadowed by a
	// control-plane path.
	big := "{" + strings.Repeat(" ", int(maxControlPlaneBodyBytes)+1024)
	code, _ := do("ray.cluster.test", http.MethodPost, "/api/v1/auth/login", big, token)
	if code != http.StatusOK {
		t.Errorf("proxied login-path POST with %d-byte body = %d, want 200 from the upstream (gateway cap is 64 MiB)", len(big), code)
	}
	if got := len(lastBody()); got != len(big) {
		t.Errorf("upstream saw %d body bytes, want %d — the control-plane cap must not clip cluster-host traffic", got, len(big))
	}

	// 4. But unauthenticated, the same request dies at RequireAuth before
	// the gateway or any body handling: 401, and the upstream never sees it.
	before := len(lastBody())
	code, _ = do("ray.cluster.test", http.MethodPost, "/api/v1/auth/login", big, "")
	if code != http.StatusUnauthorized {
		t.Errorf("unauthenticated oversized login-path POST on a cluster host = %d, want 401", code)
	}
	if len(lastBody()) != before {
		t.Error("an unauthenticated cluster-host request reached the upstream")
	}
}

// The gateway's own body cap — not the control plane's — governs
// cluster-host traffic, pinned by shrinking the gateway cap to exactly the
// control-plane value: the same 4 MiB+1 body then gets the GATEWAY's 413
// (payload_too_large), while the identical body on a control-plane host
// gets the control plane's 400 (bad_request). Two caps, two seams,
// distinguishable outcomes — a regression that lets one cap bleed into
// the other's path shows up as the wrong status.
func TestSecurityGatewayCapDistinctFromControlPlaneCap(t *testing.T) {
	upstream, _, _ := newRecordingUpstream(t, http.StatusOK, []byte("ok"), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	local, token := newLocalRoleToken(t, "dev", core.LocalRoleDeveloper)
	limits := DefaultGatewayLimits()
	limits.MaxBodyBytes = maxControlPlaneBodyBytes // both caps numerically equal; only the STATUS tells them apart
	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry, GatewayLimits: &limits})
	srv := httptest.NewServer(h)
	defer srv.Close()

	big := "{" + strings.Repeat(" ", int(maxControlPlaneBodyBytes)+1024)
	post := func(host, path, bearer string) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(big))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("ray.cluster.test", "/api/jobs/", token); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body on cluster host = %d, want 413 (the gateway cap refused it)", code)
	}
	if code := post("api.control.test", "/api/v1/auth/login", ""); code != http.StatusBadRequest {
		t.Errorf("same oversized body on control-plane host = %d, want 400 (the control-plane cap refused it)", code)
	}
}

// #58/F1 × profile expansion (requirement 7/D4): a fractional GPU that
// arrives THROUGH A PROFILE — never spelled in the request body — must hit
// the same multi-tenant-pool refusal as a literal spec field. The
// isolation check runs on the derived ClusterSpec view AFTER finishJobSpec
// expands the profile; this test is the pin against anyone moving it
// earlier.
func TestSecurityGpuIsolationViaProfileExpansion(t *testing.T) {
	ctx := context.Background()
	fracProfile := core.Profile{
		Name:       "frac-profile",
		HeadCpu:    "1",
		HeadMemory: "2Gi",
		WorkerGroups: []core.WorkerGroup{{
			Name: "w", Cpu: "1", Memory: "2Gi", Gpu: strPtr("0.5"), MinReplicas: 0, MaxReplicas: 1,
		}},
	}
	admin := testIdentity("admin", auth.RoleAdmin)
	submit := func(s *Server, id string) error {
		body := jobBodyFor("proj-a")
		body.Id = strPtr(id)
		body.Spec.Profile = strPtr("frac-profile")
		_, err := s.SubmitJob(ctxWithIdentity(admin), SubmitJobRequestObject{Body: &body})
		return err
	}

	multi := gpuIsolationStore(t, nil, "proj-a", "proj-b")
	s := &Server{Store: multi, PolicySeed: PolicyConfig{Profiles: []core.Profile{fracProfile}}}
	err := submit(s, "job-profile-frac")
	mustHTTPError(t, err, http.StatusBadRequest)
	var he HTTPError
	if errorsAs(err, &he) && !strings.Contains(he.Message, "tenant isolation") {
		t.Errorf("message = %q, want the tenant-isolation refusal (fractional GPU via profile)", he.Message)
	}
	if j, _ := multi.GetRayJob(ctx, "job-profile-frac"); j != nil {
		t.Error("a profile-expanded fractional-GPU job must not be persisted in a multi-tenant pool")
	}

	// Control: the identical profile in a single-tenant pool is admitted —
	// the refusal above is the isolation rule, not profile machinery.
	solo := gpuIsolationStore(t, nil, "proj-a")
	s2 := &Server{Store: solo, PolicySeed: PolicyConfig{Profiles: []core.Profile{fracProfile}}}
	if err := submit(s2, "job-profile-frac"); err != nil {
		t.Errorf("same profile in a single-tenant pool: %v, want admitted", err)
	}
}

// #58 × quantity spelling: "0.50" (fractional with a trailing zero) parses
// fine and must hit the tenant-isolation refusal in a shared pool, while
// "500m" — a CPU-style milli suffix — is not a GPU quantity at all and is
// refused as an invalid spec REGARDLESS of tenancy (the device plugin has
// no milli-GPU; parsing it as 0.5 would smuggle a fractional request past
// the spellcheck).
func TestSecurityGpuIsolationQuantitySpellings(t *testing.T) {
	admin := testIdentity("admin", auth.RoleAdmin)
	submit := func(s *Server, id, gpu string) error {
		body := jobBodyFor("proj-a")
		body.Id = strPtr(id)
		body.Spec.WorkerGroups = &[]WorkerGroup{{Name: "w", Cpu: "1", Memory: "2Gi", Gpu: &gpu, MinReplicas: 0, MaxReplicas: 1}}
		_, err := s.SubmitJob(ctxWithIdentity(admin), SubmitJobRequestObject{Body: &body})
		return err
	}

	multi := gpuIsolationStore(t, nil, "proj-a", "proj-b")
	sm := &Server{Store: multi}
	err := submit(sm, "job-050", "0.50")
	mustHTTPError(t, err, http.StatusBadRequest)
	var he HTTPError
	if errorsAs(err, &he) && !strings.Contains(he.Message, "tenant isolation") {
		t.Errorf(`"0.50" refusal message = %q, want tenant isolation`, he.Message)
	}
	err = submit(sm, "job-500m", "500m")
	mustHTTPError(t, err, http.StatusBadRequest)
	if errorsAs(err, &he) && strings.Contains(he.Message, "tenant isolation") {
		t.Errorf(`"500m" must fail as an invalid quantity, not as tenant isolation: %q`, he.Message)
	}
	for _, id := range []string{"job-050", "job-500m"} {
		if j, _ := multi.GetRayJob(t.Context(), core.ClusterId(id)); j != nil {
			t.Errorf("%s persisted despite refusal", id)
		}
	}

	solo := gpuIsolationStore(t, nil, "proj-a")
	ss := &Server{Store: solo}
	if err := submit(ss, "job-050", "0.50"); err != nil {
		t.Errorf(`"0.50" in a single-tenant pool: %v, want admitted (fractional is fine within one tenant)`, err)
	}
	if err := submit(ss, "job-500m", "500m"); err == nil {
		t.Error(`"500m" in a single-tenant pool was admitted — milli is not a GPU unit and must stay a 400`)
	}
}

// #58 × tenancy flip mid-flight: the pool a job is admitted into is
// re-evaluated on EVERY submission, so a pool that flips from single- to
// multi-tenant immediately starts refusing fractional jobs — and a pool
// resolving to time-slice that gains a second tenant refuses even
// whole-GPU jobs (fail closed: admission into a non-compliant pool is
// refused outright). Already-admitted jobs are untouched (admission is a
// write-time gate, not a retroactive sweep — the reconciler owns teardown
// decisions).
func TestSecurityGpuIsolationTenancyFlipMidFlight(t *testing.T) {
	ctx := context.Background()
	admin := testIdentity("admin", auth.RoleAdmin)
	submit := func(s *Server, id, gpu string) error {
		body := jobBodyFor("proj-a")
		body.Id = strPtr(id)
		body.Spec.WorkerGroups = &[]WorkerGroup{{Name: "w", Cpu: "1", Memory: "2Gi", Gpu: &gpu, MinReplicas: 0, MaxReplicas: 1}}
		_, err := s.SubmitJob(ctxWithIdentity(admin), SubmitJobRequestObject{Body: &body})
		return err
	}
	addTenant := func(t *testing.T, store controller.Store, project string) {
		t.Helper()
		if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "gpu-pool", Project: project, Namespace: "ns-" + project}); err != nil {
			t.Fatal(err)
		}
	}

	// Whole-GPU default pool: fractional admitted while single-tenant,
	// refused the moment a second tenant appears.
	store := gpuIsolationStore(t, nil, "proj-a")
	s := &Server{Store: store}
	if err := submit(s, "job-before-flip", "0.5"); err != nil {
		t.Fatalf("fractional GPU while single-tenant: %v, want admitted", err)
	}
	addTenant(t, store, "proj-b")
	err := submit(s, "job-after-flip", "0.5")
	mustHTTPError(t, err, http.StatusBadRequest)
	if j, _ := store.GetRayJob(ctx, "job-after-flip"); j != nil {
		t.Error("fractional job admitted after the pool went multi-tenant")
	}
	if j, _ := store.GetRayJob(ctx, "job-before-flip"); j == nil {
		t.Error("the pre-flip job must survive — isolation is a write-time gate, not a retroactive sweep")
	}

	// Time-slice pool: single-tenant opt-in admits anything; the flip
	// makes the pool itself non-compliant, so even a whole-GPU job is
	// refused (CheckPoolGpuIsolation fires first).
	ts := core.GpuSharingTimeSlice
	tsStore := gpuIsolationStore(t, &ts, "proj-a")
	tsServer := &Server{Store: tsStore}
	if err := submit(tsServer, "job-ts-before", "1"); err != nil {
		t.Fatalf("whole GPU in a single-tenant time-slice pool: %v, want admitted", err)
	}
	addTenant(t, tsStore, "proj-b")
	err = submit(tsServer, "job-ts-after", "1")
	mustHTTPError(t, err, http.StatusBadRequest)
	var he HTTPError
	if errorsAs(err, &he) && !strings.Contains(he.Message, "time-slice") {
		t.Errorf("post-flip refusal message = %q, want the time-slice pool refusal", he.Message)
	}
	if j, _ := tsStore.GetRayJob(ctx, "job-ts-after"); j != nil {
		t.Error("a job was admitted into a pool that became multi-tenant time-slice")
	}
}

// gpuIsolationStore builds a store with one GPU compute pool (gpu_sharing
// = mode, nil = inherit the whole-gpu platform default) and one allocation
// per project.
func gpuIsolationStore(t *testing.T, mode *core.GpuSharing, projects ...string) controller.Store {
	t.Helper()
	store := controller.NewMemoryStore()
	ctx := context.Background()
	spec := core.PoolSpec{
		Name:       "gpu-pool",
		Cohort:     "c",
		GpuSharing: mode,
		Flavors:    []core.FlavorSpec{{Name: "f", Resources: map[string]string{"nvidia.com/gpu": "8"}}},
	}
	if _, err := store.UpsertPool(ctx, "gpu-pool", spec); err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if err := store.UpsertAllocation(ctx, core.AllocationSpec{Pool: "gpu-pool", Project: p, Namespace: "ns-" + p}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}
