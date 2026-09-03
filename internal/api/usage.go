// Usage reporting API (Slice 4): the timeseries read path for the samples
// the metering loop (internal/controller) appends, plus a Prometheus
// text-format gauge for scraping.
//
// GET /api/v1/usage is consumption *reporting*, not pool topology, so it
// checks Read on Target::Cluster (Viewer+) — the same permission as
// reading cluster costs — rather than Target::Pool. The choice is
// deliberate (documented in usage.rs and carried forward here). The
// metrics endpoint shares it: usage data is no more sensitive than the
// report API, and scrape tokens are just Bearer JWTs.
//
// Aggregation semantics live in internal/policy (step function with
// carry-in). Grouping is by (project, pool, owner); the pool-level aggregate rows
// the Kueue path writes carry project = "" and OVERLAP the per-project
// rows — consumers must not sum across project boundaries. Ported from
// the Rust predecessor's usage.rs.
package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/policy"
)

// promEscape escapes a Prometheus label value (\, ", newline). Ported from
// usage.rs's prom_escape.
func promEscape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// UsageReport reports resource-hours (and cost when priced) by project,
// pool and owner over a window, plus configured projects' time-windowed
// budget status. Read on Target::Cluster. The `owner` query parameter
// narrows to one identity's consumption (requirement 14's "who"); an
// owner of "" selects unattributed samples.
func (s *Server) UsageReport(ctx context.Context, req UsageReportRequestObject) (UsageReportResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}
	q := req.Params
	to := controller.NowUnix()
	if q.To != nil {
		to = uint64(*q.To)
	}
	from := satSub(to, 86_400)
	if q.From != nil {
		from = uint64(*q.From)
	}
	if from >= to {
		return nil, badRequest("from must be before to")
	}

	// Query from 0, not `from`: a sample BEFORE the window sets the level
	// entering it (carry-in — see policy.ResourceHours).
	samples, err := s.Store.UsageSamples(ctx, q.Project, q.Pool, q.Owner, 0, to)
	if err != nil {
		return nil, wrapStoreErr(err)
	}

	type groupKey struct{ project, pool, owner string }
	grouped := map[groupKey]map[string][]policy.UsageSampleView{}
	for _, smp := range samples {
		key := groupKey{smp.Project, smp.Pool, smp.Owner}
		if grouped[key] == nil {
			grouped[key] = map[string][]policy.UsageSampleView{}
		}
		grouped[key][smp.Resource] = append(grouped[key][smp.Resource], policy.UsageSampleView{TS: smp.Ts, Quantity: smp.Quantity})
	}

	// The effective price sheet is store-backed, read per request, so a
	// settings edit applies to the very next report.
	storedPolicy, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	cfg := PolicyConfig{}
	if storedPolicy != nil {
		cfg = configFromStored(storedPolicy)
	}

	keys := make([]groupKey, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		if keys[i].pool != keys[j].pool {
			return keys[i].pool < keys[j].pool
		}
		return keys[i].owner < keys[j].owner
	})
	groups := make([]UsageGroup, 0, len(keys))
	for _, key := range keys {
		byResource := grouped[key]
		resourceHours := make(map[string]float64, len(byResource))
		for r, pts := range byResource {
			resourceHours[r] = policy.ResourceHours(pts, from, to)
		}
		var costUSD *float64
		if cfg.Prices != nil {
			c := policy.Cost(policy.ResourceMap(resourceHours), cfg.Prices)
			costUSD = &c
		}
		owner := key.owner
		groups = append(groups, UsageGroup{Project: key.project, Pool: key.pool, Owner: &owner, ResourceHours: resourceHours, CostUsd: costUSD})
	}

	// Budget status (#77): for each configured project (filtered to
	// q.project when set), compute consumption over the budget's OWN
	// trailing window ending at `to` — independent of the report's
	// from/to, which the client controls freely.
	var budgetProjects []string
	for p := range cfg.Budgets {
		budgetProjects = append(budgetProjects, p)
	}
	sort.Strings(budgetProjects)
	budgets := make([]BudgetStatus, 0, len(budgetProjects))
	for _, bp := range budgetProjects {
		if q.Project != nil && *q.Project != bp {
			continue
		}
		budget := cfg.Budgets[bp]
		bFrom := satSub(to, budget.WindowSecs)
		consumed, cerr := s.windowedConsumption(ctx, bp, bFrom, to)
		if cerr != nil {
			return nil, wrapStoreErr(cerr)
		}
		remaining := make(map[string]float64, len(budget.Limits))
		for r, limit := range budget.Limits {
			used := consumed[r]
			v := limit - used
			if v < 0 {
				v = 0
			}
			remaining[r] = v
		}
		b := budget
		exhausted := policy.AdmitBudget(bp, &b, consumed) != nil
		budgets = append(budgets, BudgetStatus{
			Project: bp, WindowSecs: int64(budget.WindowSecs),
			Limit: budget.Limits, Consumed: consumed, Remaining: remaining, Exhausted: exhausted,
		})
	}

	return UsageReport200JSONResponse(UsageReport{From: int64(from), To: int64(to), Groups: groups, Budgets: budgets}), nil
}

// stateLabel is the label value for a cluster's observed state.
// ClusterState already serializes to its snake_case wire string via
// String(); reuse it instead of a parallel match that could drift.
func stateLabel(s *core.ClusterState) string {
	if s == nil {
		return "unknown"
	}
	return s.String()
}

// renderClusterGauges renders bifrost_clusters_total{state} (counts by
// observed state, "unknown" until the reconcile engine's first observation
// lands) and bifrost_clusters_by_project{project} (counts per spec
// project). Both reflect the store as it is — Terminated rows count until
// the store reaps them. Ported from usage.rs's render_cluster_gauges.
func renderClusterGauges(clusters []controller.StoredCluster) string {
	byState := map[string]int{}
	byProject := map[string]int{}
	for i := range clusters {
		c := &clusters[i]
		byState[stateLabel(c.ObservedState)]++
		byProject[c.Spec.Project]++
	}
	var b strings.Builder
	b.WriteString("# HELP bifrost_clusters_total Managed clusters by observed state " +
		"('unknown' before the reconcile engine's first observation).\n" +
		"# TYPE bifrost_clusters_total gauge\n")
	states := sortedKeys(byState)
	for _, st := range states {
		fmt.Fprintf(&b, "bifrost_clusters_total{state=%q} %d\n", promEscape(st), byState[st])
	}
	b.WriteString("# HELP bifrost_clusters_by_project Managed clusters per project.\n" +
		"# TYPE bifrost_clusters_by_project gauge\n")
	for _, p := range sortedKeys(byProject) {
		fmt.Fprintf(&b, "bifrost_clusters_by_project{project=%q} %d\n", promEscape(p), byProject[p])
	}
	return b.String()
}

// renderPoolNominalGauge renders bifrost_pool_nominal{pool,resource} (#52):
// each pool's nominal quota, summed across flavors, with the same
// fail-soft policy as pools.go's poolView.total_nominal. Ported from
// usage.rs's render_pool_nominal_gauge.
func renderPoolNominalGauge(pools []controller.StoredPool) string {
	var b strings.Builder
	b.WriteString("# HELP bifrost_pool_nominal Pool nominal quota per resource, summed " +
		"across the pool's flavor specs.\n# TYPE bifrost_pool_nominal gauge\n")
	for i := range pools {
		p := &pools[i]
		sums, unparseable := sumFlavorResources(p.Name, p.Spec.Flavors)
		for _, k := range sortedKeys(sums) {
			if unparseable[k] {
				continue
			}
			fmt.Fprintf(&b, "bifrost_pool_nominal{pool=%q,resource=%q} %v\n", promEscape(p.Name), promEscape(k), sums[k])
		}
	}
	return b.String()
}

// renderUsageGauge renders the latest usage sample per (pool, project,
// owner, resource) as Prometheus text exposition. Ported from usage.rs's
// render_usage_gauge; the owner label is requirement 14's per-user
// attribution ("" = unattributed).
func renderUsageGauge(samples []controller.UsageSample) string {
	type key struct{ pool, project, owner, resource string }
	latest := map[key]float64{}
	for _, smp := range samples {
		// Samples arrive ts-ordered; last wins.
		latest[key{smp.Pool, smp.Project, smp.Owner, smp.Resource}] = smp.Quantity
	}
	keys := make([]key, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pool != keys[j].pool {
			return keys[i].pool < keys[j].pool
		}
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		if keys[i].owner != keys[j].owner {
			return keys[i].owner < keys[j].owner
		}
		return keys[i].resource < keys[j].resource
	})
	var b strings.Builder
	b.WriteString("# HELP bifrost_pool_resource_usage Latest metered resource usage " +
		"(Kueue reservation ledger or observed-spec estimate).\n" +
		"# TYPE bifrost_pool_resource_usage gauge\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "bifrost_pool_resource_usage{pool=%q,project=%q,owner=%q,resource=%q} %v\n",
			promEscape(k.pool), promEscape(k.project), promEscape(k.owner), promEscape(k.resource), latest[k])
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Metrics serves the Prometheus text exposition of usage and control-plane
// gauges. Read on Target::Cluster (same rationale as UsageReport).
func (s *Server) Metrics(ctx context.Context, _ MetricsRequestObject) (MetricsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}
	samples, err := s.Store.UsageSamples(ctx, nil, nil, nil, 0, controller.NowUnix())
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	clusters, err := s.Store.List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	pools, err := s.Store.ListPools(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	text := renderUsageGauge(samples) + renderClusterGauges(clusters) + renderPoolNominalGauge(pools)
	return Metrics200TextResponse(text), nil
}
