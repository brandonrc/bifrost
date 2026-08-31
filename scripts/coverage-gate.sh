#!/usr/bin/env bash
# Ratcheting coverage gate: fails if total coverage drops below THRESHOLD.
# Ratchet THRESHOLD upward as coverage grows; target 90 (SPEC.md governance).
set -euo pipefail
THRESHOLD="${COVERAGE_THRESHOLD:-70}"
total=$(go tool cover -func=coverage.txt | tail -1 | awk '{gsub("%","",$3); print $3}')
echo "total coverage: ${total}% (gate: ${THRESHOLD}%)"
awk -v t="$total" -v g="$THRESHOLD" 'BEGIN { exit (t+0 < g+0) ? 1 : 0 }'
