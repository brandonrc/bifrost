# ADR-0003: Kubernetes dependency pin set (Wave 1)

Status: accepted · Input to Wave 1's provision/controller work · Verified
against tag-pinned go.mod files upstream (not release-page summaries).

All four upstreams converge on k8s minor **1.36** with zero replace
directives:

```go
require (
	github.com/ray-project/kuberay/ray-operator v1.7.0
	k8s.io/api v0.36.4
	k8s.io/apimachinery v0.36.4
	k8s.io/client-go v0.36.4
	sigs.k8s.io/controller-runtime v0.24.1
	sigs.k8s.io/kueue v0.19.2
)
```

| Module | Version | Pins k8s.io/* | Pins controller-runtime |
|---|---|---|---|
| controller-runtime | v0.24.1 | v0.36.0 | — |
| kuberay/ray-operator | v1.7.0 | v0.36.0 | v0.24.1 |
| kueue | v0.19.2 | v0.36.2 | v0.24.1 |
| k8s.io/* | v0.36.4 (latest 0.36 patch) | — | — |

Notes:

- **KubeRay typed APIs**: `github.com/ray-project/kuberay/ray-operator/apis/ray/v1`
  (stable group; no separate apis submodule exists). Imports cleanly; expect
  a fat go.sum from transitive entries (cert-manager, openshift/api,
  volcano.sh/apis, scheduler-plugins, gateway-api) — none compile into the
  binary.
- **Kueue**: single module `sigs.k8s.io/kueue` (no apis-only module —
  proxy 404 confirmed). Import `apis/kueue/v1beta2` (current storage version
  on the 0.19 line). Kueue internally pins kuberay v1.6.2; our direct v1.7.0
  wins under MVS, no conflict.
- Patch-level skew above upstream pins (v0.36.4 > v0.36.0/v0.36.2) is safe;
  the hard invariant is one shared k8s.io minor across all staging modules.
- Client-go 1.36 supports API servers 1.35–1.37 (±1 minor skew).
- **Upgrade-as-set rule** (per ADR-0001 risk note): bump controller-runtime
  and k8s.io/* minors together, then wait for KubeRay and Kueue releases
  pinning the same minor before moving. Never bump one alone.
