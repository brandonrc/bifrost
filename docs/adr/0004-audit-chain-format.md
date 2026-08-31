# ADR-0004: Audit hash-chain byte format

Status: accepted · Wave 1, Task 1 · Ruling carried from the Wave 1 plan's
Global Constraints (supersedes SPEC.md's earlier Wave-3 deferral of this
work).

## Context

Every security-relevant decision and mutation Bifrost makes emits one
`core.AuditEvent` (`internal/core/audit.go`). The audit trail needs
tamper-*evidence*: a way for `GET /api/v1/audit/verify` (Wave 1, Task 12)
to detect that a stored row was edited, inserted, or deleted out of band,
without requiring a secret key or an external ledger.

**Ruling: there are no legacy audit chains to preserve.** No production
Bifrost deployment exists yet, and the retired reference project's chain
format is not carried forward as a compatibility constraint — this ADR
defines the format fresh, and there is no migration path because there is
nothing to migrate. Any future format change is a breaking change to a
day-1 feature, not a migration.

## Decision

Every appended audit row carries a `chain_hash`:

```
chain_hash = sha256_hex( prev_hash_bytes ‖ canonical_json_bytes(event) )
```

- `prev_hash_bytes` is the UTF-8 bytes of the previous row's `chain_hash` —
  always exactly 64 lowercase hex characters (see Genesis, below), so the
  concatenation boundary is unambiguous without a separator byte.
- `canonical_json_bytes(event)` is the output of Go's `encoding/json`
  marshaling `core.AuditEvent` (`internal/core/audit.go`) as it is
  actually marshaled at the call site — i.e. through
  `AuditEvent.MarshalJSON`, not a struct literal's raw field order.
- `sha256_hex` is the lowercase hex encoding of the raw SHA-256 digest
  (`crypto/sha256` + `encoding/hex`) — 64 characters.
- One `chain_hash` column suffices; a separate `prev_hash` column would be
  redundant, since the previous row's `chain_hash` *is* the input.

Implementation: `controller.AuditChainHash(prevHash string, event
*core.AuditEvent) string` (`internal/controller/store.go`).

### Why Go's `encoding/json` output is "canonical" here

This is not a general-purpose canonical-JSON scheme (no key sorting, no
whitespace normalization beyond what `json.Marshal` already does) — it
relies on three properties that hold for `core.AuditEvent` specifically:

1. **Field order is fixed by declaration.** `encoding/json` always
   marshals struct fields in the order they're declared in the Go source
   (`Ts, Subject, Decision, Reason, Action, Cluster, Method, Path, Status,
   LatencyMs, Required, GrantedRoles`), never sorted, never map-order.
   Every writer of a `core.AuditEvent` value (this store, a future
   SQLite/Postgres store, the chain verifier) marshals through the same
   Go type definition, so they all reproduce the same field order.
2. **Every field is always present.** `AuditEvent`'s ten `Reason`-shaped
   fields are `*T` (pointer) rather than using `omitempty`, so an absent
   value marshals as an explicit `null`, not a dropped key. There is no
   "missing key" ambiguity to canonicalize away.
3. **`AuditEvent.MarshalJSON` already normalizes the two fields that
   would otherwise vary.** `GrantedRoles` (a `[]string`) marshals as `[]`
   rather than `null` when nil, and a zero-value `Decision` defaults to
   `AuditDecisionAllow` before marshaling — both documented on
   `AuditEvent.MarshalJSON` in `internal/core/audit.go`. `AuditChainHash`
   calls `json.Marshal(event)`, which invokes this method, so both
   normalizations are baked into the hash input automatically.

Given those three properties, "the canonical form" reduces to "whatever
`json.Marshal` produces for this exact Go type" — no bespoke canonicalizer
needed, and every Go store backend (memory now; SQLite/Postgres in Tasks
3-4) gets byte-identical hash input for free just by using the same
`core.AuditEvent` type and the shared `AuditChainHash` function. This is
Go-`encoding/json`-specific, not a general JSON canonicalization scheme
(compare JCS/RFC 8785) — it is not portable to a hand-rolled JSON
marshaler or another language's default encoder without re-deriving these
three properties for that encoder.

**Corollary — the audit chain locks `core.AuditEvent`'s field order.**
Reordering, removing, or `omitempty`-ing a field on `AuditEvent` changes
every future hash's input and is therefore a breaking change to the
verifier the moment it ships, even though it wouldn't be a breaking wire
change on its own (JSON object key order is not wire-significant to any
conforming JSON consumer). Adding a *new* field is likewise chain-breaking
the moment any writer starts setting it, because the byte stream changes
shape. This is an accepted constraint on that struct going forward, not
an oversight — a hash chain's whole point is being sensitive to exactly
this. It is not, however, a wire-compatibility constraint: JSON clients of
`GET /api/v1/audit` are unaffected by field reordering.

### Genesis

The first row (seq 1) chains from a fixed genesis constant:

```go
const AuditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"
```

64 ASCII `'0'` characters — the same length as every real chain hash, so
genesis needs no special-casing in the hash-input concatenation logic (it
is simply "the previous hash," always 64 hex chars, always UTF-8 bytes).

### Verification semantics

`controller.VerifyAuditChain(head string, rows []ChainedAuditRow)
AuditChainVerification` replays a window of rows in ascending `seq` order,
recomputing each row's hash from the running `prev` value and comparing
against the row's stored `ChainHash`. It stops at the **first** mismatch:
everything after a broken link is untrustworthy by construction, so
`EventsChecked` reports how many rows verified before the break, and
`FirstBrokenSeq` names the first row that didn't. `head` lets a caller
verify a *window* of the trail (not just from genesis) against whatever
hash preceded that window — `Store.AuditChain` supplies this so
mid-trail verification (e.g. a paginated `/audit/verify` walk in Task 12)
doesn't require replaying the entire trail from the beginning every time.

This is **tamper-evidence, not tamper-proofing**: there is no secret key
involved, so an operator (or attacker) with direct write access to the
store can recompute a self-consistent chain over edited history. What the
chain guarantees is that any edit, insertion, or deletion of a row *without*
recomputing every hash after it is detectable. A documented residual gap:
deleting the *newest* rows (and nothing after them, because there is
nothing after them) leaves no broken link to find — mitigated
operationally by exporting the JSONL audit stream off-box for
non-repudiation, not by anything in this chain design.

### Why not a general canonical-JSON library

A dependency like JCS (RFC 8785) or a custom key-sorting canonicalizer
would make the hash format resilient to *arbitrary* struct shape changes
and portable across languages/encoders. That generality is not needed
here: `AuditEvent` is a single, already-stable, already-flat Go struct
with no `map[string]T`-shaped fields (whose iteration order genuinely is
undefined and would need real canonicalization), and every producer and
verifier of the chain is this same Go binary family (Tasks 1/3/4/12 all
live in `internal/controller`/the same module). Reaching for RFC 8785
would add a dependency and a formatting scheme to defend against a
cross-language-interop scenario this system doesn't have. If a second,
non-Go audit-chain producer/verifier is ever built, this decision should
be revisited — that motivates using this ADR's format guarantees (fixed
field order, no `omitempty`, no maps) as design constraints on any future
audited-event types too, rather than reaching for full JCS at that point.

## Consequences

- `core.AuditEvent`'s field order and full-presence-of-every-field shape
  (established in Wave 0) become a load-bearing invariant for Task 1
  onward, not just a wire-format convention. Documented above so a future
  editor of `internal/core/audit.go` sees the constraint.
- SQLite and Postgres stores (Tasks 3-4) MUST call the same
  `controller.AuditChainHash`/`VerifyAuditChain` functions rather than
  reimplementing the byte construction — this is why they live in the
  shared `internal/controller` package rather than duplicated per backend
  (mirrors the Rust reference's `pub(crate)` visibility for the
  equivalent helpers).
- No migration tooling, no legacy-format detection, no dual-write period:
  the first `record_audit` call any Bifrost deployment ever makes uses
  this format, full stop.
