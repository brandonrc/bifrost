// Federating gateway websocket bridge (Wave 1 T14, ADR-0002/ADR-0003).
// Ported from mobula-api's gateway.rs's `mod ws` (gateway.rs:399-597):
// Ray's job log tail (`.../logs/tail`) is a websocket endpoint served by
// the cluster's dashboard head, and the gateway bridges it end to end
// with the same credential swap as the plain-HTTP proxy in gateway.go.
//
// HostGateway (gateway.go) has already resolved the cluster and holds the
// shared inflight permit by the time proxyUpgrade runs; proxyUpgrade
// blocks for the bridge's entire lifetime, so HostGateway's `defer
// release()` naturally holds the permit for as long as the bridge is
// open — exactly gateway.rs's `_permit: OwnedSemaphorePermit` moved into
// `bridge()`. No changes to HostGateway's dispatch/permit/limit
// composition were needed for this task; see gateway.go's HostGateway
// doc comment.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// proxyUpgrade bridges a websocket upgrade request to cluster's native Ray
// websocket endpoint (gateway.rs:414's proxy_upgrade). It:
//
//  1. builds the southbound ws(s):// URL from cluster.ApiBaseUrl plus the
//     caller's path+query;
//  2. dials the cluster BEFORE accepting the caller's upgrade, bounded by
//     Limits.WSConnectTimeout, so an unreachable cluster surfaces as a
//     normal HTTP 502/504 rather than a half-opened client socket
//     (gateway.rs's comment at ws.rs:462 — "Connect southbound BEFORE
//     accepting the client upgrade");
//  3. only then accepts the caller's upgrade and relays frames
//     bidirectionally until either side closes/errors or the bridge goes
//     idle for Limits.WSIdleTimeout.
//
// Strip-and-swap (ADR-0003): the southbound dial's only header is a
// freshly built Authorization carrying cluster.AuthToken — nothing from
// the caller's inbound request (including any Authorization it sent) is
// copied southbound at all, mirroring gateway.rs's proxy_upgrade, which
// builds the southbound request from scratch and never touches
// `parts.headers`. websocket.Accept (the northbound handshake) never sets
// Authorization either — Accept only ever emits
// Upgrade/Connection/Sec-WebSocket-Accept/-Protocol/-Extensions — so the
// cluster token can never echo back to the caller.
func (gw *GatewayState) proxyUpgrade(w http.ResponseWriter, r *http.Request, cluster *core.ClusterEndpoint) {
	identity, _ := IdentityFromContext(r.Context())
	subject := identitySubject(identity)

	wsURL, err := southboundWSURL(cluster.ApiBaseUrl, r.URL)
	if err != nil {
		slog.Warn("api: gateway could not build upstream websocket url", "cluster", cluster.Id, "error", err)
		WriteError(w, r, errGatewayUpstream)
		return
	}

	dialHeader := http.Header{}
	if cluster.AuthToken != nil {
		token := "Bearer " + *cluster.AuthToken
		if !validHeaderValue(token) {
			WriteError(w, r, errGatewayBadToken)
			return
		}
		dialHeader.Set("Authorization", token)
	}

	// Bound the southbound connect (#31): a black-holing cluster head
	// must not pin the caller's half-open upgrade indefinitely. dial()
	// (coder/websocket) only uses this context for the handshake — the
	// returned *Conn's later Read/Write calls take their own contexts —
	// so the bound does not leak onto the bridge's steady-state traffic.
	dialCtx, cancel := context.WithTimeout(r.Context(), gw.Limits.WSConnectTimeout)
	upstream, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: gw.Client,
		HTTPHeader: dialHeader,
	})
	cancel()
	if err != nil {
		if dialCtx.Err() != nil && errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
			slog.Warn("api: upstream websocket connect timed out", "cluster", cluster.Id)
			WriteError(w, r, errWSConnectTimeout)
			return
		}
		slog.Warn("api: upstream websocket connect failed", "cluster", cluster.Id)
		WriteError(w, r, errGatewayUpstream)
		return
	}
	upstream.SetReadLimit(gw.Limits.WSMaxMessageBytes)

	// websocket.Accept writes its own HTTP response on failure (it has
	// not hijacked yet), so on error we only need to tear down the
	// upstream we already opened.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		slog.Warn("api: client websocket upgrade failed", "cluster", cluster.Id, "error", err)
		_ = upstream.CloseNow()
		return
	}
	client.SetReadLimit(gw.Limits.WSMaxMessageBytes)

	// The bridge opening is an allowed gateway request (same policy as an
	// HTTP proxy row, gateway.rs's ws.rs:489 comment): there is no
	// status/latency to report until it closes, so this row carries
	// method "WS" and no Status/LatencyMs, matching the Rust reference.
	clusterID := cluster.Id.String()
	method := "WS"
	path := r.URL.Path
	EmitAudit(r.Context(), gw.Store, &core.AuditEvent{
		Ts:       controller.NowUnix(),
		Subject:  subject,
		Decision: core.AuditDecisionAllow,
		Cluster:  &clusterID,
		Method:   &method,
		Path:     &path,
	})

	wsBridge(client, upstream, gw.Limits.WSIdleTimeout)
}

// southboundWSURL builds the cluster-facing websocket URL from
// cluster.ApiBaseUrl (an http(s):// dashboard base) plus reqURL's
// path+query, mirroring gateway.rs's proxy_upgrade URL construction:
// https:// becomes wss://, http:// becomes ws://, anything else (e.g. a
// test fixture that's already ws(s)://) passes through unchanged.
func southboundWSURL(apiBaseURL string, reqURL *url.URL) (string, error) {
	if apiBaseURL == "" {
		return "", errors.New("empty cluster api_base_url")
	}
	base := strings.TrimRight(apiBaseURL, "/")
	var wsBase string
	switch {
	case strings.HasPrefix(base, "https://"):
		wsBase = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		wsBase = "ws://" + strings.TrimPrefix(base, "http://")
	default:
		wsBase = base
	}
	return wsBase + reqURL.RequestURI(), nil
}

// wsBridge relays frames both ways between client and upstream until
// either side closes/errors or the bridge sits idle for idleTimeout
// (gateway.rs's ws.rs:516 `bridge`). The inflight permit acquired by
// HostGateway is held for wsBridge's entire (synchronous, blocking)
// duration by construction — proxyUpgrade's caller (HostGateway) doesn't
// release it until this call returns — matching #31's "a websocket
// bridge holds its permit for the bridge's whole lifetime."
//
// coder/websocket auto-closes a Conn (with an appropriate close code)
// whenever any Read/Write on it errors — "On any error from any method,
// the connection is closed with an appropriate reason" (Conn's doc
// comment) — so a pump goroutine never needs to close its own `from`
// side; it only needs to make sure the *other* Conn also tears down,
// which is what the stop() closures below do. Conn.Close "unblocks all
// goroutines interacting with the connection once complete", so a
// concurrent Read blocked on either Conn returns promptly once stop()
// closes it — no goroutine leak.
//
// Known port gap: coder/websocket exposes only a message-level read cap
// (SetReadLimit) — it has no equivalent of tokio-tungstenite's separate
// max_frame_size, since Read/Reader transparently reassemble fragmented
// frames up to that cap. proxyUpgrade applies Limits.WSMaxMessageBytes
// (the more permissive of the Rust reference's two caps) as the one
// available bound; Limits.WSMaxFrameBytes has no enforcement point in
// this library and is not silently dropped — it stays a documented gap,
// consistent with GatewayLimits' own doc comment.
func wsBridge(client, upstream *websocket.Conn, idleTimeout time.Duration) {
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			_ = client.Close(websocket.StatusNormalClosure, "")
			_ = upstream.Close(websocket.StatusNormalClosure, "")
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())

	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(from, to *websocket.Conn) {
		defer wg.Done()
		for {
			typ, data, err := from.Read(ctx)
			if err != nil {
				// from is already closed (with its own appropriate close
				// code) by the library itself; tear down the other side
				// too so its blocked Read/Write also returns.
				stop()
				return
			}
			lastActivity.Store(time.Now().UnixNano())
			if err := to.Write(ctx, typ, data); err != nil {
				stop()
				return
			}
		}
	}
	go pump(client, upstream)
	go pump(upstream, client)

	// Idle watchdog (#31): any frame in either direction resets the
	// clock (gateway.rs's ws.rs:561-563 comment); no traffic for
	// idleTimeout tears the bridge down so a quiet tail doesn't hold its
	// semaphore permit (and sockets) forever. Polls at idleTimeout/10
	// (clamped to [10ms, 1s]) rather than a single one-shot timer so the
	// check is cheap in production (300s default → 30s ticks) while
	// staying responsive for tests that shrink idleTimeout.
	interval := idleTimeout / 10
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	} else if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastActivity.Load())) >= idleTimeout {
					stop()
					return
				}
			}
		}
	}()

	wg.Wait()
	cancel() // let the watchdog goroutine exit promptly once both pumps are done.
	<-watchdogDone
	stop() // no-op (sync.Once) if a pump or the watchdog already stopped the bridge.
}

// Gateway error responses specific to the websocket bridge (T14). Status
// codes match gateway.rs's inline `(StatusCode, msg)` responses in
// ws::proxy_upgrade exactly.
var errWSConnectTimeout = HTTPError{
	Status: http.StatusGatewayTimeout, Code: "gateway_timeout",
	Message: "cluster connect timed out",
}
