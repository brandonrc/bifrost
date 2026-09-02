# Handoff — grace end-to-end run and the L3 lane (2026-09-02, evening)

Follows `2026-09-02-p0-session-handoff.md`. Same branch (`feat/req-framework-p0`,
draft PR #1), same worktree.

## What was done

1. **Manual end-to-end exercise on grace** (`~/work/bifrost-e2e-e2e73357.log` on
   grace; 75 checks passed, 9 failed). Everything a self-serve user does was
   driven through the public API against the deployed image `sha-95dcede`:
   login/PAT/revoke, users + project-scoped grants, create → running in ~30 s,
   per-owner NetworkPolicy, a Ray job and a Ray Client session from an
   owner-labelled pod in `jupyter`, a non-owner pod refused on :8265/:10001,
   suspend/resume, control-plane restart, out-of-band RayCluster deletion,
   delete, TTL reaper, audit chain, dashboard. Findings are in §Findings.
2. **The same exercise as requirement tests** — the spec's P2 "cluster
   target", pulled forward because the manual run needed a home in CI:
   - `test/requirements/target/cluster` — `REQ_TARGET=kind|grace`, one
     implementation, `targets.yaml` for contexts/capabilities. Seeds the same
     principals inproc has (`req-dev-a` … as local developers holding
     `operator@project:<p>`), wraps the k8s client so tests can mutate only
     run-labelled objects, runs probe pods (`req.PodRunner`), restarts the
     control plane (`req.Restarter`), per-test postflight.
   - `test/requirements/fixture` — shared cluster vocabulary.
   - Tests: `r03_rbac` (incl. `TestCNIEnforcesNetworkPolicy`, the lane's
     precondition), `r06_self_serve`, `r08_cleanup`, `r10_nebi_envs`
     (defect 2 as NotYetBuilt), `r15_health`, `r18_baseline`.
   - `.github/workflows/l3-kind.yml` — kind + Calico + KubeRay 1.7.0 + Kueue
     0.19.2, control plane deployed from `target/cluster/kind/manifests.yaml`
     (a mirror of the pack chart, until the pack has a remote). NodePort →
     runner :8484 so restart tests survive. Nightly, dispatch, PRs touching
     the relevant paths. Matrix is an artifact, not committed.
   - `scripts/l3-grace.sh` / `make test-l3 TARGET=grace` — cross-compiles the
     requirement packages, runs them **on grace** over ssh, converts back to
     `go test -json`. No `-race` (cgo). Excludes `contract` (malformed bodies
     would pollute a pre-fix store) and `r17_slurm` (needs the source tree).
3. **Grace L3 run** (run id `t6a986e8a`): r03 5/5, r06 7/7, r15 1/1, r18 3/3,
   r08 4/5 — the one failure was an id collision between packages that
   exposed the store bug below; fixed and pinned.

## Findings (and where each landed)

| Finding | Where it stands |
|---|---|
| Deployed image accepts a create with no `spec` **and** a non-RFC1123 id (201) | Fixed on this branch (validation middleware + `IsK8sName`); grace still runs the old image. Store had an empty-id zombie row → removed by hand 2026-09-02 (scale to 0, python pod on the PVC). |
| **Re-creating a deleted cluster id answers 201 and leaves `desired=terminated`** — a zombie nothing can start or delete | Fixed in all three stores (`UpsertDesired` onto a terminated record = fresh create), conformance scenario + `TestClusterIdCanBeReusedAfterDelete`. |
| **Suspend/resume require global cluster write**; the project operator who created the cluster gets 403 — bifrost-jupyter shows those buttons to that user | Pinned as NotYetBuilt `TestSuspendResumeByProjectOperator`. Fix = `AuthorizeScoped` with `cluster.Spec.Project` in `lifecycleCommand`. Not done: it is a deliberate port of clusters.rs; the user should rule. |
| Dashboard crash-loop on Helm rev 8: nginx template rendered `${NGINX_LOCAL_RESOLVERS}` literally | `bifrost-pack` 3b973df sets `NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1`; grace rev 9 healthy; pack test asserts it. |
| KubeRay default probes use `wget`; images without it restart every ~10 min | Not fixed (P2). Pinned as NotYetBuilt in `r10_nebi_envs`; on grace set `REQ_NOWGET_RAY_IMAGE=localhost:32000/checkmaite-api:2.56.0-r1`. |
| Global job history (`GET /api/v1/jobs`) empty after a real job | Known: side-write deferred to Wave 3. |
| `ClusterView` has no `owner`; owner is visible only as the k8s label | Contract question, not a defect. Tests read the label. |
| Gateway registry is static; provisioned clusters are not routable through the Jobs gateway; grace has no registry | Design gap for #5 and for checkmaite (its `ray-gw` host points at mobula's deleted route). User decision. |
| RBAC model: developer is read-only on clusters | Documented in the seeding; `TestDeveloperCannotCreateCluster`. |

## Running the lanes

- L2 (every push, merge-blocking): `make test-l2` / `make report`.
- L3 kind: the workflow; locally `make test-l3 TARGET=kind` after deploying
  `target/cluster/kind` into a kind cluster with Calico.
- L3 grace: `make test-l3 TARGET=grace` (needs `ssh geraci@grace`). ~20 min.
  Results in `l3-grace.json`, matrix in `.l3/report/`.

## Open items for the next session

- Deploy an image with the validation fix to grace, then add `contract` back
  to the grace lane.
- Rule on suspend/resume scoping; then flip the NotYetBuilt.
- Defect 2 probe fix (provisioner sets probes that do not need wget).
- The pack's org (brandonrc vs nebari-dev) so the kind lane can install the
  real chart instead of `target/cluster/kind/manifests.yaml`.
- Whole-branch review; un-draft PR #1.
