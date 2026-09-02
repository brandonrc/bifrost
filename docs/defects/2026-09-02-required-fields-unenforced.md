# Required fields in the frozen contract are not enforced at runtime

**Found**: 2026-09-02, on Grace, by sending a wrong-shaped create request.
**Severity**: High. Accepts invalid input, persists it, and cannot undo it.

## What happened

A `POST /api/v1/clusters` with the wrong body shape — `{"name": ...,
"engine": ..., "workers": ...}` instead of the contract's `{"id": ...,
"spec": {...}}` — returned **201 Created**.

The body contained no `id` and no `spec` whatsoever. The server accepted it,
wrote a record with an empty id, and audited it as a success:

```
INFO api: audit event ts=1788318639 decision=allow subject=admin \
     action=create_cluster cluster="" status=201
```

The reconciler then failed on it every 30 seconds, indefinitely:

```
WARN reconcile failed cluster="" error="backend error: resource name may not be empty"
```

The record is visible in every listing and **cannot be deleted through the
API** — `DELETE /api/v1/clusters/{id}` cannot address an empty id
(`/api/v1/clusters/` and `/api/v1/clusters/%20` both 404). The API created
something it has no route to remove:

```json
{"desired":"running","engine":"ray","generation":1,"id":"",
 "observed_generation":0,"project":"","ray_version":""}
```

## Root cause

Two independent gaps that compound:

1. **No request validation runs at all.** There is no
   `OapiRequestValidator`, no `openapi3filter`, no embedded spec checked at
   request time. `kin-openapi` appears in `go.mod` only as an *indirect*
   dependency pulled in by `oapi-codegen` at generation time. The generated
   "strict server" enforces *types*, not the contract's `required`.

2. **Go's `encoding/json` zero-fills.** A missing required string field
   unmarshals to `""` rather than erroring, so an absent `id` is
   indistinguishable from a deliberately empty one at the handler.

`CreateCluster` then uses `core.ClusterId(body.Id)` with no emptiness check.
`clusterSpecFromWire` validates `engine`, `ttl_seconds` and
`idle_timeout_secs`, but never `name`, `project`, `image`, `ray_version`,
`head_cpu` or `head_memory` — all `required` in the contract.

## Scope

This is not one endpoint. Because no validator runs, `required` is enforced
only where some handler happens to hand-check a field. `CreateCluster`
demonstrably does not. Every operation taking a body needs auditing; the
per-endpoint blast radius is unknown until that audit runs, which is
precisely the point.

## Why it matters beyond tidiness

- **Req #3 (RBAC / security baseline)** — unvalidated input reaches
  persistence. Authorization ran, validation never did.
- **Req #8 (state recovered on restart)** — the bad record survives restart
  and resumes failing forever. There is no dead-lettering and no bound on
  reconcile retries for a permanently un-actionable record.
- **Req #18 (NIST baseline + audit evidence)** — the audit log records
  `decision=allow ... status=201` for a request that created nothing usable.
  The audit trail asserts a success that did not occur.

## Fix directions

1. Wire request validation middleware against the embedded spec so
   `required` is enforced once, centrally, for all operations — rather than
   per-handler, which is what let this through.
2. Independently, reject an empty `ClusterId` at the domain boundary.
   Defence in depth: the contract and the domain should each refuse it.
3. Bound or dead-letter reconcile retries for records that cannot ever
   succeed, so one bad row cannot log forever.
4. Provide a way to remove an unaddressable record.

## Reproduction

```sh
curl -X POST http://bifrost:8484/api/v1/clusters \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"x","engine":"ray","workers":1}'
# observed: 201 Created, empty-id record persisted
# expected: 400 Bad Request
```

## Note on how this was found

By fumbling the request body, not by looking for it. A correct-by-luck test
would never have caught it — which is the argument for negative/malformed
cases being first-class in the requirement test framework, not an afterthought.
