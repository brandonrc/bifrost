# Observability for a Bifrost deployment

What it took to get Bifrost and every Ray cluster it runs onto Prometheus and
Grafana, written down as the files that did it. First done on grace
(2026-09-08); reproducible from here.

| file | what it is |
|---|---|
| `lgtm-values.yaml` | values for [nebari-dev/lgtm-pack](https://github.com/nebari-dev/lgtm-pack) — Grafana behind Keycloak SSO (client provisioned by the nebari-operator; group `admin` → Grafana Admin, else Viewer), Loki, Tempo, monolithic Mimir. Two settings matter off a NIC cluster: `otelCollectorOverrides.enabled=false` (its post-install hook otherwise waits for a collector that is not there and leaves the release failed) and Mimir's per-tenant series limit raised (a whole node's series exceed the default 150k). |
| `scrape-bifrost-and-ray.yaml` | the `ServiceMonitor` for `GET /api/v1/metrics` (bearer token of a local `viewer` user) and the `PodMonitor` for every Ray head and worker, relabelling `bifrost-compute.dev/cluster-id` and `bifrost-compute.dev/owner` onto every series. |
| `ray-metrics-netpol.yaml` | the one ingress Bifrost's tenant policy does not grant: the scraper's namespace to `:8080` on Ray pods. |
| `envoy-gateway-podmonitor.yaml` | the gateway's request-level series for the platform's HTTPRoutes, everything else dropped at scrape time. |
| `make-scraper-identity.py` | makes the local `metrics-scraper` viewer user, mints its 90-day PAT and writes the Secret the ServiceMonitor reads. Run on the cluster host (uses `microk8s kubectl`). |
| `../grafana/bifrost-platform.json` | the dashboard. |

The same wiring is a chart feature in bifrost-pack (`observability.enabled`),
which renders the monitors, the policy and the dashboard ConfigMap from values.
These files are the standalone form and the record of what was actually applied.

The scraper on grace is the microk8s `observability` addon's Prometheus,
remote-writing into lgtm-pack's Mimir:

```sh
kubectl patch prometheus -n observability kube-prom-stack-kube-prome-prometheus --type=merge \
  -p '{"spec":{"remoteWrite":[{"url":"http://lgtm-pack-mimir.monitoring.svc:8080/api/v1/push"}]}}'
```

Re-enabling that addon would drop the patch.
