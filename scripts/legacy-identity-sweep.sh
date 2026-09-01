#!/usr/bin/env bash
# Re-runnable sweep for the retired "mobula" product identity in LIVE runtime
# surfaces — strings a running system reads or writes, or that an operator
# types or reads: storage keys, event names, env var names, metric names,
# client ids, realm names, headers, cookies, query params, CSS prefixes.
#
# WHY THIS EXISTS
#
# The first hand sweep of this rename matched `mobula-` and `mobula_` and
# missed the dot-separated form (`mobula.token`, `mobula.pkce`, …). Five live
# browser storage keys survived a sweep that reported clean. The lesson is not
# "add dots" — it is that a one-time act of diligence has no way to tell you
# what shape it forgot. So the sweep is a script: separator-agnostic,
# case-insensitive, with every accepted exception written down next to its
# reason, and re-runnable by whoever comes next.
#
# WHAT IS DELIBERATELY *NOT* A FINDING
#
# Heritage: this codebase is a line-by-line port, and `// Reference:
# mobula-auth/src/local.rs:28` is what let reviewers trace what the original
# did — it caught both the session-PAT-mint defect and the owner-match error.
# Deleting that record would remove the reasons without removing the identity.
# So comments, ADRs, the frozen-contract parity note and superseded ruling text
# are excluded BY CATEGORY, and the exclusions are listed below, not implied.
#
# Usage:  scripts/legacy-identity-sweep.sh [--rev REF] [repo-root ...]
# Exit:   0 = nothing outside the allowlist, 1 = candidates to triage,
#         2 = the sweep's own self-test failed (a "clean" result is meaningless).
#
# --rev matters more than it looks. Without it the sweep reads the WORKING
# TREE, which is whatever branch happens to be checked out — and in a shared
# checkout that may be someone else's. A sweep run that way answers "is this
# directory clean right now", not "is this branch clean", and the two diverge
# silently. Pass --rev to make a claim about a branch:
#
#   scripts/legacy-identity-sweep.sh --rev rename/bfr-prefix .
#
# It reads the ref through `git grep <rev>` and never touches the working tree,
# so it is safe to run while someone else is working in the same clone.

set -uo pipefail

LEGACY="${LEGACY_IDENTITY:-mobula}"
REV=""
if [ "${1:-}" = "--rev" ]; then
  REV="${2:?--rev needs a ref}"
  shift 2
fi
ROOTS=("$@")
[ ${#ROOTS[@]} -eq 0 ] && ROOTS=(".")

# Build artifacts, dependencies, VCS, and agent scratch — never product source.
PRUNE='/(\.git|node_modules|dist|build|coverage|\.venv|\.yarn|\.claude|__pycache__|\.mypy_cache|\.ruff_cache|\.pytest_cache|labextension|vendor)/'

# Only files that can carry a runtime string. Prose (.md) is reviewed by hand;
# it documents, it does not execute.
#
# NOTE the shape of this pattern. grep -rn emits `path:line:content`, so the
# extension has to be anchored against the *path* — immediately before
# `:<line>:` — not against the end of the line. An earlier version of this
# script anchored with `$`, which matches the end of the CONTENT, so it
# silently rejected every hit and reported four clean repos while real
# findings sat in the tree. That is the identical failure this script exists
# to prevent, committed inside the script itself: a filter that looks right,
# reports clean, and was never checked against a known-positive.
#
# It is checked now — see the self-test at the bottom, which plants a known
# finding and fails if the sweep does not see it.
EXTS='\.(go|ts|tsx|js|jsx|py|sh|ya?ml|json|toml|html|css|npmrc)|/(Dockerfile|\.dockerignore|\.golangci\.yml)'
PATH_FILTER="^[^:]*(${EXTS}):[0-9]+:"

# ---------------------------------------------------------------------------
# ALLOWLIST — every accepted occurrence, with the reason it is accepted.
# Adding a line here is a decision; leaving one undocumented is a bug.
# ---------------------------------------------------------------------------
allow() {
  grep -vE \
    -e 'MOBULA_CONFIG_DIR|MOBULA_LOCAL_ADMIN_PASSWORD' \
    -e 'no_mobula_policy_selects_namespace_wide' \
    -e 'mobula ADR-0002|mobula-api #45' \
    -e 'mobula_auth::|mobula-auth/|mobula-cli|mobula-api mounts'
  # 1. MOBULA_CONFIG_DIR / MOBULA_LOCAL_ADMIN_PASSWORD appear only inside
  #    comments recording what the Rust CLI called the variable that this
  #    codebase already renamed (BIFROST_*). Heritage, and the rename is done.
  # 2. no_mobula_policy_selects_namespace_wide cites the Rust test this Go test
  #    was ported from. Heritage.
  # 3. golangci depguard `desc:` strings cite mobula ADR-0002 / mobula-api #45
  #    as the source of a lint rule. Citations, shown to developers only.
  # 4. mobula_auth:: / mobula-auth/ / mobula-cli / "mobula-api mounts" are
  #    source-of-truth references in doc comments. Heritage.
}

# Strip whole-line comments (// … , # …, * … in a block comment). A runtime
# string cannot live in one, and leaving them in buries the real findings.
strip_comments() {
  grep -vE '^[^:]+:[0-9]+:[[:space:]]*(//|#|\*|/\*)'
}

raw_hits() {
  # `git grep <rev>` reads the ref, not the checkout; plain grep reads the tree.
  if [ -n "$REV" ] && git -C "$1" rev-parse --verify --quiet "$REV" >/dev/null 2>&1; then
    git -C "$1" grep -niE "$LEGACY" "$REV" 2>/dev/null \
      | sed "s|^${REV}:||"
  else
    grep -rniE "$LEGACY" "$1" 2>/dev/null
  fi
}

sweep() {
  raw_hits "$1" \
    | grep -vE "$PRUNE" \
    | grep -E "$PATH_FILTER" \
    | strip_comments \
    | allow
}

# --- self-test: prove the sweep can still see a finding -----------------------
# A sweep over a clean tree passes whether or not it works, exactly like the
# AST guards in bifrost-jupyter. So plant one and require it to be found.
selftest() {
  local dir probe
  dir="$(mktemp -d)"
  probe="$dir/planted.go"
  printf 'package p\n\nconst K = "%s.planted-key"\n' "$LEGACY" > "$probe"
  if ! sweep "$dir" | grep -q 'planted-key'; then
    echo "FATAL: self-test failed — the sweep did not find a planted '$LEGACY' string." >&2
    echo "       The filters are broken; a 'clean' result from this run means nothing." >&2
    rm -rf "$dir"
    exit 2
  fi
  rm -rf "$dir"
}

selftest

status=0
for root in "${ROOTS[@]}"; do
  hits=$(sweep "$root")
  if [ -n "$hits" ]; then
    echo "=== $root: live-string candidates to triage ==="
    echo "$hits"
    echo
    status=1
  else
    echo "=== $root: clean (no live '$LEGACY' strings outside the allowlist) ==="
  fi
done

exit "$status"
