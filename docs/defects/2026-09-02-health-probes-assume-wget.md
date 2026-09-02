# Provisioned RayClusters never report ready on images without `wget`

**Found**: 2026-09-02, on Grace, provisioning a cluster through the live API.
**Severity**: High for req #10. The cluster *works*; it just never says so.

## What happened

A cluster provisioned through Bifrost came up with Ray fully healthy and
stayed `0/1 Ready` indefinitely. The RayCluster `STATUS` column stayed empty
rather than `ready`.

```
Warning  Unhealthy  84s (x58 over 6m9s)  kubelet
  Liveness probe failed: bash: line 1: wget: command not found
```

Ray itself was fine the whole time — checked directly, without `wget`:

```
local_raylet_healthz -> b'success'
gcs_healthz          -> b'success'
```

The image ships neither `wget` nor `curl`.

## Root cause

`internal/provision` emits KubeRay's **default** health probes, which are:

```sh
wget --tries 1 -T 2 -q -O- http://localhost:52365/api/local_raylet_healthz | grep success && \
wget --tries 1 -T 10 -q -O- http://localhost:8265/api/gcs_healthz | grep success
```

That command needs `wget` *and* `grep` present in the user's image. When
either is missing the probe fails identically to a genuinely unhealthy Ray,
and the reported reason blames health rather than the missing tool.

This is the same failure shape as the CI smoke-test bug fixed in 144ccce
(`ubi9-micro` has no `grep`): a check that shells out to a tool the image
does not ship cannot distinguish "property is false" from "tool is absent",
and it accuses a healthy artifact.

## Why this matters for requirement #10

Req #10 is "use nebi environments on the cluster", and the agreed shape is
container images built from nebi envs and served from Artifact Keeper. Those
are slim, conda/uv-based images. **`wget` is not a reasonable thing to
require of a user's data-science image.** As written, Bifrost silently
requires it of every image it provisions, and a user bringing their own
environment gets a cluster that runs but never becomes ready.

## Prior art already on this cluster

`rayserve-pack` hit this and solved it — its head probe uses the
interpreter that is guaranteed present rather than a shell utility:

```json
{"exec": {"command": ["/app/.venv/bin/python", "-c",
  "import urllib.request; urllib.request.urlopen('http://localhost:52365/api/local_raylet_healthz', timeout=4)"]},
 "initialDelaySeconds": 20, "periodSeconds": 10,
 "timeoutSeconds": 5, "failureThreshold": 12}
```

Same cluster, same image, and it reports `ready`.

## Fix direction

Emit probes that do not depend on shell utilities. Python is present in every
Ray image by definition — Ray is a Python library — which makes the
interpreter the only defensible thing to probe with. Hardcoding a venv path
as rayserve-pack does is not portable, so resolve the interpreter rather than
assume `/app/.venv/bin/python`, and let an operator override the probe.

## Test to write

Provision against an image with no `wget`/`curl` and assert the cluster
reaches `ready`. The current suites cannot catch this: they never run a real
image, so the probe command is never executed by a kubelet.
