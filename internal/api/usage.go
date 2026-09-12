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

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/policy"
)

// promEscape escapes a Prometheus label value (\, ", newline). Ported from
// usage.rs's prom_escape.
func promEscape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// usageScope is what one caller may see of the usage ledger, settled once
// per request. The report is a list endpoint, so it follows the list rule
// (readGate + per-row tenant access) rather than the global Authorize it
// used to carry: that check refused the caller the ledger exists for — the
// project-only member, roles [] and a project grant, every Keycloak-group
// user — and left the dashboard's Usage page empty for exactly the people
// whose usage it shows (#39).
//
// Admin and Auditor see everything. Everyone else sees rows attributed to
// them, and rows of projects they hold a read-granting scoped assignment
// in. A global viewer with no project ties sees only what they own — the
// same boundary clusterTenantAccess draws for clusters.
type usageScope struct {
	all         bool
	owner       string
	assignments []auth.RoleScope
}

func newUsageScope(ctx context.Context, store controller.Store, identity *auth.Identity) usageScope {
	if identity == nil || hasRole(identity, auth.RoleAdmin, auth.RoleAuditor) {
		return usageScope{all: true}
	}
	return usageScope{owner: identity.Owner(), assignments: EffectiveAssignments(ctx, store, identity)}
}

// project reports whether the caller may read project-level facts about p
// (a budget, an unattributed pool row).
func (u usageScope) project(p string) bool {
	if u.all {
		return true
	}
	for _, a := range u.assignments {
		if a.Scope != auth.GlobalScope && auth.ScopeCovers(a.Scope, p) && a.Role.Grants(auth.Read, auth.TargetCluster) {
			return true
		}
	}
	return false
}

// sample reports whether one ledger row is the caller's to see.
func (u usageScope) sample(smp controller.UsageSample) bool {
	if u.all {
		return true
	}
	if smp.Owner != "" && smp.Owner == u.owner {
		return true
	}
	return smp.Project != "" && u.project(smp.Project)
}

// UsageReport reports resource-hours (and cost when priced) by project,
// pool and owner over a window, plus configured projects' time-windowed
// budget status, narrowed to what the caller may see (usageScope). The
// `owner` query parameter narrows to one identity's consumption
// (requirement 14's "who"); an owner of "" selects unattributed samples.
func (s *Server) UsageReport(ctx context.Context, req UsageReportRequestObject) (UsageReportResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := readGate(ctx, s.Store, identity, auth.TargetCluster); err != nil {
		return nil, err
	}
	scope := newUsageScope(ctx, s.Store, identity)
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
		if !scope.sample(smp) {
			continue
		}
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
		if !scope.project(bp) {
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
// stateLabel is the `state` a gauge reports for one stored record. A record
// whose desired state is terminated is a tombstone — a stopped cluster kept
// until purge — and says so, whatever its last observation was; it used to
// report "unknown" beside clusters that were merely new, and a dashboard read
// 104 clusters in a project that had one (#37). Otherwise the observed
// state, or "unknown" until the reconcile engine has observed anything.
func stateLabel(c *controller.StoredCluster) string {
	if c.Desired == controller.DesiredTerminated {
		return "terminated"
	}
	if c.ObservedState == nil {
		return "unknown"
	}
	return c.ObservedState.String()
}

// renderClusterGauges renders bifrost_clusters_total{state} (counts by
// observed state, "unknown" until the reconcile engine's first observation
// lands) and bifrost_clusters_by_project{project} (counts per spec
// project). Both reflect the store as it is — Terminated rows count until
// the store reaps them. Ported from usage.rs's render_cluster_gauges.
func renderClusterGauges(clusters []controller.StoredCluster) string {
	type projectState struct{ project, state string }
	byState := map[string]int{}
	byProject := map[projectState]int{}
	for i := range clusters {
		c := &clusters[i]
		st := stateLabel(c)
		byState[st]++
		byProject[projectState{c.Spec.Project, st}]++
	}
	var b strings.Builder
	b.WriteString("# HELP bifrost_clusters_total Managed cluster records by state: the observed state, " +
		"'unknown' before the reconcile engine's first observation, 'terminated' for a stopped cluster's record awaiting purge.\n" +
		"# TYPE bifrost_clusters_total gauge\n")
	for _, st := range sortedKeys(byState) {
		fmt.Fprintf(&b, "bifrost_clusters_total{state=%q} %d\n", promEscape(st), byState[st])
	}
	b.WriteString("# HELP bifrost_clusters_by_project Managed cluster records per project and state " +
		"(same state values as bifrost_clusters_total; sum over state!=\"terminated\" for live clusters).\n" +
		"# TYPE bifrost_clusters_by_project gauge\n")
	keys := make([]projectState, 0, len(byProject))
	for k := range byProject {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].state < keys[j].state
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "bifrost_clusters_by_project{project=%q,state=%q} %d\n", promEscape(k.project), promEscape(k.state), byProject[k])
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
