package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// wsUpstreamOpts configures wsUpstream's behavior.
type wsUpstreamOpts struct {
	// delay, when set, is slept before Accept is called — used to force
	// a southbound connect timeout deterministically.
	delay time.Duration
	// onAccept, when set, replaces the default echo loop entirely.
	onAccept func(t *testing.T, c *websocket.Conn)
	// capturedHeader receives the *http.Request the fake upstream saw
	// (headers only matter; body is empty for a ws upgrade).
	capturedHeader *atomic.Pointer[http.Header]
}

// wsUpstream starts a fake Ray dashboard websocket endpoint.
func wsUpstream(t *testing.T, opts wsUpstreamOpts) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.capturedHeader != nil {
			h := r.Header.Clone()
			opts.capturedHeader.Store(&h)
		}
		if opts.delay > 0 {
			time.Sleep(opts.delay)
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		if opts.onAccept != nil {
			opts.onAccept(t, c)
			return
		}
		// Default: echo every message back until the connection closes.
		ctx := context.Background()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gatewayWSServer wraps gw.HostGateway in an httptest.Server, mirroring
// gateway_test.go's HTTP-side helpers (e.g. TestGatewayEnforcesMaxInflight).
func gatewayWSServer(t *testing.T, gw *GatewayState) *httptest.Server {
	t.Helper()
	notGateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(gw.HostGateway(notGateway))
	t.Cleanup(srv.Close)
	return srv
}

// dialThroughGateway performs a real client-side websocket handshake
// through gwSrv, routed to hostname via DialOptions.Host (the gateway
// only inspects the Host header, not the actual TCP destination).
func dialThroughGateway(t *testing.T, gwSrv *httptest.Server, hostname string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws://" + strings.TrimPrefix(gwSrv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Host:       hostname,
		HTTPHeader: header,
	})
}

// attemptRawWSUpgrade issues a plain HTTP request carrying the minimal
// websocket-upgrade headers, for asserting failure-path status codes
// (connect timeout / unreachable / bad token) where the handshake never
// reaches 101 and coder/websocket's Dial would just return an opaque
// error — this gives us the actual HTTPError status + body.
func attemptRawWSUpgrade(t *testing.T, rawURL, hostname string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = hostname
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// settledGoroutines returns runtime.NumGoroutine(), polling briefly to
// let just-stopped goroutines actually finish unwinding (Close()'s
// "unblocks all goroutines... once complete" is a happens-before for the
// method call, not for the OS scheduler running the rest of the stack).
func settledGoroutines(t *testing.T) int {
	t.Helper()
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	return runtime.NumGoroutine()
}

// assertNoGoroutineLeak polls until NumGoroutine returns to at most
// `before`, or fails after a bounded wait.
func assertNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := settledGoroutines(t)
		if n <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d after=%d (buf=%d)", before, n, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// southboundWSURL — unit tests
// ---------------------------------------------------------------------------

func TestSouthboundWSURLSchemeConversion(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		path    string
		want    string
		wantErr bool
	}{
		{"https to wss", "https://ray.internal:8265", "/api/jobs/x/logs/tail", "wss://ray.internal:8265/api/jobs/x/logs/tail", false},
		{"http to ws", "http://ray.internal:8265", "/api/jobs/x/logs/tail", "ws://ray.internal:8265/api/jobs/x/logs/tail", false},
		{"trailing slash trimmed", "http://ray.internal:8265/", "/api/jobs/x/logs/tail", "ws://ray.internal:8265/api/jobs/x/logs/tail", false},
		{"query preserved", "http://ray.internal:8265", "/api/jobs/x/logs/tail?since=0", "ws://ray.internal:8265/api/jobs/x/logs/tail?since=0", false},
		{"already-ws base passes through", "ws://ray.internal:8265", "/logs", "ws://ray.internal:8265/logs", false},
		{"empty base errors", "", "/logs", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := southboundWSURL(tc.base, u)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got url %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("southboundWSURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bridge behavior — bidirectional relay, both text and binary.
// ---------------------------------------------------------------------------

func TestWSBridgeRelaysBothDirections(t *testing.T) {
	upstream := wsUpstream(t, wsUpstreamOpts{})
	registry := testRegistry("ray.cluster.test", upstream.URL, "cluster-secret-token")
	gw := NewGatewayState(registry, nil)
	gwSrv := gatewayWSServer(t, gw)

	// Captured after both httptest.Servers are up (their own accept-loop
	// goroutines are steady-state, not bridge-related) and before the
	// dial, so it isolates goroutines the bridge itself creates.
	before := settledGoroutines(t)

	conn, resp, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.Write(ctx, websocket.MessageText, []byte("hello upstream")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageText || string(data) != "hello upstream" {
		t.Fatalf("echo = (%v, %q), want (Text, %q)", typ, data, "hello upstream")
	}

	if err := conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	typ2, data2, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read binary echo: %v", err)
	}
	if typ2 != websocket.MessageBinary || len(data2) != 4 || data2[3] != 4 {
		t.Fatalf("binary echo = (%v, %v)", typ2, data2)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "")
	assertNoGoroutineLeak(t, before)
}

// The Ray job log tail pushes lines the client never asked for; the
// gateway must relay upstream->client traffic that isn't triggered by a
// prior client message.
func TestWSBridgeRelaysUnsolicitedUpstreamPush(t *testing.T) {
	sent := make(chan struct{})
	upstream := wsUpstream(t, wsUpstreamOpts{onAccept: func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		if err := c.Write(ctx, websocket.MessageText, []byte("mobula-contract-ok\n")); err != nil {
			return
		}
		close(sent)
		// Keep the connection open until the client goes away.
		_, _, _ = c.Read(ctx)
	}})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayState(registry, nil)
	gwSrv := gatewayWSServer(t, gw)

	before := settledGoroutines(t)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read unsolicited push: %v", err)
	}
	if string(data) != "mobula-contract-ok\n" {
		t.Fatalf("got %q", data)
	}
	<-sent

	_ = conn.Close(websocket.StatusNormalClosure, "")
	assertNoGoroutineLeak(t, before)
}

// ---------------------------------------------------------------------------
// Security invariant: strip-and-swap holds for the websocket path too.
// ---------------------------------------------------------------------------

func TestWSBridgeStripsCallerCredentialInjectsClusterToken(t *testing.T) {
	var captured atomic.Pointer[http.Header]
	upstream := wsUpstream(t, wsUpstreamOpts{capturedHeader: &captured})
	registry := testRegistry("ray.cluster.test", upstream.URL, "cluster-secret-token")
	gw := NewGatewayState(registry, nil)
	gwSrv := gatewayWSServer(t, gw)

	callerHeader := http.Header{}
	callerHeader.Set("Authorization", "Bearer caller-own-jwt-must-never-reach-cluster")
	callerHeader.Set("Cookie", "session=super-secret")

	conn, resp, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", callerHeader)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	// Northbound: the handshake response accepted back to the caller
	// must never carry the cluster's token (Accept never sets
	// Authorization at all — this pins that it doesn't leak some other
	// way, e.g. via a stray header echo).
	if resp.Header.Get("Authorization") != "" {
		t.Error("northbound handshake response must never carry an Authorization header")
	}

	got := captured.Load()
	if got == nil {
		t.Fatal("upstream never saw the handshake request")
	}
	southboundAuth := got.Get("Authorization")
	if southboundAuth != "Bearer cluster-secret-token" {
		t.Errorf("southbound Authorization = %q, want the cluster's static token", southboundAuth)
	}
	if strings.Contains(southboundAuth, "caller-own-jwt") {
		t.Fatal("the caller's own credential reached the cluster — ADR-0003 violation")
	}
	if got.Get("Cookie") != "" {
		t.Errorf("Cookie must never reach the cluster, got %q", got.Get("Cookie"))
	}
}

// The bridge-open audit row (method "WS") is emitted with the caller's
// subject and the resolved cluster, matching gateway.rs's ws.rs:489-503.
func TestWSBridgeEmitsAllowAuditRow(t *testing.T) {
	upstream := wsUpstream(t, wsUpstreamOpts{})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	store := controller.NewMemoryStore()
	gw := NewGatewayState(registry, store)
	gwSrv := gatewayWSServer(t, gw)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	rows, _, err := store.ListAudit(context.Background(), core.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.Event.Method != nil && *row.Event.Method == "WS" {
			found = true
			if row.Event.Decision != core.AuditDecisionAllow {
				t.Errorf("decision = %v, want allow", row.Event.Decision)
			}
			if row.Event.Cluster == nil || *row.Event.Cluster != "c1" {
				t.Errorf("cluster = %v, want c1", row.Event.Cluster)
			}
		}
	}
	if !found {
		t.Fatal("no WS audit row found")
	}
}

// ---------------------------------------------------------------------------
// Limits: connect timeout, unreachable cluster, max message size.
// ---------------------------------------------------------------------------

func TestWSBridgeConnectTimeoutAnswers504(t *testing.T) {
	block := make(chan struct{})

	// A handler that never responds at all (never even completes the
	// websocket handshake) — the southbound dial hangs until the
	// gateway's own WSConnectTimeout context fires, which is what's
	// under test. t.Cleanup runs LIFO, so register hang.Close before
	// close(block): the handler must be unblocked (closing block) BEFORE
	// hang.Close() waits for its still-active connection to finish, or
	// Close() deadlocks against the still-blocked handler goroutine.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(block) })

	registry := testRegistry("ray.cluster.test", hang.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{
		MaxInflight:      4,
		WSConnectTimeout: 100 * time.Millisecond,
		WSIdleTimeout:    time.Minute,
	})
	gwSrv := gatewayWSServer(t, gw)

	resp := attemptRawWSUpgrade(t, gwSrv.URL+"/logs/tail", "ray.cluster.test")
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
}

func TestWSBridgeUnreachableClusterAnswers502(t *testing.T) {
	registry := testRegistry("ray.cluster.test", "http://127.0.0.1:1", "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{
		MaxInflight:      4,
		WSConnectTimeout: 2 * time.Second,
		WSIdleTimeout:    time.Minute,
	})
	gwSrv := gatewayWSServer(t, gw)

	resp := attemptRawWSUpgrade(t, gwSrv.URL+"/logs/tail", "ray.cluster.test")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestWSBridgeEnforcesMaxMessageBytes(t *testing.T) {
	received := make(chan []byte, 1)
	upstream := wsUpstream(t, wsUpstreamOpts{onAccept: func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		_, data, err := c.Read(ctx)
		if err == nil {
			received <- data
		}
	}})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{
		MaxInflight:       4,
		WSConnectTimeout:  2 * time.Second,
		WSIdleTimeout:     time.Minute,
		WSMaxMessageBytes: 16,
	})
	gwSrv := gatewayWSServer(t, gw)

	before := settledGoroutines(t)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oversize := make([]byte, 64)
	// Write may itself report an error once the peer closes concurrently;
	// what matters is that the message never reaches upstream and the
	// client connection ends up closed.
	_ = conn.Write(ctx, websocket.MessageText, oversize)

	select {
	case <-received:
		t.Fatal("oversize message reached the upstream cluster — WSMaxMessageBytes not enforced")
	case <-time.After(300 * time.Millisecond):
	}

	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected the bridge to close the connection after an oversize message")
	}
	assertNoGoroutineLeak(t, before)
}

// ---------------------------------------------------------------------------
// Idle timeout and close propagation.
// ---------------------------------------------------------------------------

func TestWSBridgeIdleTimeoutClosesBridge(t *testing.T) {
	upstreamClosed := make(chan struct{})
	upstream := wsUpstream(t, wsUpstreamOpts{onAccept: func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = c.Read(ctx) // blocks until the gateway tears the bridge down on idle
		close(upstreamClosed)
	}})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{
		MaxInflight:      4,
		WSConnectTimeout: 2 * time.Second,
		WSIdleTimeout:    150 * time.Millisecond,
	})
	gwSrv := gatewayWSServer(t, gw)

	before := settledGoroutines(t)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("expected the connection to close on idle timeout")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("closed after %v, before the idle timeout elapsed", elapsed)
	} else if elapsed > 3*time.Second {
		t.Errorf("closed after %v, far later than the idle timeout", elapsed)
	}

	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream side of the bridge was never torn down by the idle timeout")
	}
	assertNoGoroutineLeak(t, before)
}

func TestWSBridgeClientCloseTearsDownUpstream(t *testing.T) {
	upstreamSawClose := make(chan struct{})
	upstream := wsUpstream(t, wsUpstreamOpts{onAccept: func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = c.Read(ctx) // errors once the gateway closes this side, following the client's close
		close(upstreamSawClose)
	}})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayState(registry, nil)
	gwSrv := gatewayWSServer(t, gw)

	before := settledGoroutines(t)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}

	if err := conn.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("client close: %v", err)
	}

	select {
	case <-upstreamSawClose:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the client side never propagated to the upstream side")
	}
	assertNoGoroutineLeak(t, before)
}

// ---------------------------------------------------------------------------
// Inflight permit: shared with HTTP, held for the bridge's whole lifetime.
// ---------------------------------------------------------------------------

func TestWSBridgeHoldsInflightPermitForBridgeLifetime(t *testing.T) {
	upstream := wsUpstream(t, wsUpstreamOpts{})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayStateWithLimits(registry, nil, GatewayLimits{
		MaxBodyBytes:     1024,
		MaxInflight:      1,
		WSConnectTimeout: 2 * time.Second,
		WSIdleTimeout:    time.Minute,
	})
	gwSrv := gatewayWSServer(t, gw)

	conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
	if err != nil {
		t.Fatalf("dial through gateway: %v", err)
	}

	// The one permit is held by the open bridge: a concurrent plain HTTP
	// proxied request must be refused with 503, proving the semaphore is
	// shared across the HTTP and websocket paths (#31).
	req, err := http.NewRequest(http.MethodGet, gwSrv.URL+"/api/jobs/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ray.cluster.test"
	resp, err := gwSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("concurrent HTTP request while WS bridge open: status = %d, want 503", resp.StatusCode)
	}

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}

	// Give HostGateway's `defer release()` a moment to run once
	// proxyUpgrade (called synchronously from within it) returns.
	var resp2 *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		req2, _ := http.NewRequest(http.MethodGet, gwSrv.URL+"/api/jobs/", nil)
		req2.Host = "ray.cluster.test"
		resp2, err = gwSrv.Client().Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		if resp2.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
			break
		}
		_ = resp2.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}
	_ = resp2.Body.Close()
	// wsUpstream only implements the websocket handshake, so a plain GET
	// naturally gets a 426 from it (Upgrade Required) rather than a 200 —
	// what matters here is that it's no longer 503, proving the gateway
	// itself let the request through, i.e. the permit was released.
	if resp2.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("after the bridge closed: status = %d, permit was never released", resp2.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// No goroutine leak, run under -race per the task brief.
// ---------------------------------------------------------------------------

func TestWSBridgeManyOpenCloseCyclesNoLeak(t *testing.T) {
	upstream := wsUpstream(t, wsUpstreamOpts{})
	registry := testRegistry("ray.cluster.test", upstream.URL, "tok")
	gw := NewGatewayState(registry, nil)
	gwSrv := gatewayWSServer(t, gw)

	before := settledGoroutines(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := dialThroughGateway(t, gwSrv, "ray.cluster.test", nil)
			if err != nil {
				t.Error(err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
				t.Error(err)
				return
			}
			if _, _, err := conn.Read(ctx); err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}()
	}
	wg.Wait()

	assertNoGoroutineLeak(t, before)
}
