# Catalogued storage is unreachable from the cluster: tenant egress is DNS-only

**Found:** 2026-09-03, grace, while reading a parquet file from an in-cluster S3
server (aks3) through a row-12 storage entry.

**What happens.** `PUT /settings/policy {"storage":[…]}` catalogues a Secret and a
cluster/job/service that references it gets the credentials projected
(`envFrom.secretRef`, verified). But the tenant NetworkPolicy the provisioner
writes for every cluster allows egress to kube-dns only (plus intra-cluster
traffic through the per-cluster allow policy), so the first request to the
storage endpoint hangs: `AWS Error NETWORK_CONNECTION during HeadObject
operation: curlCode: 28, Timeout was reached`. The requirement — private
storage *from the cluster* — is therefore only half met: credentials arrive,
packets do not.

**Workaround used.** A hand-written NetworkPolicy in the `bifrost` namespace
selecting `bifrost.dev/cluster-id Exists` with egress to the storage
namespace/pod on its port (`grace-deploy/aks3-egress.yaml`). With it the job
read 1000 rows and summed them correctly.

**Fix.** A storage entry should carry the egress it needs and the provisioner
should render it into the per-cluster policy only for clusters that reference
the entry:

```json
{"name":"team-a-aks3","secret_name":"team-a-aks3","mode":"env","projects":["team-a"],
 "egress":[{"namespace":"aks3","pod_labels":{"app":"aks3"},"port":9000}]}
```

with a second form for external endpoints (`{"cidr":"52.216.0.0/15","port":443}`).
The entry is admin-written, so the allowance stays under platform control; a
cluster without a storage reference keeps DNS-only egress. Add a requirement
test on the cluster lanes that reads an object through a catalogued entry
(aks3 or MinIO in the lane namespace) — today `r12` proves projection only.

**Also observed.** A job with `worker_groups: []` cannot run Ray Data tasks:
the head advertises no CPU for tasks, so `read_parquet` waits forever on
`{'CPU': 1.0}`. Documented in the verification guide; not a defect.
