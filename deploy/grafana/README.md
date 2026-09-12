# Grafana dashboard: Bifrost platform

`bifrost-platform.json` is the dashboard for the Ray platform Bifrost runs —
what is up, who is using it, what each cluster is doing, what the gateway
sees, and the control plane and machine underneath. Every panel slices by the
labels Bifrost stamps on the pods it provisions (`bifrost_cluster`,
`bifrost_owner`), so a tenant's share is one filter away.

## What it reads

| source | metrics | how they get scraped |
|---|---|---|
| Bifrost `GET /api/v1/metrics` | `bifrost_clusters_total{state}`, `bifrost_clusters_by_project{project,state}` (state `terminated` = a stopped cluster awaiting purge), `bifrost_pool_nominal`, `bifrost_pool_resource_usage` | a `ServiceMonitor` with a bearer token from a local `viewer` user — the endpoint is Read on the cluster target |
| every Ray head and worker, `:8080` | `ray_node_*`, `ray_resources`, `ray_scheduler_tasks`, `ray_running_jobs`, `ray_finished_jobs_total`, `ray_gcs_actors_count`, `ray_object_store_*` | a `PodMonitor` on `ray.io/is-ray-node=yes` in the workload namespace, relabelling the `bifrost-compute.dev/*` pod labels onto every series; needs an ingress allow on 8080 from the scraper's namespace, because the tenant policy admits nothing else |
| the platform gateway (Envoy) | `envoy_cluster_upstream_rq_*` for the `httproute/bifrost/*` routes | a `PodMonitor` on the gateway pods, keeping only request-level series for the platform's routes |
| kubelet / cAdvisor, kube-state-metrics, node-exporter | `container_*`, `kube_pod_*`, `node_*` | whatever Prometheus stack the cluster runs |

The datasource is referenced by uid `mimir` (nebari-dev/lgtm-pack's Mimir).
For a plain Prometheus, replace the uid.

## Install

Any Grafana with the dashboards sidecar picks it up from a ConfigMap:

```sh
kubectl -n <grafana-ns> create configmap grafana-dashboard-bifrost-platform \
  --from-file=bifrost-platform.json --dry-run=client -o yaml \
  | kubectl label --local -f - grafana_dashboard=1 -o yaml | kubectl apply -f -
```

## Colour, on purpose

Series colours are fixed to the entity, never assigned by position: heads are
always blue, workers orange; `running` is always green and `unknown` grey;
`team-a`/`team-b`/`checkmaite` keep their hue whether or not the others are on
the chart. Status colours are reserved for state and never reused for a
series. One axis per panel. The palette is the validated reference set from the
dataviz method (blue `#2a78d6`, orange `#eb6834`, aqua `#1baf7a`, yellow
`#eda100`, violet `#4a3aa7`; status good `#0ca30c`, warning `#fab219`, serious
`#ec835a`, critical `#d03b3b`, muted `#898781`).
