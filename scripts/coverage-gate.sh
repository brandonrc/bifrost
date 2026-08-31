#!/usr/bin/env bash
# Ratcheting coverage gate: fails if total coverage drops below THRESHOLD.
# Ratchet THRESHOLD upward as coverage grows; target 90 (SPEC.md governance).
#
# COVERAGE_EXCLUDE is a newline-separated list of substrings; coverprofile
# lines whose path contains any of them are dropped before computing the
# total, so wiring that isn't meaningfully unit-testable doesn't drag the
# gate down. Defaults to the cmd/ main-wiring package and the future
# internal/provision/live k8s client wrappers (both named in SPEC.md's
# coverage-gate governance). With no exclusions matching, behavior is
# identical to computing coverage over the unfiltered profile.
set -euo pipefail
THRESHOLD="${COVERAGE_THRESHOLD:-70}"
COVERAGE_EXCLUDE="${COVERAGE_EXCLUDE:-/cmd/
internal/provision/live}"

filtered=coverage.filtered.txt
head -1 coverage.txt > "$filtered"
if [ -n "$COVERAGE_EXCLUDE" ]; then
	tail -n +2 coverage.txt | grep -v -F -f <(printf '%s\n' "$COVERAGE_EXCLUDE") >> "$filtered" || true
else
	tail -n +2 coverage.txt >> "$filtered"
fi

total=$(go tool cover -func="$filtered" | tail -1 | awk '{gsub("%","",$3); print $3}')
echo "total coverage: ${total}% (gate: ${THRESHOLD}%, exclusions: $(printf '%s' "$COVERAGE_EXCLUDE" | tr '\n' ' '))"
awk -v t="$total" -v g="$THRESHOLD" 'BEGIN { exit (t+0 < g+0) ? 1 : 0 }'
