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
// attributed to the cluster's project and its pool (empty when the project
// has no allocation), tagged observed_spec. Suspended and terminated
// clusters hold nothing and record nothing; the step series then reads as
// zero from the last running sample onward once a zero sample closes it.
func (r *Reconciler) Meter(ctx context.Context, now uint64) (int, error) {
	clusters, err := r.store.List(ctx)
	if err != nil {
		return 0, err
	}
	var samples []UsageSample
	for i := range clusters {
		c := &clusters[i]
		running := c.ObservedState != nil && *c.ObservedState == core.ClusterStateRunning && c.Desired == DesiredRunning
		wasMetered := r.metered[c.ID]
		if !running {
			if wasMetered {
				// Close the step: a zero reading so the held level does not
				// persist past the cluster's life in the integration.
				samples = append(samples, r.zeroSamples(ctx, c, now)...)
				delete(r.metered, c.ID)
			}
			continue
		}
		minDemand, _, derr := policy.ClusterDemand(&c.Spec)
		if derr != nil {
			slog.Warn("metering: unparseable spec skipped", "cluster", c.ID, "error", derr)
			continue
		}
		pool := r.poolFor(ctx, c.Spec.Project)
		for resource, qty := range minDemand {
			samples = append(samples, UsageSample{
				Ts: now, Project: c.Spec.Project, Pool: pool, Resource: resource, Quantity: qty, Source: UsageSourceObservedSpec,
			})
		}
		r.metered[c.ID] = true
	}
	if len(samples) == 0 {
		return 0, nil
	}
	if err := r.store.RecordUsageSamples(ctx, samples); err != nil {
		return 0, fmt.Errorf("record usage samples: %w", err)
	}
	return len(samples), nil
}

func (r *Reconciler) zeroSamples(ctx context.Context, c *StoredCluster, now uint64) []UsageSample {
	minDemand, _, err := policy.ClusterDemand(&c.Spec)
	if err != nil {
		return nil
	}
	pool := r.poolFor(ctx, c.Spec.Project)
	out := make([]UsageSample, 0, len(minDemand))
	for resource := range minDemand {
		out = append(out, UsageSample{Ts: now, Project: c.Spec.Project, Pool: pool, Resource: resource, Quantity: 0, Source: UsageSourceObservedSpec})
	}
	return out
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
