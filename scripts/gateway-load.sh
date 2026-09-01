#!/usr/bin/env bash
# Gateway p99 load rig (Wave 1 Task 16 — SPEC.md's REQUIREMENTS-amendment
# acceptance evidence for the gateway path: does the Go runtime's lack of
# Rust-style GC determinism show up as gateway tail latency?).
#
# Stands up:
#   1. A trivial fake upstream (cmd/gateway-load's "fake" subcommand — a
#      stdlib net/http server that answers every request immediately with
#      a small fixed JSON body, cheap enough not to dominate the
#      measurement) standing in for a Ray dashboard/job-API endpoint. A
#      real Ray head is not available in this environment (see the task
#      report for why); isolating the fake upstream's own latency this
#      way is what lets the BASELINE vs THROUGH-GATEWAY delta below
#      isolate exactly the Go-runtime/GC contribution the SPEC amendment
#      asks about, independent of whatever a real upstream would add on
#      top.
#   2. `bifrost serve` (memory store, dev-allow-unauthenticated, no k8s
#      namespace — the gateway's HTTP proxy path doesn't touch the
#      reconciler) with a one-cluster registry routing a hostname to the
#      fake upstream.
#   3. cmd/gateway-load's "bench" subcommand, run twice:
#        - BASELINE: directly against the fake upstream (no gateway hop).
#        - GATEWAY:  against bifrost's gateway, Host header set to the
#          registered cluster hostname (HostGateway routes it through the
#          proxy() path: credential strip, southbound header rewrite,
#          buffered-body forward, response passthrough).
#      The GATEWAY-minus-BASELINE delta at each percentile is the
#      gateway's own added latency.
#
# Results (both runs' JSON, plus the delta) are written to
# docs/adr/0005-gateway-p99-evidence.md by this script.
#
# Usage: scripts/gateway-load.sh [N] [CONCURRENCY]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

N="${1:-5000}"
C="${2:-32}"

WORKDIR="$(mktemp -d)"
trap 'cleanup' EXIT

FAKE_PID=""
GATEWAY_PID=""

cleanup() {
  [ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" 2>/dev/null || true
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORKDIR"
}

echo "== building bifrost + gateway-load =="
go build -o "$WORKDIR/bifrost" ./cmd/bifrost
go build -o "$WORKDIR/gateway-load" ./cmd/gateway-load

echo "== starting fake upstream =="
"$WORKDIR/gateway-load" fake --bind 127.0.0.1:0 > "$WORKDIR/fake-addr.txt" 2> "$WORKDIR/fake.log" &
FAKE_PID=$!
for _ in $(seq 1 50); do
  [ -s "$WORKDIR/fake-addr.txt" ] && break
  sleep 0.1
done
FAKE_ADDR="$(cat "$WORKDIR/fake-addr.txt")"
if [ -z "$FAKE_ADDR" ]; then
  echo "fake upstream failed to start; log:" >&2
  cat "$WORKDIR/fake.log" >&2
  exit 1
fi
echo "fake upstream listening on $FAKE_ADDR"

CLUSTER_HOST="gateway-load.test"
cat > "$WORKDIR/clusters.json" <<EOF
{"clusters": [{"id": "loadtest", "hostname": "$CLUSTER_HOST", "api_base_url": "http://$FAKE_ADDR"}]}
EOF

echo "== starting bifrost serve (gateway) =="
GATEWAY_BIND="127.0.0.1:18485"
"$WORKDIR/bifrost" serve --store memory --bind "$GATEWAY_BIND" \
  --registry "$WORKDIR/clusters.json" --dev-allow-unauthenticated \
  > "$WORKDIR/gateway.log" 2>&1 &
GATEWAY_PID=$!
for _ in $(seq 1 50); do
  curl -sf "http://$GATEWAY_BIND/healthz" > /dev/null 2>&1 && break
  sleep 0.1
done
if ! curl -sf "http://$GATEWAY_BIND/healthz" > /dev/null 2>&1; then
  echo "bifrost serve failed to start; log:" >&2
  cat "$WORKDIR/gateway.log" >&2
  exit 1
fi
echo "gateway listening on $GATEWAY_BIND"

echo "== baseline: direct to fake upstream (n=$N c=$C) =="
"$WORKDIR/gateway-load" bench --dial "$FAKE_ADDR" --host "$FAKE_ADDR" --path /api/version \
  -n "$N" -c "$C" --label baseline-direct-to-upstream | tee "$WORKDIR/baseline.json"

echo "== through gateway (n=$N c=$C) =="
"$WORKDIR/gateway-load" bench --dial "$GATEWAY_BIND" --host "$CLUSTER_HOST" --path /api/version \
  -n "$N" -c "$C" --label through-bifrost-gateway | tee "$WORKDIR/gateway.json"

echo "== writing docs/adr/0005-gateway-p99-evidence.md =="
python3 "$ROOT/scripts/render_gateway_load_adr.py" \
  --baseline "$WORKDIR/baseline.json" \
  --gateway "$WORKDIR/gateway.json" \
  --n "$N" --concurrency "$C" \
  --go-version "$(go version | awk '{print $3}')" \
  --out "$ROOT/docs/adr/0005-gateway-p99-evidence.md"

echo "== done =="
