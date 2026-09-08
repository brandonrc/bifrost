# The in-tree autoscaler cannot reach the API server through the tenant egress policy

**Found:** 2026-09-08, on grace, by the grace-e2e `sim` lane — the first run of
`extension-lifecycle` with `--ray-autoscaling` on. Twenty jobs succeeded on two
head-only clusters and no worker ever appeared; the sampler recorded four head
restarts per cluster and `Back-off restarting failed container autoscaler`.

**Reproduced:** one `small` cluster (min 0 / max 2 workers) with the flag on.
The KubeRay-injected `autoscaler` sidecar starts, then dies after its connect
timeout:

```
_fetch_ray_cr_from_k8s_with_retries → kubernetes_api_client.get(self._ray_cr_path)
requests.exceptions.ConnectTimeout: HTTPSConnectionPool(host='10.152.183.1', port=443):
  Max retries exceeded with url: /apis/ray.io/v1/namespaces/bifrost/rayclusters/repro-autoscaler
  (Connection to 10.152.183.1 timed out. (connect timeout=60))
```

`10.152.183.1` is the `kubernetes` Service. `bifrost-default-deny` denies all
egress from pods carrying `bifrost.dev/cluster-id`, and `bifrost-tenant-allow`
opens exactly one egress: kube-dns on :53. The autoscaler runs in the head pod
with that label, so its only way to read and patch its own RayCluster is
closed. It restarts until the head restarts with it.

**Why nothing caught it:** neither the kind lane nor grace runs the control
plane with `--ray-autoscaling`; ADR-0007's ownership rules are unit-tested, the
sidecar's network path is not.

**Fix (Bifrost):** when `autoscaling` is on for a cluster, the cluster's allow
policy (`ClusterAllowNetworkPolicy`) needs an egress rule from the **head** pod
to the API server — the `kubernetes` Endpoints' addresses and port (here
`192.168.42.150:16443`; the Service VIP is DNAT'd, so the rule must name the
endpoint IPs as `ipBlock`s, refreshed if they change), or the head must run with
the `bifrost.dev/control-plane` egress exemption the control plane itself uses.
The sidecar also needs the RBAC KubeRay grants it (it does: a RoleBinding named
after the cluster exists). Add a kind-lane job that runs the shards with
`--ray-autoscaling` so the path stays covered.

**Until then:** grace runs with the flag off again. The `sim` lane asserts the
fixed-replica shape when the flag is off and the up-then-down shape when it is
on, and records which in its manifest.

**Fixed (bifrost #36):** with autoscaling on for a cluster, the live client
applies `bifrost-cluster-<id>-autoscaler` — an egress-only policy selecting
that cluster's head, allowing the `kubernetes` Endpoints' addresses on their
port (read from `endpoints/kubernetes` in `default`, cached five minutes; RBAC
`get` added to the pack chart and the kind manifests). Deleted with the
cluster. A control plane that cannot read the endpoints refuses the cluster
with a message naming the grant. The kind lane gained an `autoscaling` shard
(`REQ_TARGET=kind-autoscaling`, `--ray-autoscaling` patched onto the
Deployment) running r06, whose `TestAutoscalerReachesTheAPIServerAndAddsAWorker`
watches the sidecar bring a min-1 worker group from zero to one.
