# Gateway-proxied requests are audited without an action or the role that allowed them

**Found:** 2026-09-04 on grace, while wiring checkmaite to submit Ray jobs
through Bifrost's gateway.

**What happens.** A proxied request is audited with subject, cluster, method,
path, status and latency, which is enough to reconstruct what happened:

```json
{"cluster":"checkmaite-jobs","decision":"allow","granted_roles":[],"latency_ms":5,
 "method":"GET","path":"/api/version","status":200,"subject":"checkmaite-svc","ts":1788526766}
```

Two fields are missing that every API-side row carries:

- `action` is absent, so an auditor filtering the trail by action (`submit_job`,
  `create_cluster`, …) never sees gateway traffic at all — the requests that
  actually run work on a cluster are the ones that drop out of that view.
- `granted_roles` is empty on an allowed request, so the trail does not record
  which assignment permitted it. API-side rows name the role.

**Why it matters.** Requirement 18 asks for audit evidence. The evidence exists
but is not uniform, and the uniform part is what a reviewer queries.

**Fix.** Stamp a synthetic action on proxied rows — `gateway_request`, or
`gateway_jobs`/`gateway_serve`/`gateway_dashboard` derived from the matched
route, keeping `method` and `path` as they are — and fill `granted_roles` from
the authorization decision the proxy already made. Then extend
`r18_baseline` with a case asserting that a gateway request appears in the
trail with an action and the role that allowed it (needs capability `gateway`).
