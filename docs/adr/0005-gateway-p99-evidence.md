# ADR-0005: Gateway p99 evidence (Go proxy overhead)

Status: accepted · Wave 1 Task 16 · generated 2026-09-01 04:06 UTC

## Context

SPEC.md's REQUIREMENTS-amendment for the Rust->Go port asks a specific question about the gateway: Rust's `gateway.rs` had no GC, so its tail latency was a function of the OS scheduler and the Ray upstream alone. Go's runtime has a concurrent, mostly-non-blocking GC, but it is still a GC, and the port needs evidence — not an assumption — about what that costs the gateway's p99, particularly under concurrent load where a GC pause can show up as a tail-latency spike that a Rust binary would never produce.

A real Ray cluster is not available in this environment (no cluster to provision against locally, and standing up `ray start --head` plus a live job submission loop is exactly what `.github/workflows/contract.yml` does in CI, not something this task re-implements). Running the load rig against a real Ray head is out of scope here for that reason.

Instead this rig isolates the part of the question that *is* answerable locally and is exactly what the amendment is asking about: the Go runtime/GC contribution to the gateway's own proxy path (host-match, auth check pass-through, southbound header rewrite, buffered-body forward, response passthrough), independent of whatever a real Ray upstream adds on top. It does this by driving identical concurrent load against a trivial fake upstream both directly (BASELINE) and through the bifrost gateway (GATEWAY), and reporting the delta — the fake upstream's own latency is small and roughly cancels out of the subtraction, leaving the gateway's added cost.

## Method

`scripts/gateway-load.sh` (stdlib-only Go driver: `cmd/gateway-load`, subcommands `fake` and `bench` — no `hey`/`vegeta` binary was available in this environment, so a small Go driver was written instead, per the task brief's fallback):

1. `cmd/gateway-load fake` — a stdlib `net/http` server answering every
   request immediately with a small fixed JSON body.
2. `bifrost serve --store memory --dev-allow-unauthenticated` with a
   one-cluster registry routing a hostname to the fake upstream.
3. `cmd/gateway-load bench` fires N HTTP GET requests (C concurrent
   workers, persistent connections, warmup requests excluded from
   the measured window) — once directly at the fake upstream
   (BASELINE), once at the gateway with `Host:` set to the
   registered cluster hostname so the request takes the real
   `HostGateway` -> `proxy()` path (GATEWAY).

- N = 20000 requests, concurrency = 48
- Go: `go1.26.2`, OS: `macOS-26.3.1-arm64-arm-64bit`
- Reproduce: `scripts/gateway-load.sh [N] [CONCURRENCY]`

## Finding fixed while building this rig

The first runs of this rig (concurrency 32-128) showed 20-40% request errors and a p99/max blowing out to 50-150ms — which looked, at first glance, exactly like the kind of GC-pause tail spike this ADR exists to check for. It was not: `internal/api/gateway.go`'s `buildSouthboundGatewayClient` built its `http.Transport` with no `MaxIdleConnsPerHost` override, so it fell back to Go's default of 2 idle connections per host. Every southbound request beyond the second concurrently in flight to the same cluster paid a fresh TCP handshake instead of reusing a pooled connection, and under this rig's concurrency that connection churn (not GC) produced the latency blowup and the errors (connection resets once the churn saturated the loopback accept queue).

Fixed in the same change as this ADR: `buildSouthboundGatewayClient` now takes `GatewayLimits.MaxInflight` and sizes `MaxIdleConns`/`MaxIdleConnsPerHost` to it — that limit already hard-caps concurrent southbound requests per `GatewayState`, so the pool can never leave idle connections unused. The results below are measured *after* that fix; re-running at concurrency 48 pre-fix reproduces the blowup, post-fix does not (0 errors, clean percentiles) — see the task report for both sets of numbers.

## Results

All values in milliseconds.

| Percentile | Baseline (direct to fake upstream) | Through bifrost gateway | Delta (gateway overhead) |
|---|---|---|---|
| p50 | 0.354 | 0.734 | 0.380 |
| p90 | 0.938 | 1.410 | 0.473 |
| p99 | 1.343 | 2.306 | 0.964 |
| max | 2.843 | 3.804 | 0.961 |
| mean | 0.466 | 0.834 | 0.368 |

Baseline: 101005 req/s, 0 errors. Gateway: 57040 req/s, 0 errors.

## Interpretation

The gateway-added p99 delta above is the Go proxy path's own tail-latency contribution (host-match middleware, auth pass-through, southbound header rewrite/token injection, buffered-body copy, response passthrough) under this load, on this machine, against a near-zero-latency fake upstream — the worst case for exposing GC-pause-shaped tail spikes, since there is no large upstream latency to hide behind. If Go's GC were producing Rust-incomparable tail spikes under this load, they would show up as a p99/max that diverges sharply from p50 in the GATEWAY column relative to BASELINE; the numbers above are the evidence, not an assumption.

This is NOT a substitute for the real-Ray contract replay in `.github/workflows/contract.yml` (which validates protocol correctness, not latency) — it is the SPEC amendment's separate p99-evidence requirement, satisfied against the closest thing to a real upstream this environment can provide.

## Raw results

```json
// baseline
{
  "label": "baseline-direct-to-upstream",
  "n": 20000,
  "concurrency": 48,
  "errors": 0,
  "duration_s": 0.19800975,
  "rps": 101005.12727277318,
  "mean_ms": 0.46606328989999996,
  "p50_ms": 0.354,
  "p90_ms": 0.937791,
  "p99_ms": 1.342583,
  "max_ms": 2.843375
}
// gateway
{
  "label": "through-bifrost-gateway",
  "n": 20000,
  "concurrency": 48,
  "errors": 0,
  "duration_s": 0.350634,
  "rps": 57039.53410108546,
  "mean_ms": 0.8336859347,
  "p50_ms": 0.734125,
  "p90_ms": 1.410292,
  "p99_ms": 2.306125,
  "max_ms": 3.803958
}
```
