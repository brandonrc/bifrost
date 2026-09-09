# With `--ray-autoscaling` on, every cluster in a Kueue pool is denied at admission

**Found:** 2026-09-09, on grace, by the in-cluster requirements run started after
the autoscaler fix (#36/#38) was rolled out with `--ray-autoscaling`. Requirements
3, 4 and 5 went red together — 17 tests — while the `sim` lane's two `checkmaite`
clusters had just scaled 0→2→0 without complaint.

**What happened:** the r03/r04/r05 tests put their project in a pool
(`elastic: false`), so the RayCluster carries `kueue.x-k8s.io/queue-name`. With
the flag on, Bifrost also wrote `enableInTreeAutoscaling: true`. Kueue's
RayCluster webhook refuses that combination:

```
admission webhook "vraycluster.kb.io" denied the request:
spec.enableInTreeAutoscaling: Invalid value: true: a kueue-managed RayCluster can
use autoscaling only as an elastic job: enable the ElasticJobsViaWorkloadSlices
feature gate and set the "kueue.x-k8s.io/elastic-job": "true" annotation
```

Kueue admits a workload by counting its pods; a cluster whose worker count the
sidecar changes at will cannot be admitted that way unless it is declared
elastic (workload slices). The sim's clusters were fine because `team-a` and
`team-b` are in no pool on grace: no queue label, no webhook.

**Second finding, from the same clusters:** the denied cluster never existed as
a RayCluster, but `Apply` had already created its two NetworkPolicies
(`bifrost-cluster-<id>` and `-autoscaler`) before the SSA call was refused.
Delete then does nothing — `Terminate` fires only for a cluster that was
*observed* — so the policies stayed (the tombstone sweep would have removed
them 24h later), and every later test in the run failed its postflight for
objects left behind with the run's prefix. Eleven `delete_cluster` audit rows,
one cluster.

**Why nothing caught it:** the kind lane runs the autoscaling shard against
r06 only, with no pool in play; the other shards run without the flag. Neither
combines a pool with the flag.

**Fixed (this change):**

- `provision.EffectiveAutoscaling(flag, queue)` is now the one rule, used by
  `RayClusterFor` and by the live client's autoscaler-egress decision: no pool →
  the operator flag; elastic pool → always on (as before); **non-elastic pool →
  always off**, replicas written. The pool owner asked for fixed-size admission
  and gets it. For a pool to scale, mark it `elastic: true` and run Kueue with
  the `ElasticJobsViaWorkloadSlices` gate.
- The reconciler reaps the policies of a cluster that is deleted before it ever
  materialised. The still-Pending outbox intent for its generation is the
  marker (an apply that began and never completed); the reap runs once, closes
  the intent, and later passes over the tombstone are no-ops.

**Consequence worth knowing on grace:** a project that is later put into a
non-elastic pool loses autoscaling for its clusters, including the ones the
JupyterLab extension creates. The `sim` lane's manifest records
`autoscaling: false` for such a cluster and asserts the fixed-replica shape.
