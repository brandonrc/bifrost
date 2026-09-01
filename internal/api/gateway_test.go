package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// testRegistry builds a one-cluster ClusterRegistry routing hostname to
// upstreamURL with the given static Ray auth token.
func testRegistry(hostname, upstreamURL, clusterToken string) *core.ClusterRegistry {
	token := clusterToken
	return &core.ClusterRegistry{Clusters: []core.ClusterEndpoint{{
		Id:         core.ClusterId("c1"),
		Hostname:   hostname,
		ApiBaseUrl: upstreamURL,
		AuthToken:  &token,
	}}}
}

// newRecordingUpstream starts a fake Ray dashboard/job API that answers
// every request with (status, body, headers) and records the last
// request it received (headers + body) for the caller to inspect.
func newRecordingUpstream(t *testing.T, status int, body []byte, headers map[string]string) (srv *httptest.Server, lastReq func() *http.Request, lastBody func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var gotReq *http.Request
	var gotBody []byte
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotReq = r.Clone(r.Context())
		gotBody = b
		mu.Unlock()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv,
		func() *http.Request { mu.Lock(); defer mu.Unlock(); return gotReq },
		func() []byte { mu.Lock(); defer mu.Unlock(); return gotBody }
}

// newLocalRoleToken issues a local PAT for a fresh user holding exactly
// one auth.Role (via core.LocalRole's 1:1 mapping — see
// internal/auth/local.go's localRoleToRole).
func newLocalRoleToken(t *testing.T, username string, role core.LocalRole) (*auth.LocalAuthenticator, string) {
	t.Helper()
	store := newFakeUserStore()
	store.users[username] = &core.LocalUserRecord{Username: username, Role: role}
	local := auth.NewLocalAuthenticator(store, 3600, 30)
	minted, _, err := local.IssueToken(context.Background(), username, "test", 1)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return local, minted.Token
}

// ---------------------------------------------------------------------------
// Unit tests ported from gateway.rs's `mod tests` (everything except the
// websocket bridge, which is out of scope for T13).
// ---------------------------------------------------------------------------

// Port of southbound_strips_host_auth_and_hop_by_hop.
func TestSouthboundGatewayHeadersStripsHostAuthAndHopByHop(t *testing.T) {
	inbound := http.Header{}
	inbound.Set("Host", "demo.ray.test")
	inbound.Set("Authorization", "Bearer user-jwt")
	inbound.Set("Connection", "keep-alive")
	inbound.Set("Transfer-Encoding", "chunked")
	inbound.Set("Content-Length", "42")
	inbound.Set("Content-Type", "application/json")
	inbound.Set("X-Request-Id", "abc123")

	out := southboundGatewayHeaders(inbound)
	if out.Get("Host") != "" {
		t.Error("Host must be stripped")
	}
	if out.Get("Authorization") != "" {
		t.Error("Authorization must be stripped")
	}
	if out.Get("Connection") != "" {
		t.Error("Connection must be stripped")
	}
	if out.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding must be stripped")
	}
	if out.Get("Content-Length") != "" {
		t.Error("Content-Length must be stripped")
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := out.Get("X-Request-Id"); got != "abc123" {
		t.Errorf("X-Request-Id = %q, want abc123", got)
	}
}

// Port of southbound_strips_cookie_and_forwarded.
func TestSouthboundGatewayHeadersStripsCookieAndForwarded(t *testing.T) {
	inbound := http.Header{}
	inbound.Set("Cookie", "session=abc")
	inbound.Set("X-Forwarded-For", "1.2.3.4")
	inbound.Set("X-Forwarded-Host", "evil.example")
	inbound.Set("X-Forwarded-Proto", "http")
	inbound.Set("Forwarded", "for=1.2.3.4")
	inbound.Set("Content-Type", "application/json")

	out := southboundGatewayHeaders(inbound)
	for _, name := range []string{"Cookie", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if out.Get(name) != "" {
			t.Errorf("%s must be stripped", name)
		}
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// Port of southbound_preserves_repeated_headers.
func TestSouthboundGatewayHeadersPreservesRepeatedHeaders(t *testing.T) {
	inbound := http.Header{}
	inbound.Add("Accept-Encoding", "gzip")
	inbound.Add("Accept-Encoding", "br")

	out := southboundGatewayHeaders(inbound)
	if got := out.Values("Accept-Encoding"); len(got) != 2 {
		t.Errorf("Accept-Encoding values = %v, want 2 entries", got)
	}
}

// Port of hop_by_hop_classification.
func TestGatewayHopByHopClassification(t *testing.T) {
	for _, name := range []string{
		"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade",
	} {
		if !isGatewayHopByHop(name) {
			t.Errorf("%s should be hop-by-hop", name)
		}
	}
	for _, name := range []string{"content-type", "accept", "x-anything"} {
		if isGatewayHopByHop(name) {
			t.Errorf("%s should NOT be hop-by-hop", name)
		}
	}
}

// Port of websocket_upgrade_detection_is_case_insensitive.
func TestIsWebsocketUpgradeIsCaseInsensitive(t *testing.T) {
	h := func(v string) http.Header {
		hh := http.Header{}
		if v != "" {
			hh.Set("Upgrade", v)
		}
		return hh
	}
	if !isWebsocketUpgrade(h("WebSocket")) {
		t.Error("WebSocket should be detected")
	}
	if !isWebsocketUpgrade(h("websocket")) {
		t.Error("websocket should be detected")
	}
	if isWebsocketUpgrade(h("h2c")) {
		t.Error("h2c should not be detected as websocket")
	}
	if isWebsocketUpgrade(h("")) {
		t.Error("no Upgrade header should not be detected as websocket")
	}
}

// Port of gateway_error_status_codes.
func TestGatewayErrorStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  HTTPError
		want int
	}{
		{"BodyTooLarge", errGatewayBodyTooLarge, http.StatusRequestEntityTooLarge},
		{"BadToken", errGatewayBadToken, http.StatusInternalServerError},
		{"Upstream", errGatewayUpstream, http.StatusBadGateway},
	}
	for _, c := range cases {
		if c.err.Status != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, c.err.Status, c.want)
		}
	}
}

// Value-pins DefaultGatewayLimits against gateway.rs's constants so a
// silent edit can't drift the production posture without a test failing.
func TestDefaultGatewayLimitsMatchRustReference(t *testing.T) {
	got := DefaultGatewayLimits()
	want := GatewayLimits{
		MaxBodyBytes:      64 * 1024 * 1024,
		MaxInflight:       64,
		WSConnectTimeout:  15 * time.Second,
		WSIdleTimeout:     300 * time.Second,
		WSMaxFrameBytes:   4 * 1024 * 1024,
		WSMaxMessageBytes: 16 * 1024 * 1024,
	}
	if got != want {
		t.Errorf("DefaultGatewayLimits() = %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// hostIsCluster / requiredGatewayPermission (middleware.go, T13)
// ---------------------------------------------------------------------------

func TestHostIsCluster(t *testing.T) {
	registry := testRegistry("ray.cluster.test", "http://example.invalid", "tok")

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Host = "ray.cluster.test"
	if !hostIsCluster(registry, req) {
		t.Error("registered hostname should match")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/x", nil)
	req2.Host = "control-plane.example.test"
	if hostIsCluster(registry, req2) {
		t.Error("an unregistered hostname must not match")
	}

	req3 := httptest.NewRequest(http.MethodGet, "/x", nil)
	req3.Host = "ray.cluster.test"
	if hostIsCluster(nil, req3) {
		t.Error("a nil registry must never match (gateway disabled)")
	}
}

func TestRequiredGatewayPermission(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if requiredGatewayPermission(m) != auth.Read {
			t.Errorf("%s should require Read", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if requiredGatewayPermission(m) != auth.Write {
			t.Errorf("%s should require Write", m)
		}
	}
}

// authorizeGatewayRequest's denial audit row must carry Method and Path
// (core.AuditEvent's doc comment: "for gateway and authn/ext_authz
// rows") — the field this file deliberately does NOT reuse authz.go's
// shared Authorize/emitAuthzDenial for (see authorizeGatewayRequest's
// doc comment in middleware.go).
func TestAuthorizeGatewayRequestDenialAuditsMethodAndPath(t *testing.T) {
	buf := captureLogs(t)
	identity := &auth.Identity{Subject: "auditor-1", Roles: []auth.Role{auth.RoleAuditor}}
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/", nil)

	err := authorizeGatewayRequest(nil, identity, req)
	if err != ErrForbidden {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}

	logged := buf.String()
	for _, want := range []string{
		"decision=deny", "reason=insufficient_permission",
		"method=GET", "path=/api/jobs/",
		"required_action=read", "required_target=job",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q, got: %s", want, logged)
		}
	}
}

func TestAuthorizeGatewayRequestAllowsPermittedRole(t *testing.T) {
	identity := &auth.Identity{Subject: "dev-1", Roles: []auth.Role{auth.RoleDeveloper}}
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/", nil)
	if err := authorizeGatewayRequest(nil, identity, req); err != nil {
		t.Fatalf("developer POST to the job surface: unexpected error %v", err)
	}
}

// ---------------------------------------------------------------------------
// Invariant 1: host_is_cluster — a cluster hostname is never public and
// never shadowed by the control plane.
// ---------------------------------------------------------------------------

// The mandatory case from the task brief: GET /healthz on a cluster host
// must NOT be answered by the control plane's own (public, unauthenticated)
// /healthz — it must require a token, and once authenticated, be proxied
// to the cluster's Ray surface.
func TestHostIsClusterSuppressesPublicAllowlist(t *testing.T) {
	upstream, lastReq, _ := newRecordingUpstream(t, http.StatusOK, []byte(`{"ok":true}`), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "cluster-secret-token")
	local, token := newLocalRoleToken(t, "dev", core.LocalRoleDeveloper)

	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// No token at all, exact control-plane public path, cluster host:
	// must be 401, and the upstream cluster must never see it.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ray.cluster.test"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /healthz on cluster host, no token: status = %d, want 401 (public allowlist must be suppressed)", resp.StatusCode)
	}
	if lastReq() != nil {
		t.Error("an unauthenticated request must never reach the upstream cluster")
	}

	// Same path, same host, WITH a valid token holding Target::Job Read:
	// it must now be proxied to the Ray surface, not answered locally.
	req2, err := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.Host = "ray.cluster.test"
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /healthz on cluster host: status = %d, want 200, body=%s", resp2.StatusCode, body)
	}
	if lastReq() == nil {
		t.Fatal("expected the request to reach the upstream cluster (proxied), not the control plane's own /healthz")
	}
	if lastReq().URL.Path != "/healthz" {
		t.Errorf("upstream path = %q, want /healthz", lastReq().URL.Path)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want the upstream's response body (proof it was proxied)", body)
	}
}

func TestHostIsClusterDeniesInsufficientPermission(t *testing.T) {
	upstream, lastReq, _ := newRecordingUpstream(t, http.StatusOK, []byte("ok"), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	// Auditor holds no Job permission at all (Grants returns false for
	// every action on TargetJob).
	local, token := newLocalRoleToken(t, "auditor", core.LocalRoleAuditor)

	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/", nil)
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
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("auditor role on cluster host: status = %d, want 403", resp.StatusCode)
	}
	if lastReq() != nil {
		t.Error("a denied request must never reach the upstream cluster")
	}
}

// Viewer holds Read but not Write on Job: GET proxies through, POST is
// denied — proving the method-derived Read/Write split actually gates
// the gateway, not just authentication.
func TestHostIsClusterEnforcesReadWriteSplitByMethod(t *testing.T) {
	upstream, _, _ := newRecordingUpstream(t, http.StatusOK, []byte("ok"), nil)
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	local, token := newLocalRoleToken(t, "viewer", core.LocalRoleViewer)

	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/", nil)
	get.Host = "ray.cluster.test"
	get.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(get)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("viewer GET: status = %d, want 200", resp.StatusCode)
	}

	post, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/jobs/", strings.NewReader("{}"))
	post.Host = "ray.cluster.test"
	post.Header.Set("Authorization", "Bearer "+token)
	resp2, err := srv.Client().Do(post)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("viewer POST: status = %d, want 403", resp2.StatusCode)
	}
}

// An unregistered Host is not a gateway request at all: it must fall
// through unchanged to the ordinary control-plane router (public paths
// still public, unmatched paths still 404), never treated as a gateway
// failure.
func TestUnknownHostFallsThroughToControlPlane(t *testing.T) {
	registry := testRegistry("ray.cluster.test", "http://127.0.0.1:1", "tok")
	h := NewHandler(NewServer(), HandlerOptions{Registry: registry, AllowUnauthenticated: true})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "bifrost" {
		t.Errorf(`name = %v, want "bifrost"`, got["name"])
	}

	resp2, err := srv.Client().Get(srv.URL + "/no/such/path")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unmatched path on a non-gateway host: status = %d, want 404", resp2.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Invariant 2: credential strip-and-swap (ADR-0003), both directions.
// ---------------------------------------------------------------------------

func TestProxyStripsCallerCredentialAndInjectsClusterToken(t *testing.T) {
	var gotAuth, gotCookie, gotXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Server", "ray-dashboard/2.9.0")
		w.Header().Set("Location", "http://internal-ray-head.svc.cluster.local:8265/foo")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	registry := testRegistry("ray.cluster.test", upstream.URL, "cluster-secret-token")
	local, callerToken := newLocalRoleToken(t, "dev", core.LocalRoleDeveloper)

	h := NewHandler(NewServer(), HandlerOptions{Local: local, Registry: registry})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ray.cluster.test"
	req.Header.Set("Authorization", "Bearer "+callerToken)
	req.Header.Set("Cookie", "session=super-secret-session-id")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}

	// Southbound: the caller's own token must never reach the cluster —
	// only the cluster's static Ray token does.
	if gotAuth != "Bearer cluster-secret-token" {
		t.Errorf("southbound Authorization = %q, want the cluster's static token", gotAuth)
	}
	if strings.Contains(gotAuth, callerToken) {
		t.Fatal("the caller's own PAT reached the cluster — ADR-0003 violation")
	}
	if gotCookie != "" {
		t.Errorf("Cookie must never reach the cluster, got %q", gotCookie)
	}
	if gotXFF != "" {
		t.Errorf("X-Forwarded-For must never reach the cluster, got %q", gotXFF)
	}

	// Northbound: nothing about the cluster's own credential or internal
	// topology leaks back to the caller.
	if resp.Header.Get("Server") != "" {
		t.Error("Server header must be stripped northbound (#32)")
	}
	if resp.Header.Get("Location") != "" {
		t.Error("Location header must be stripped northbound (#32)")
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want \"ok\"", body)
	}
}

// ---------------------------------------------------------------------------
// Invariant 3: GatewayLimits — body cap and inflight cap.
// ---------------------------------------------------------------------------

func TestGatewayEnforcesMaxBodyBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{MaxBodyBytes: 8, MaxInflight: 4})
	notGateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	gwSrv := httptest.NewServer(gw.HostGateway(notGateway))
	t.Cleanup(gwSrv.Close)

	ok, err := http.NewRequest(http.MethodPost, gwSrv.URL+"/api/jobs/", strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	ok.Host = "ray.cluster.test"
	resp, err := gwSrv.Client().Do(ok)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("body under the cap: status = %d, want 200", resp.StatusCode)
	}

	tooBig, err := http.NewRequest(http.MethodPost, gwSrv.URL+"/api/jobs/", strings.NewReader("this body is definitely more than eight bytes long"))
	if err != nil {
		t.Fatal(err)
	}
	tooBig.Host = "ray.cluster.test"
	resp2, err := gwSrv.Client().Do(tooBig)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the cap: status = %d, want 413", resp2.StatusCode)
	}
}

func TestGatewayEnforcesMaxInflight(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{MaxBodyBytes: 1024, MaxInflight: 1})
	notGateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	gwSrv := httptest.NewServer(gw.HostGateway(notGateway))
	t.Cleanup(gwSrv.Close)

	var wg sync.WaitGroup
	var firstStatus int
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodGet, gwSrv.URL+"/api/jobs/", nil)
		if err != nil {
			t.Error(err)
			return
		}
		req.Host = "ray.cluster.test"
		resp, err := gwSrv.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
		firstStatus = resp.StatusCode
	}()

	// Wait for the first request to actually be holding the one permit
	// (blocked inside the fake upstream) before firing the second.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the upstream")
	}

	req2, err := http.NewRequest(http.MethodGet, gwSrv.URL+"/api/jobs/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.Host = "ray.cluster.test"
	resp2, err := gwSrv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent request over MaxInflight: status = %d, want 503", resp2.StatusCode)
	}

	close(release)
	wg.Wait()
	if firstStatus != http.StatusOK {
		t.Errorf("first request: status = %d, want 200", firstStatus)
	}
}

// T14 replaced the 501 seam (proxyUpgradeStub) with the real websocket
// bridge (proxyUpgrade, gateway_ws.go) — see gateway_ws_test.go for its
// tests, including the "websocket upgrade to a registered cluster host
// reaches the upstream, not a 501" case this file used to pin.
