# Namespace posture was ensured only on the cluster path; jobs and services provisioned first ran without it

**Found:** 2026-09-04 on the kind lane (runs 33802820554 through 33825288414), after a
week of "RayJob never completes / RayService never ready" on kind that grace never showed.

**What happened.** `EnsureNamespacePosture` (default-deny + tenant-allow NetworkPolicies,
PSS labels) ran only inside the cluster reconciler's actuating apply. `ApplyJob` and
`ServiceClient.Deploy` ensured the per-cluster allow policy and nothing else. In a
namespace whose first workload is a RayJob or RayService — every kind shard that
starts with r05 or r01 — the job's head was selected by exactly one policy,
`bifrost-cluster-<id>`, whose ingress admits same-cluster pods and the owner's
notebook only. KubeRay's operator and the control plane were therefore dropped:
the operator's `GET <head>:8265/api/jobs/<id>` timed out for 300 s and it marked the
RayJob `Failed` (`JobStatusCheckTimeoutExceeded`); serve-status polls failed the
same way, so RayServices never became ready.

Grace never showed it because clusters were created there long before the first job
or service, so the tenant policies already existed. The kind rbac shard passed the
operator-peer probe for the same reason: r03 creates clusters first.

**Evidence.** The early head diagnostic (fixture `DiagnoseJobHead`, #28/#29): head pod
Running and Ready, `dashboard-host=0.0.0.0`, headless Service resolving to the pod IP,
only `bifrost-cluster-<id>` selecting the head, and probes from the operator (two
namespaces) and the control plane all timing out while the same probe reached an
ordinary cluster's head. The failure dump showed `bifrost-default-deny` and
`bifrost-tenant-allow` aged 8 minutes at the end of a 43-minute shard — created by the
first cluster apply in r08, not at startup.

**Fix.** `ApplyJob` and `Deploy` call `EnsureNamespacePosture` first, fail-closed, the
same as the cluster apply. Regression: unit tests in `internal/provision/live` assert
the posture policies are applied before the job's and service's own resources; the kind
jobs and serving shards are the end-to-end check.

**Follow-up.** Ensure the posture once at controller startup as well, so a namespace
is never observed without it, and have the tenant-allow test lanes start a shard
with a job to keep this ordering covered.
