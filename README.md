# Bifrost

**An open-source, Anyscale-grade control plane for Ray and Dask clusters on
Kubernetes.** Self-serve compute for data scientists, governed serving for
platform teams, and audit-ready operations for the people who answer for it —
without handing your stack to a managed platform.

In Norse myth, Bifrost is the guarded bridge between realms: the only way
across, and someone watches every crossing. That is the design in one
sentence — users get a bridge to real compute in seconds; operators get a
gate where identity, quota, and policy are enforced on every request.

## What it does

- **Self-serve clusters** — a JupyterLab user requests a private Ray (or
  Dask) cluster and gets a client address back; no shared heads, workers, or
  object stores; idle clusters are reaped automatically.
- **A guarded gateway** — callers never talk to Ray directly. The gateway
  terminates the caller's identity, enforces RBAC, and swaps in per-cluster
  credentials southbound, including for the Ray Jobs API and WebSocket log
  tails.
- **Governed model serving** — groups share models through group-owned
  RayService deployments, isolated in their own resource pools so notebook
  workloads can't starve production serving.
- **Admin-controlled profiles** — administrators decide which images, CPU /
  memory / GPU shapes, and worker counts users may request; users pick from
  approved options instead of submitting raw manifests.
- **Fair-share capacity** — Kueue-backed resource pools with quotas, weights,
  and borrowing between groups.
- **Cost and usage visibility** — who requested what, for how long, and what
  it cost, with tamper-evident audit trails.
- **Crash-safe by design** — ownership is recorded in a persistent store and
  observed state is reconstructed from the cluster on every reconcile pass,
  so the control plane recovers cleanly from its own restarts.

Bifrost composes with the open Ray stack rather than competing with it:
[KubeRay](https://github.com/ray-project/kuberay) is the Kubernetes
substrate, [Kueue](https://kueue.sigs.k8s.io) handles admission, and any
OIDC identity provider (Keycloak, Okta, Dex, …) supplies SSO.

## Architecture

One Go binary, one enforced dependency direction:

```
cmd/bifrost            the binary: serve · login · token · exchange
internal/api           REST surface (spec-first, generated from the frozen
                       contract) + auth middleware + the federating gateway
internal/auth          OIDC discovery, JWKS, RBAC, local users & PATs
internal/controller    persistent store + level-triggered reconcile engine
internal/provision     the ONLY package that talks Kubernetes: typed
                       KubeRay/Kueue translators behind a Provisioner interface
internal/policy        pure functions: quotas, budgets, cost, GPU sharing
internal/core          the domain model — imports nothing but stdlib
```

Invariants the code is built around:

- **Deny by default.** Every route requires authorization; the server fails
  closed if bound to a non-loopback address without auth configured.
- **Credentials stop at the control plane.** User tokens never travel to
  clusters; clusters never see who called.
- **Observation over memory.** The reconciler never trusts a stored phase —
  it re-observes the cluster every pass and repairs drift.
- **Contract-first API.** The OpenAPI 3.1 contract is frozen in
  [`bifrost-api`](https://github.com/brandonrc/bifrost-api); the server's
  handlers are generated from it, so the spec and the code cannot drift
  apart. TypeScript, Python, and Rust SDKs are generated from the same file.

## Status

Early and moving fast. Foundations are in: the domain model and policy
engine (quota, budget, cost, GPU-sharing) are implemented and tested, the
API contract v1 is frozen with SDK pipelines, the
[management console](https://github.com/brandonrc/bifrost-ui) runs, and CI
enforces race-enabled tests, lint, coverage, and vulnerability gates on
every push. The store, reconcile engine, provisioners, and gateway are
landing next, followed by the JupyterLab extension and serving workflows.

Not yet usable for real workloads — watch the repo if you want to be here
when it is.

## Development

```bash
make build    # CGO_ENABLED=0 — ships as a single static binary
make test     # race-enabled full suite
make cover    # coverage report
make lint     # golangci-lint (errcheck, exhaustive, depguard boundaries)
```

Go ≥ 1.25. No cgo in shipped binaries; pure-Go SQLite for dev, Postgres for
production. Start with [`docs/SPEC.md`](docs/SPEC.md) for the program
specification and [`docs/adr/`](docs/adr/) for the decisions and their
evidence.

## Related repositories

| Repo | What it holds |
|---|---|
| [`bifrost-api`](https://github.com/brandonrc/bifrost-api) | The frozen OpenAPI contract + generated TS/Python/Rust SDKs |
| [`bifrost-ui`](https://github.com/brandonrc/bifrost-ui) | The management console (React) |

## License

Apache-2.0.
