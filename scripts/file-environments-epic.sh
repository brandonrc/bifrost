#!/usr/bin/env bash
# Files the "Governed environments for Ray workloads" epic and its 10 issues
# on GitHub. Requires: gh authenticated with issue-write on the target repo.
# Usage: scripts/file-environments-epic.sh [owner/repo]   (default: bifrost-compute/bifrost)
# Idempotency: none — run once. Issue bodies are extracted from
# docs/epics/2026-09-12-governed-environments.md.
set -euo pipefail

REPO="${1:-bifrost-compute/bifrost}"
DOC="$(dirname "$0")/../docs/epics/2026-09-12-governed-environments.md"

section() { # section <heading-prefix> — prints one issue body from the epic doc
  awk -v pat="^### Issue $1 — " '
    $0 ~ pat {found=1; sub(pat, ""); print "**" $0 "**"; print ""; next}
    found && /^### / {exit}
    found {print}
  ' "$DOC"
}

epic_body() {
  awk '/^## Epic statement/,/^### Issue 1/' "$DOC" | sed '$d'
}

echo "Filing epic + 10 issues on $REPO"
EPIC_URL=$(gh issue create --repo "$REPO" \
  --title "Epic: Governed environments for Ray workloads" \
  --label "epic" \
  --body "$(epic_body)")
echo "epic: $EPIC_URL"
EPIC_NUM="${EPIC_URL##*/}"

declare -A TITLES=(
  [1]="Contract: EnvironmentSpec schema and environment references (bifrost-api)"
  [2]="Govern runtime_env_yaml as it exists today"
  [3]="Environment catalog on the policy row"
  [4]="Environment resolution at admission, jobs and clusters"
  [5]="MVP: runtime_env-backed environments end to end"
  [6]="Environment publication workflow, versioning, audit"
  [7]="CVE/scan gate for environments"
  [8]="Console affordances: environment picker + guided builder (bifrost-ui)"
  [9]="Design: server-side image builds (ADR + spike)"
  [10]="Image-build pipeline implementation (post-MVP)"
)

for i in 1 2 3 4 5 6 7 8 9 10; do
  URL=$(gh issue create --repo "$REPO" \
    --title "${TITLES[$i]}" \
    --body "$(section "$i")

---
Part of #$EPIC_NUM.")
  echo "issue $i: $URL"
done
