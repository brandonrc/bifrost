#!/usr/bin/env python3
"""Render docs/adr/0005-gateway-p99-evidence.md from a baseline/gateway
pair of cmd/gateway-load `bench` JSON results.

Called by scripts/gateway-load.sh; not meant to be invoked directly
(although it can be, for re-rendering from saved JSON without re-running
the load rig).
"""
import argparse
import json
import platform
from datetime import datetime, timezone


def load(path):
    with open(path) as f:
        return json.load(f)


def fmt(v):
    return f"{v:.3f}"


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--baseline", required=True)
    p.add_argument("--gateway", required=True)
    p.add_argument("--n", required=True)
    p.add_argument("--concurrency", required=True)
    p.add_argument("--go-version", required=True)
    p.add_argument("--out", required=True)
    args = p.parse_args()

    baseline = load(args.baseline)
    gateway = load(args.gateway)

    rows = []
    for key, label in (
        ("p50_ms", "p50"),
        ("p90_ms", "p90"),
        ("p99_ms", "p99"),
        ("max_ms", "max"),
        ("mean_ms", "mean"),
    ):
        b = baseline[key]
        g = gateway[key]
        rows.append((label, b, g, g - b))

    now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

    lines = []
    lines.append("# ADR-0005: Gateway p99 evidence (Go proxy overhead)")
    lines.append("")
    lines.append(f"Status: accepted · Wave 1 Task 16 · generated {now}")
    lines.append("")
    lines.append("## Context")
    lines.append("")
    lines.append(
        "SPEC.md's REQUIREMENTS-amendment for the Rust->Go port asks a specific "
        "question about the gateway: Rust's `gateway.rs` had no GC, so its tail "
        "latency was a function of the OS scheduler and the Ray upstream alone. "
        "Go's runtime has a concurrent, mostly-non-blocking GC, but it is still a "
        "GC, and the port needs evidence — not an assumption — about what that "
        "costs the gateway's p99, particularly under concurrent load where a GC "
        "pause can show up as a tail-latency spike that a Rust binary would never "
        "produce."
    )
    lines.append("")
    lines.append(
        "A real Ray cluster is not available in this environment (no cluster to "
        "provision against locally, and standing up `ray start --head` plus a "
        "live job submission loop is exactly what `.github/workflows/contract.yml` "
        "does in CI, not something this task re-implements). Running the load rig "
        "against a real Ray head is out of scope here for that reason."
    )
    lines.append("")
    lines.append(
        "Instead this rig isolates the part of the question that *is* answerable "
        "locally and is exactly what the amendment is asking about: the Go "
        "runtime/GC contribution to the gateway's own proxy path (host-match, "
        "auth check pass-through, southbound header rewrite, buffered-body "
        "forward, response passthrough), independent of whatever a real Ray "
        "upstream adds on top. It does this by driving identical concurrent load "
        "against a trivial fake upstream both directly (BASELINE) and through the "
        "bifrost gateway (GATEWAY), and reporting the delta — the fake upstream's "
        "own latency is small and roughly cancels out of the subtraction, leaving "
        "the gateway's added cost."
    )
    lines.append("")
    lines.append("## Method")
    lines.append("")
    lines.append(
        "`scripts/gateway-load.sh` (stdlib-only Go driver: `cmd/gateway-load`, "
        "subcommands `fake` and `bench` — no `hey`/`vegeta` binary was available "
        "in this environment, so a small Go driver was written instead, per the "
        "task brief's fallback):"
    )
    lines.append("")
    lines.append("1. `cmd/gateway-load fake` — a stdlib `net/http` server answering every")
    lines.append("   request immediately with a small fixed JSON body.")
    lines.append("2. `bifrost serve --store memory --dev-allow-unauthenticated` with a")
    lines.append("   one-cluster registry routing a hostname to the fake upstream.")
    lines.append("3. `cmd/gateway-load bench` fires N HTTP GET requests (C concurrent")
    lines.append("   workers, persistent connections, warmup requests excluded from")
    lines.append("   the measured window) — once directly at the fake upstream")
    lines.append("   (BASELINE), once at the gateway with `Host:` set to the")
    lines.append("   registered cluster hostname so the request takes the real")
    lines.append("   `HostGateway` -> `proxy()` path (GATEWAY).")
    lines.append("")
    lines.append(f"- N = {args.n} requests, concurrency = {args.concurrency}")
    lines.append(f"- Go: `{args.go_version}`, OS: `{platform.platform()}`")
    lines.append("- Reproduce: `scripts/gateway-load.sh [N] [CONCURRENCY]`")
    lines.append("")
    lines.append("## Finding fixed while building this rig")
    lines.append("")
    lines.append(
        "The first runs of this rig (concurrency 32-128) showed 20-40% request "
        "errors and a p99/max blowing out to 50-150ms — which looked, at first "
        "glance, exactly like the kind of GC-pause tail spike this ADR exists to "
        "check for. It was not: `internal/api/gateway.go`'s "
        "`buildSouthboundGatewayClient` built its `http.Transport` with no "
        "`MaxIdleConnsPerHost` override, so it fell back to Go's default of 2 "
        "idle connections per host. Every southbound request beyond the second "
        "concurrently in flight to the same cluster paid a fresh TCP handshake "
        "instead of reusing a pooled connection, and under this rig's concurrency "
        "that connection churn (not GC) produced the latency blowup and the "
        "errors (connection resets once the churn saturated the loopback accept "
        "queue)."
    )
    lines.append("")
    lines.append(
        "Fixed in the same change as this ADR: `buildSouthboundGatewayClient` now "
        "takes `GatewayLimits.MaxInflight` and sizes `MaxIdleConns`/"
        "`MaxIdleConnsPerHost` to it — that limit already hard-caps concurrent "
        "southbound requests per `GatewayState`, so the pool can never leave idle "
        "connections unused. The results below are measured *after* that fix; "
        "re-running at concurrency 48 pre-fix reproduces the blowup, post-fix "
        "does not (0 errors, clean percentiles) — see the task report for both "
        "sets of numbers."
    )
    lines.append("")
    lines.append("## Results")
    lines.append("")
    lines.append("All values in milliseconds.")
    lines.append("")
    lines.append("| Percentile | Baseline (direct to fake upstream) | Through bifrost gateway | Delta (gateway overhead) |")
    lines.append("|---|---|---|---|")
    for label, b, g, d in rows:
        lines.append(f"| {label} | {fmt(b)} | {fmt(g)} | {fmt(d)} |")
    lines.append("")
    lines.append(
        f"Baseline: {baseline['rps']:.0f} req/s, {baseline['errors']} errors. "
        f"Gateway: {gateway['rps']:.0f} req/s, {gateway['errors']} errors."
    )
    lines.append("")
    lines.append("## Interpretation")
    lines.append("")
    lines.append(
        "The gateway-added p99 delta above is the Go proxy path's own tail-latency "
        "contribution (host-match middleware, auth pass-through, southbound header "
        "rewrite/token injection, buffered-body copy, response passthrough) under "
        "this load, on this machine, against a near-zero-latency fake upstream — "
        "the worst case for exposing GC-pause-shaped tail spikes, since there is no "
        "large upstream latency to hide behind. If Go's GC were producing "
        "Rust-incomparable tail spikes under this load, they would show up as a "
        "p99/max that diverges sharply from p50 in the GATEWAY column relative to "
        "BASELINE; the numbers above are the evidence, not an assumption."
    )
    lines.append("")
    lines.append(
        "This is NOT a substitute for the real-Ray contract replay in "
        "`.github/workflows/contract.yml` (which validates protocol correctness, "
        "not latency) — it is the SPEC amendment's separate p99-evidence "
        "requirement, satisfied against the closest thing to a real upstream this "
        "environment can provide."
    )
    lines.append("")
    lines.append("## Raw results")
    lines.append("")
    lines.append("```json")
    lines.append("// baseline")
    lines.append(json.dumps(baseline, indent=2))
    lines.append("// gateway")
    lines.append(json.dumps(gateway, indent=2))
    lines.append("```")
    lines.append("")

    with open(args.out, "w") as f:
        f.write("\n".join(lines))


if __name__ == "__main__":
    main()
