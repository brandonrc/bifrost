package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/core"
)

// Control-plane request hardening (F3a body cap, F5b login rate limit):
// both wrap the control-plane routes only, inside the gateway dispatch.

// ---------------------------------------------------------------------------
// F3a: control-plane request-body cap
// ---------------------------------------------------------------------------

// An oversized body to a PUBLIC route (no token needed) is refused cleanly
// instead of being decoded into memory. Before the cap, /api/v1/auth/login
// would happily buffer and decode an arbitrary-length body.
func TestOversizedBodyOnPublicLoginIsRejected(t *testing.T) {
	local := auth.NewLocalAuthenticator(newFakeUserStore(), 3600, 90)
	s := &Server{Store: newMemStore(t), Local: local}
	srv := httptest.NewServer(NewHandler(s, HandlerOptions{Local: local}))
	defer srv.Close()

	big := "{" + strings.Repeat(" ", int(maxControlPlaneBodyBytes)+1024)
	resp, err := srv.Client().Post(srv.URL+"/api/v1/auth/login", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized login body -> %d, want a clean 400-class rejection; body=%s",
			resp.StatusCode, bytes200(body))
	}
	if resp.StatusCode == http.StatusOK {
		t.Error("oversized body was processed")
	}

	// The server is unharmed: a well-formed small body gets the normal
	// answer (401 invalid_credentials for an unknown user), proving the cap
	// rejects by size, not by blanket refusal.
	small := strings.NewReader(`{"username":"ghost","password":"x"}`)
	resp2, err := srv.Client().Post(srv.URL+"/api/v1/auth/login", "application/json", small)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("small login body -> %d, want 401 (cap must not interfere with legit traffic)", resp2.StatusCode)
	}
}

// The cap guards ONLY the control plane: a cluster-host request is proxied
// by the gateway under its own (64 MiB) buffered-body limit, so a body over
// 4 MiB but under the gateway cap must reach the upstream intact.
func TestGatewayProxyPathIsNotBodyCappedByControlPlaneLimit(t *testing.T) {
	upstream, _, lastBody := newRecordingUpstream(t, http.StatusOK, []byte("ok"), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	local, token := newLocalRoleToken(t, "dev", core.LocalRoleDeveloper)

	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	defer srv.Close()

	payload := strings.Repeat("j", int(maxControlPlaneBodyBytes)+1024)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/jobs/", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ray.cluster.test"
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied POST with a %d-byte body -> %d, want 200 (gateway cap is 64 MiB, not the control plane's 4 MiB)",
			len(payload), resp.StatusCode)
	}
	if got := len(lastBody()); got != len(payload) {
		t.Errorf("upstream saw %d body bytes, want %d — the control-plane cap must not clip proxied traffic", got, len(payload))
	}
}

// ---------------------------------------------------------------------------
// F5b: per-client-IP rate limit on /api/v1/auth/login
// ---------------------------------------------------------------------------

// Token-bucket arithmetic under a fake clock: burst drains, sustained rate
// refills, and each client IP gets its own bucket.
func TestLoginRateLimiterBucket(t *testing.T) {
	l := newLoginRateLimiter(60, 3) // 1 token/sec, burst 3
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !l.allow("203.0.113.7") {
			t.Fatalf("attempt %d within burst refused", i+1)
		}
	}
	if l.allow("203.0.113.7") {
		t.Fatal("burst exhausted but a 4th attempt was allowed")
	}
	// A different IP has its own bucket.
	if !l.allow("203.0.113.8") {
		t.Error("a fresh IP must not inherit another IP's exhaustion")
	}
	// Refill at the sustained rate: +1s buys exactly one more attempt.
	now = now.Add(time.Second)
	if !l.allow("203.0.113.7") {
		t.Error("one second of refill must buy one attempt")
	}
	if l.allow("203.0.113.7") {
		t.Error("refill must not exceed the sustained rate")
	}
}

// Idle buckets are swept so an IP-spray cannot grow the map without bound.
func TestLoginRateLimiterSweepsIdleBuckets(t *testing.T) {
	l := newLoginRateLimiter(60, 3)
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	l.allow("10.0.0.1")
	l.allow("10.0.0.2")
	now = now.Add(2 * time.Minute) // past the sweep interval
	l.allow("10.0.0.2")            // triggers the sweep; 10.0.0.1 is idle but under the TTL
	l.mu.Lock()
	mid := len(l.buckets)
	l.mu.Unlock()
	if mid != 2 {
		t.Fatalf("buckets = %d, want 2 (neither idle past the TTL yet)", mid)
	}

	now = now.Add(loginBucketIdleTTL + time.Minute)
	l.allow("10.0.0.2") // triggers the sweep; 10.0.0.1 is now stale
	l.mu.Lock()
	got := len(l.buckets)
	_, staleKept := l.buckets["10.0.0.1"]
	l.mu.Unlock()
	if got != 1 || staleKept {
		t.Errorf("buckets = %d (stale kept: %v), want 1 with the idle bucket swept", got, staleKept)
	}
}

// The middleware guards exactly POST /api/v1/auth/login — nothing else, not
// even the same path with another verb.
func TestLoginRateLimitMiddlewareScope(t *testing.T) {
	l := newLoginRateLimiter(1, 1) // one attempt, then dry
	h := l.middleware(okHandler())

	call := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.7:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(http.MethodPost, "/api/v1/auth/login"); got != http.StatusOK {
		t.Fatalf("first login attempt = %d, want through to the handler", got)
	}
	if got := call(http.MethodPost, "/api/v1/auth/login"); got != http.StatusTooManyRequests {
		t.Fatalf("second login attempt = %d, want 429", got)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/login"},
		{http.MethodPost, "/api/v1/auth/providers"},
		{http.MethodPost, "/api/v1/clusters"},
	} {
		if got := call(tc.method, tc.path); got != http.StatusOK {
			t.Errorf("%s %s = %d, want untouched by the login limiter", tc.method, tc.path, got)
		}
	}
}

// End-to-end through the assembled handler with the PRODUCTION defaults:
// the burst (20) of attempts all reach the route (a malformed body is a
// fast 400 — no bcrypt involved, keeping this cheap), and the 21st from the
// same IP is a 429 while a different IP is unaffected. A legit client
// retrying a typo a few times never comes near the limit.
func TestLoginRateLimitEndToEndWithProductionDefaults(t *testing.T) {
	h := NewHandler(NewServer(), HandlerOptions{
		Local: authForRateLimitTest(t),
	})

	call := func(remoteAddr string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{"))
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
		return rec.Code, envelope.Error
	}

	attacker := "203.0.113.7:5555"
	for i := 0; i < defaultLoginBurst; i++ {
		if code, _ := call(attacker); code != http.StatusBadRequest {
			t.Fatalf("attempt %d within the burst = %d, want 400 from the route (not rate-limited)", i+1, code)
		}
	}
	code, errCode := call(attacker)
	if code != http.StatusTooManyRequests || errCode != "rate_limited" {
		t.Errorf("attempt past the burst = %d (%q), want 429 rate_limited", code, errCode)
	}
	if code, _ := call("198.51.100.23:4444"); code != http.StatusBadRequest {
		t.Errorf("a different client IP = %d, want 400 — buckets are per-IP", code)
	}
}

func authForRateLimitTest(t *testing.T) *auth.LocalAuthenticator {
	t.Helper()
	return auth.NewLocalAuthenticator(newFakeUserStore(), 3600, 90)
}

func bytes200(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Red-team fix (2026-09-12): IPv6 peers share one bucket per /64.
// ---------------------------------------------------------------------------

// A single IPv6 host rotates source addresses within its /64 at will
// (SLAAC privacy extensions), so per-address buckets were unlimited fresh
// buckets — unlimited password spraying and unlimited account-lockout
// pressure. Bucket keys now aggregate IPv6 peers at /64; IPv4 stays
// per-address.
func TestLoginBucketKeyIPv6AggregatesAtSlash64(t *testing.T) {
	key := func(remoteAddr string) string {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.RemoteAddr = remoteAddr
		return loginBucketKey(r)
	}

	// Same /64, different low 64 bits (and different ports): one bucket.
	a := key("[2001:db8:1:2::1]:5555")
	b := key("[2001:db8:1:2::ffff]:6666")
	if a != b {
		t.Errorf("same-/64 keys differ: %q vs %q — a rotating host gets fresh buckets", a, b)
	}
	// A different /64 is a different bucket.
	if c := key("[2001:db8:1:3::1]:5555"); c == a {
		t.Errorf("different /64 shared a key: %q", c)
	}
	// Zone qualifiers and bracket forms don't split a host's bucket.
	if z := key("[fe80::1%eth0]:1234"); z != key("[fe80::1]:5678") {
		t.Errorf("zone-qualified key %q differs from the unzoned one", z)
	}
	// Bare literals (no port) normalize too.
	if bare := key("2001:db8:1:2::99"); bare != a {
		t.Errorf("bare IPv6 literal key = %q, want the same /64 key %q", bare, a)
	}
	// IPv4 unchanged: per-address, port stripped, no aggregation.
	if v4 := key("203.0.113.7:4000"); v4 != "203.0.113.7" {
		t.Errorf("IPv4 key = %q, want the bare address", v4)
	}
	if key("203.0.113.7:4000") == key("203.0.113.8:4000") {
		t.Error("distinct IPv4 addresses must not share a bucket")
	}
	// Unparseable peers still key on the raw string (one shared bucket,
	// never a free pass).
	if raw := key("not-an-ip"); raw != "not-an-ip" {
		t.Errorf("unparseable peer key = %q, want the raw string", raw)
	}
}

// Behavioral: two addresses in the same /64 drain ONE bucket; a third
// /64 is unaffected.
func TestLoginRateLimiterSharesBucketAcrossSlash64(t *testing.T) {
	l := newLoginRateLimiter(1, 1) // one attempt, then dry
	key := func(remoteAddr string) string {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		r.RemoteAddr = remoteAddr
		return loginBucketKey(r)
	}

	if !l.allow(key("[2001:db8:1:2::1]:1111")) {
		t.Fatal("first attempt within the /64 should be allowed")
	}
	if l.allow(key("[2001:db8:1:2::2]:2222")) {
		t.Error("a rotated address in the same /64 got a fresh bucket — the /64 aggregation is not in effect")
	}
	if !l.allow(key("[2001:db8:9:9::1]:3333")) {
		t.Error("a different /64 must have its own bucket")
	}
}

// ---------------------------------------------------------------------------
// Red-team fix (2026-09-12): control-plane inflight semaphore.
// ---------------------------------------------------------------------------

// With the one permit held by a blocked request, the next control-plane
// request is refused 503 immediately rather than queueing; releasing the
// blocker lets traffic through again.
func TestControlPlaneInflightCapRefusesExcess(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(capControlPlaneInflight(1, next))
	t.Cleanup(srv.Close)

	first := make(chan int, 1)
	go func() {
		resp, err := srv.Client().Get(srv.URL + "/api/v1/version")
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
		first <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never entered the handler")
	}

	resp, err := srv.Client().Get(srv.URL + "/api/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second request over the cap = %d, want 503", resp.StatusCode)
	}

	close(release)
	if got := <-first; got != http.StatusOK {
		t.Errorf("first request = %d, want 200", got)
	}
}

// The semaphore wraps ONLY the control-plane routes: cluster-host traffic
// is dispatched by HostGateway one layer out and governed by the gateway's
// own inflight cap, so two concurrent proxied requests both reach the
// upstream even with the control-plane cap at one permit.
func TestControlPlaneInflightCapDoesNotTouchGatewayPath(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayState(registry, nil)
	notGateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	// The production layer order: gateway OUTSIDE the control-plane cap.
	srv := httptest.NewServer(gw.HostGateway(capControlPlaneInflight(1, notGateway)))
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Host = "ray.cluster.test"
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	// Both must be in flight upstream simultaneously — impossible if the
	// one-permit control-plane semaphore gated the gateway path.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d proxied requests reached the upstream; the control-plane cap must not gate the gateway", i)
		}
	}
	close(release)
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("proxied request %d = %d, want 200", i, s)
		}
	}
}
