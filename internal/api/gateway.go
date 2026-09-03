// Federating job gateway (Wave 1 T13/T14, ADR-0002/ADR-0003). Ported from
// the Rust predecessor's gateway.rs — the HTTP proxy lives here; the websocket
// bridge (gateway.rs:399-597) is gateway_ws.go's proxyUpgrade.
//
// Requests whose Host header matches a registered cluster are proxied to
// that cluster's native Ray dashboard/job API, with the cluster's static
// Ray token injected southbound (ADR-0003) in place of the caller's own
// credential, which is stripped and never forwarded. Host matching runs
// as middleware (HostGateway, below) that server.go's NewHandler installs
// directly in front of route matching, so a cluster hostname can never be
// shadowed by a control-plane path — everything else falls through to
// next unchanged.
//
// Authentication and the Target::Job authorization check for cluster-host
// traffic do NOT live in this file: they run one layer out, in
// RequireAuth (middleware.go), which wraps HostGateway (see server.go's
// NewHandler and middleware.go's package doc comment) — mirroring
// the predecessor's lib.rs's layer order (auth outermost, gateway inner) and
// auth_layer.rs's require_auth, which performs the host_is_cluster check
// itself rather than delegating to gateway.rs. By the time a request
// reaches HostGateway it is already either authorized or arrived through
// dev mode (no AuthState configured at all).
package api

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// GatewayLimits are the federating gateway's hardening knobs, ported from
// gateway.rs's GatewayLimits (issues #30/#31). DefaultGatewayLimits is
// the production posture; tests shrink the values to exercise the caps
// deterministically.
//
// The WS* fields feed gateway_ws.go's proxyUpgrade/wsBridge (T14, #31).
type GatewayLimits struct {
	// MaxBodyBytes bounds a buffered proxied request body (#30): request
	// bodies are buffered before forwarding, not streamed — job
	// submissions are tiny and runtime-env uploads are modest, and
	// streaming passthrough is an acknowledged follow-up (gateway.rs's
	// doc comment says the same). Default: 64 MiB.
	MaxBodyBytes int64
	// MaxInflight bounds concurrent proxied requests — HTTP and websocket
	// bridges share the same semaphore (#30/#31): caps peak buffered-body
	// memory at roughly MaxInflight × MaxBodyBytes, and bounds how many
	// websocket bridges can be open at once. Default: 64.
	MaxInflight int64
	// WSConnectTimeout, WSIdleTimeout, WSMaxFrameBytes, and
	// WSMaxMessageBytes are the websocket-bridge knobs (#31) — see
	// gateway_ws.go's proxyUpgrade/wsBridge.
	WSConnectTimeout  time.Duration
	WSIdleTimeout     time.Duration
	WSMaxFrameBytes   int64
	WSMaxMessageBytes int64
}

// DefaultGatewayLimits is the production posture, values ported verbatim
// from gateway.rs's MAX_BODY_BYTES / MAX_INFLIGHT / WS_CONNECT_TIMEOUT /
// WS_IDLE_TIMEOUT / WS_MAX_FRAME_BYTES / WS_MAX_MESSAGE_BYTES constants.
func DefaultGatewayLimits() GatewayLimits {
	return GatewayLimits{
		MaxBodyBytes:      64 * 1024 * 1024,
		MaxInflight:       64,
		WSConnectTimeout:  15 * time.Second,
		WSIdleTimeout:     300 * time.Second,
		WSMaxFrameBytes:   4 * 1024 * 1024,
		WSMaxMessageBytes: 16 * 1024 * 1024,
	}
}

// GatewayState is the federating gateway's runtime state (gateway.rs's
// GatewayState): the routing table, optional audit persistence, the
// hardening limits, the southbound HTTP client (reverse-proxy posture —
// no redirects, bounded connect/read timeouts), and the inflight
// semaphore bounding concurrent proxied requests.
type GatewayState struct {
	Registry *core.ClusterRegistry
	// Store persists the gateway's per-request audit trail (api-v1.md
	// §5.9, EmitAudit's row for every proxied request — always
	// decision=allow, matching audit.rs's doc comment: a request Bifrost
	// refuses never reaches the gateway, so its deny row comes from the
	// refuser, not here). nil keeps rows trace-only.
	Store  controller.Store
	Limits GatewayLimits
	Client *http.Client

	inflight chan struct{}
}

// NewGatewayState builds a GatewayState with DefaultGatewayLimits().
func NewGatewayState(registry *core.ClusterRegistry, store controller.Store) *GatewayState {
	return NewGatewayStateWithLimits(registry, store, DefaultGatewayLimits())
}

// NewGatewayStateWithLimits is NewGatewayState with explicit
// GatewayLimits (gateway.rs's try_with_limits). Unlike the Rust
// reference, this is infallible: T13 does not port
// GatewayLimits.southbound_ca_bundle (operator-pinned CA bundles for
// self-signed cluster TLS) — the southbound client trusts only the
// system roots. That knob is a TLS-hardening feature orthogonal to the
// invariants this task ports (host dispatch, credential strip-and-swap,
// authz, body/inflight limits); it is a tracked gap, not a silent one.
func NewGatewayStateWithLimits(registry *core.ClusterRegistry, store controller.Store, limits GatewayLimits) *GatewayState {
	return &GatewayState{
		Registry: registry,
		Store:    store,
		Limits:   limits,
		Client:   buildSouthboundGatewayClient(limits.MaxInflight),
		inflight: make(chan struct{}, limits.MaxInflight),
	}
}

// buildSouthboundGatewayClient mirrors gateway.rs's
// build_southbound_client's reverse-proxy posture (issues #2/#3/#5):
// redirects are never followed — a 3xx is passed through untouched to
// the caller (its Location header is additionally stripped northbound,
// see northboundGatewayHeaders, since it can carry internal service
// names/IPs) — and connect/read are bounded so a hung or black-holing
// cluster head can't pin a connection indefinitely. Values match the
// Rust reference (10s connect, 120s read).
//
// maxInflight sizes the southbound connection pool (ADR-0005 finding):
// left at Go's http.Transport default (2 idle connections per host),
// every southbound request beyond the second concurrently in flight to
// the same cluster pays a fresh TCP handshake instead of reusing a
// pooled connection — under concurrent load that showed up in the
// gateway load rig as connection churn and a p99/max tail growing to
// tens of milliseconds, unrelated to request processing or GC. Since
// MaxInflight (gw.inflight) already hard-caps concurrent southbound
// requests per GatewayState, sizing the pool to it can never leave idle
// connections unused and removes the churn entirely.
func buildSouthboundGatewayClient(maxInflight int64) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	maxIdle := int(maxInflight)
	if maxIdle < 2 {
		maxIdle = 2
	}
	return &http.Client{
		// Go's http.Client.Timeout bounds the whole round trip (connect
		// through body read), not just idle-between-reads the way
		// reqwest's read_timeout does; this is the same approximation
		// cluster_obs.go's obsHTTPClient already makes for the sibling
		// southbound proxy, so the two southbound clients share one
		// posture.
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConns:        maxIdle,
			MaxIdleConnsPerHost: maxIdle,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// tryAcquire takes one of Limits.MaxInflight permits, bounding peak
// buffered-body memory and upstream fan-out (#30) — the same
// try_acquire_owned() semantics as gateway.rs's inflight semaphore:
// excess requests are refused immediately (503) rather than queueing
// unboundedly, since a queue behind the cap is itself a DoS surface.
func (gw *GatewayState) tryAcquire() (release func(), ok bool) {
	select {
	case gw.inflight <- struct{}{}:
		return func() { <-gw.inflight }, true
	default:
		return nil, false
	}
}

// clusterForHost resolves r's Host against the registry, or reports not
// found when no registry is configured at all (a gateway-less
// deployment).
func (gw *GatewayState) clusterForHost(host string) (core.ClusterEndpoint, bool) {
	if gw.Registry == nil {
		return core.ClusterEndpoint{}, false
	}
	return gw.Registry.ByHostname(host)
}

// HostGateway is the federating gateway's dispatch middleware
// (gateway.rs's host_gateway). It MUST be installed directly in front of
// route matching (server.go's NewHandler) so a cluster hostname can
// never be shadowed by a control-plane path: a request whose Host
// matches a registered cluster is proxied southbound here; every other
// request falls through to next (the control-plane mux) unchanged.
func (gw *GatewayState) HostGateway(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cluster, ok := gw.clusterForHost(r.Host)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		release, ok := gw.tryAcquire()
		if !ok {
			WriteError(w, r, errGatewayBusy)
			return
		}
		defer release()

		if isWebsocketUpgrade(r.Header) {
			gw.proxyUpgrade(w, r, &cluster)
			return
		}
		gw.proxy(w, r, &cluster)
	})
}

// isWebsocketUpgrade mirrors gateway.rs's is_websocket_upgrade: an
// Upgrade header case-insensitively equal to "websocket".
func isWebsocketUpgrade(h http.Header) bool {
	return strings.EqualFold(h.Get("Upgrade"), "websocket")
}

// proxy forwards a non-websocket request to cluster's native Ray API and
// relays the response back, exactly as gateway.rs's proxy() does:
// buffered (bounded) request body, southbound header stripping +
// credential swap, no-redirect southbound client, northbound header
// stripping, and a per-request allow audit row.
func (gw *GatewayState) proxy(w http.ResponseWriter, r *http.Request, cluster *core.ClusterEndpoint) {
	started := time.Now()
	identity, _ := IdentityFromContext(r.Context())
	subject := identitySubject(identity)

	method := r.Method
	path := r.URL.Path
	target := strings.TrimRight(cluster.ApiBaseUrl, "/") + r.URL.RequestURI()

	body, within, err := readCapped(r.Body, gw.Limits.MaxBodyBytes)
	if err != nil {
		slog.Warn("api: gateway request body read failed", "cluster", cluster.Id, "error", err)
		WriteError(w, r, errGatewayUpstream)
		return
	}
	if !within {
		WriteError(w, r, errGatewayBodyTooLarge)
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		slog.Warn("api: gateway could not build upstream request", "cluster", cluster.Id, "error", err)
		WriteError(w, r, errGatewayUpstream)
		return
	}
	outReq.Header = southboundGatewayHeaders(r.Header)
	if cluster.AuthToken != nil {
		token := "Bearer " + *cluster.AuthToken
		if !validHeaderValue(token) {
			WriteError(w, r, errGatewayBadToken)
			return
		}
		outReq.Header.Set("Authorization", token)
	}

	// without a URL embedded: an upstream transport error's message can
	// carry the full southbound URL including query — keep cluster
	// topology out of logs (#5), mirroring gateway.rs's `.without_url()`.
	resp, err := gw.Client.Do(outReq)
	if err != nil {
		slog.Warn("api: gateway upstream request failed", "cluster", cluster.Id)
		WriteError(w, r, errGatewayUpstream)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Append-only audit trail (issue #8, api-v1.md §5.9): every proxied
	// request, one row, always decision=allow — a request Bifrost
	// refuses never reaches here (RequireAuth emits the deny row); an
	// upstream 4xx/5xx is the cluster's own answer and lives in Status.
	status := uint16(resp.StatusCode)
	latencyMs := uint64(time.Since(started).Milliseconds())
	clusterID := cluster.Id.String()
	EmitAudit(r.Context(), gw.Store, &core.AuditEvent{
		Ts:        controller.NowUnix(),
		Subject:   subject,
		Decision:  core.AuditDecisionAllow,
		Cluster:   &clusterID,
		Method:    &method,
		Path:      &path,
		Status:    &status,
		LatencyMs: &latencyMs,
	})

	for name, values := range northboundGatewayHeaders(resp.Header) {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("api: gateway response copy failed", "cluster", cluster.Id, "error", err)
	}
}

// gatewayHopByHop is the static hop-by-hop header set (RFC 9110 §7.6.1),
// ported from gateway.rs's is_hop_by_hop.
var gatewayHopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func isGatewayHopByHop(lowerName string) bool { return gatewayHopByHop[lowerName] }

// isGatewayForwardedHeader reports whether lowerName is one of the
// X-Forwarded-* headers gateway.rs's southbound_headers strips (Forwarded
// itself is checked separately, matching the Rust reference's own
// separate `name == header::FORWARDED` check).
func isGatewayForwardedHeader(lowerName string) bool {
	switch lowerName {
	case "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
		return true
	}
	return false
}

// gatewayConnectionNominated collects the header names the Connection
// header(s) nominate as hop-by-hop, lowercased — ported from gateway.rs's
// connection_nominated. A static denylist alone leaves a smuggling
// channel (`Connection: x-secret`); this closes it in both the
// southbound and northbound direction.
func gatewayConnectionNominated(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, v := range h.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// southboundGatewayHeaders copies the caller's headers onto the outbound
// cluster request, dropping: hop-by-hop headers (the static set plus
// Connection-nominated names); Host (the cluster's own, not the
// caller's); Authorization (ADR-0003 — the caller's identity must NEVER
// reach the cluster; only the injected static Ray token, applied by the
// caller of this function, does); Content-Length (recomputed by the
// transport from the buffered body); Cookie (the caller's control-plane
// session cookie must not ship to every cluster they route to); and
// Forwarded/X-Forwarded-* (client-supplied values would spoof source
// identity in cluster-side logs/ACLs — Bifrost appends no trusted XFF of
// its own, it only strips inbound ones). Ported from gateway.rs's
// southbound_headers.
func southboundGatewayHeaders(inbound http.Header) http.Header {
	nominated := gatewayConnectionNominated(inbound)
	out := make(http.Header, len(inbound))
	for name, values := range inbound {
		lower := strings.ToLower(name)
		if isGatewayHopByHop(lower) || nominated[lower] ||
			lower == "host" || lower == "authorization" || lower == "content-length" ||
			lower == "cookie" || lower == "forwarded" || isGatewayForwardedHeader(lower) {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// northboundGatewayHeaders copies the cluster's response headers back to
// the caller, dropping hop-by-hop/Connection-nominated headers and
// headers that leak internal cluster topology: Location (a 3xx is never
// followed southbound, so its internal service name/IP has no northbound
// use — #32) and Server (advertises the Ray/dashboard version — #32).
// Ported from gateway.rs's proxy() response-header loop.
func northboundGatewayHeaders(upstream http.Header) http.Header {
	nominated := gatewayConnectionNominated(upstream)
	out := make(http.Header, len(upstream))
	for name, values := range upstream {
		lower := strings.ToLower(name)
		if isGatewayHopByHop(lower) || nominated[lower] || lower == "location" || lower == "server" {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// validHeaderValue mirrors the check reqwest's HeaderValue::from_str
// performs (gateway.rs's BadToken path): visible ASCII plus tab, no
// control characters or DEL. A malformed cluster token must surface as
// this distinguishable 500 error class rather than an opaque transport
// failure indistinguishable from a genuinely unreachable cluster (502).
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// Gateway error responses. Status codes match gateway.rs's GatewayError
// enum (BodyTooLarge/BadToken/Upstream) and host_gateway's inline 503
// exactly; the JSON envelope (rather than the Rust reference's ad hoc
// plain-text tuple bodies) follows this package's established
// convention — every other handler in internal/api answers through
// HTTPError/WriteError, including cluster_obs.go's semantically
// identical inflight-limit 503 (errGatewayBusy, reused directly below
// rather than duplicated).
var (
	errGatewayBodyTooLarge = HTTPError{
		Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large",
		Message: "request body too large",
	}
	errGatewayBadToken = HTTPError{
		Status: http.StatusInternalServerError, Code: "internal_error",
		Message: "cluster auth token is not a valid header value",
	}
	errGatewayUpstream = HTTPError{
		Status: http.StatusBadGateway, Code: "bad_gateway",
		Message: "cluster unreachable",
	}
)
