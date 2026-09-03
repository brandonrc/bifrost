package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/policy"
)

// DefaultMeteringInterval is how often the metering pass records a usage
// sample per running cluster. Coarse on purpose: the usage report
// integrates a step series, so one reading a minute is exact for anything
// that lives longer than a minute, and the table stays small.
const DefaultMeteringInterval = time.Minute

// Meter is the usage producer for requirement 14 (who requested what, for
// how long, at what cost). Until 2026-09-02 nothing wrote usage samples:
// RecordUsageSamples existed on every store and the report was a correct
// reader over a permanently empty table. Each pass records, for every
// cluster whose observed state is running, one sample per resource of its
// minimum demand (head plus min_replicas workers — what is actually held),
// attributed to the cluster's project, its pool (empty when the project
// has no allocation) and its owner (the "who" of requirement 14; "" when
// the cluster is unattributed), tagged observed_spec. Suspended and
// terminated clusters hold nothing and record nothing; the step series
// then reads as zero from the last running sample onward once a zero
// sample closes it.
//
// Ephemeral Ray jobs (requirement 5) are metered the same way: a job whose
// KubeRay deployment status is Running holds its head plus min workers,
// attributed to the job's project, pool and submitting owner. Jobs and
// clusters share an id namespace only by convention, so the closing-zero
// bookkeeping keys jobs under a "job:" prefix — a job and a cluster with
// the same id are metered independently.
func (r *Reconciler) Meter(ctx context.Context, now uint64) (int, error) {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return 0, err
	}
	var samples []UsageSample
	for i := range clusters {
		c := &clusters[i]
		running := c.ObservedState != nil && *c.ObservedState == core.ClusterStateRunning && c.Desired == DesiredRunning
		samples = r.meterOne(ctx, samples, c.ID, &c.Spec, derefOr(c.Spec.Owner, ""), running, now, "cluster")
	}
	jobs, err := r.store.ListRayJobs(ctx)
	if err != nil {
		return 0, err
	}
	for i := range jobs {
		j := &jobs[i]
		running := j.DeploymentStatus == "Running" && j.Desired == DesiredRunning
		spec := jobDemandSpec(&j.Spec)
		samples = r.meterOne(ctx, samples, jobMeterKey(j.ID), &spec, derefOr(j.Owner, ""), running, now, "job")
	}
	if len(samples) == 0 {
		return 0, nil
	}
	if err := r.store.RecordUsageSamples(ctx, samples); err != nil {
		return 0, fmt.Errorf("record usage samples: %w", err)
	}
	return len(samples), nil
}

// meterOne appends one workload's samples for this pass: its held demand
// while running, one closing zero per resource on the pass it stops being
// running (so the held level does not persist past its life in the
// integration), nothing otherwise. key identifies the workload in the
// metered set; kind names it in log lines.
func (r *Reconciler) meterOne(ctx context.Context, samples []UsageSample, key core.ClusterId, spec *core.ClusterSpec, owner string, running bool, now uint64, kind string) []UsageSample {
	wasMetered := r.metered[key]
	if !running {
		if wasMetered {
			samples = append(samples, r.zeroSamples(ctx, spec, owner, now)...)
			delete(r.metered, key)
		}
		return samples
	}
	minDemand, _, derr := policy.ClusterDemand(spec)
	if derr != nil {
		slog.Warn("metering: unparseable spec skipped", kind, key, "error", derr)
		return samples
	}
	pool := r.poolFor(ctx, spec.Project)
	for resource, qty := range minDemand {
		samples = append(samples, UsageSample{
			Ts: now, Project: spec.Project, Pool: pool, Owner: owner, Resource: resource, Quantity: qty, Source: UsageSourceObservedSpec,
		})
	}
	r.metered[key] = true
	return samples
}

func (r *Reconciler) zeroSamples(ctx context.Context, spec *core.ClusterSpec, owner string, now uint64) []UsageSample {
	minDemand, _, err := policy.ClusterDemand(spec)
	if err != nil {
		return nil
	}
	pool := r.poolFor(ctx, spec.Project)
	out := make([]UsageSample, 0, len(minDemand))
	for resource := range minDemand {
		out = append(out, UsageSample{Ts: now, Project: spec.Project, Pool: pool, Owner: owner, Resource: resource, Quantity: 0, Source: UsageSourceObservedSpec})
	}
	return out
}

// jobMeterKey is the metered-set key for a job: prefixed so it cannot
// collide with a cluster of the same id.
func jobMeterKey(id core.ClusterId) core.ClusterId { return core.ClusterId("job:" + string(id)) }

// jobDemandSpec projects a job's shape onto the cluster-spec fields
// policy.ClusterDemand reads (project, head, worker groups) so both
// workload kinds share one demand computation instead of two that could
// drift.
func jobDemandSpec(j *core.RayJobSpec) core.ClusterSpec {
	return core.ClusterSpec{Project: j.Project, HeadCpu: j.HeadCpu, HeadMemory: j.HeadMemory, WorkerGroups: j.WorkerGroups}
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// poolFor is the pool the project is allocated to, or "" when it has no
// allocation — the same lookup create's quota admission makes.
func (r *Reconciler) poolFor(ctx context.Context, project string) string {
	pools, err := r.store.ListPools(ctx)
	if err != nil {
		return ""
	}
	for i := range pools {
		allocs, err := r.store.ListAllocations(ctx, pools[i].Name)
		if err != nil {
			continue
		}
		for _, a := range allocs {
			if a.Project == project {
				return pools[i].Name
			}
		}
	}
	return ""
}
