# Running the requirement suite inside the cluster

The L3 grace lane (`make test-l3 TARGET=grace`) cross-compiles the suite on a
laptop and runs it over ssh. This is the same suite as a Job, so the cluster
validates itself: nightly on a schedule, or on demand, with no repo, no Go
toolchain and no tunnel.

## Install

```sh
kubectl apply -f rbac.yaml       # ServiceAccount + three namespaced Roles
kubectl apply -f cronjob.yaml    # nightly run, and the template for on-demand
```

Check the four site-specific values in `cronjob.yaml` first: the image tag,
`REQ_TARGET`, `REQ_GATEWAY_DOMAIN` (must match the deployment's
`--gateway-domain`) and `REQ_NOWGET_RAY_IMAGE`. `rbac.yaml` names three
namespaces — `bifrost` (workload), `jupyter` (probe pods) and `kuberay` (where
two tests pose as the operator to prove the tenant policy admits it) — so
change all three if yours differ.

## Run it now

```sh
kubectl -n bifrost create job --from=cronjob/bifrost-requirements req-$(date +%s)
kubectl -n bifrost logs -f job/req-<id>
```

The log is the report. `reqrun` prints a line per package and then the
traceability matrix:

```
=== r03_rbac
--- ok r03_rbac  11 passed, 0 failed, 0 skipped  (3m41s)
...
req  3  built         tests=11 pass=11 fail=0 nyb=0
```

A failing package prints each failed test with the last line it logged, which
is usually the assertion, so the log alone says what broke.

To run one package while chasing something:

```sh
kubectl -n bifrost create job --from=cronjob/bifrost-requirements req-r05 --dry-run=client -o yaml \
  | kubectl set env --local -f - REQ_ONLY=r05_ephemeral_rayjob -o yaml \
  | kubectl apply -f -
```

## What it does to the cluster

Every object a run creates carries `req.bifrost.dev/run=<run id>`, and the
suite's Kubernetes client refuses to mutate anything that does not. Each
package sweeps its own objects afterwards and fails if any survive, so a green
run also means it left nothing behind.

It is not read-only, and it is not meant for a production namespace: it creates
and deletes clusters, jobs and services in projects `team-a` and `team-b`,
seeds `req-*` users through the API, and — where the target declares the
`restart` capability — deletes the control plane's pod to prove the store
recovers. Point it at a deployment you are willing to have exercised.

## Which lane to use when

| Lane | Runs | Good for |
|------|------|----------|
| L2 (`make report`) | every push, no cluster | semantics, RBAC, validation, audit, policy |
| kind (`l3-kind.yml`) | GitHub, four shards | provisioning and isolation on a throwaway cluster, per pull request |
| grace (`make test-l3 TARGET=grace`) | a laptop over ssh | the real deployment, while you are working on it |
| in-cluster (here) | the cluster itself | the real deployment, on a schedule or from a terminal with only `kubectl` |

The in-cluster lane and the grace lane compile the same packages with the same
environment; the difference is where the binaries live and who starts them.
