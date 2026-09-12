package api

import (
	"context"
	"strings"
	"testing"

	"github.com/bifrost-compute/bifrost/internal/auth"
	"github.com/bifrost-compute/bifrost/internal/controller"
	"github.com/bifrost-compute/bifrost/internal/core"
)

// --- Prometheus gauge rendering: ported 1:1 from usage.rs's #[cfg(test)]
// module (gauge_renders_latest_sample_per_label_set,
// gauge_escapes_label_values, gauge_empty_when_no_samples,
// cluster_gauges_count_by_state_and_project,
// pool_nominal_gauge_sums_flavors_and_omits_unparseable). ---

func usageSample(ts uint64, pool, project, resource string, qty float64) controller.UsageSample {
	return controller.UsageSample{Ts: ts, Pool: pool, Project: project, Resource: resource, Quantity: qty, Source: controller.UsageSourceObservedSpec}
}

func TestRenderUsageGauge_LatestSamplePerLabelSet(t *testing.T) {
	text := renderUsageGauge([]controller.UsageSample{
		usageSample(100, "gpu", "proj-a", "cpu", 4.0),
		usageSample(200, "gpu", "proj-a", "cpu", 8.0), // newer wins
		usageSample(150, "gpu", "", "cpu", 16.0),
	})
	if !strings.Contains(text, "# TYPE bifrost_pool_resource_usage gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `bifrost_pool_resource_usage{pool="gpu",project="proj-a",owner="",resource="cpu"} 8`) {
		t.Errorf("stale sample not overwritten: %s", text)
	}
	if !strings.Contains(text, `bifrost_pool_resource_usage{pool="gpu",project="",owner="",resource="cpu"} 16`) {
		t.Errorf("pool-aggregate row missing: %s", text)
	}
	if strings.Count(text, "proj-a") != 1 {
		t.Errorf("stale sample overwritten, want exactly one proj-a occurrence: %s", text)
	}
}

func TestPromEscape_EscapesBackslashQuoteNewline(t *testing.T) {
	got := promEscape("a\"b\nc\\d")
	want := `a\"b\nc\\d`
	if got != want {
		t.Errorf("promEscape = %q, want %q", got, want)
	}
}

func TestRenderUsageGauge_EmptyWhenNoSamples(t *testing.T) {
	text := renderUsageGauge(nil)
	if !strings.Contains(text, "# HELP") {
		t.Errorf("missing HELP line: %s", text)
	}
	if strings.Contains(text, "bifrost_pool_resource_usage{") {
		t.Errorf("should have no data rows: %s", text)
	}
}

func stateStr(s core.ClusterState) *core.ClusterState { return &s }

func TestRenderClusterGauges_CountsByStateAndProject(t *testing.T) {
	clusters := []controller.StoredCluster{
		{ID: "c1", Spec: core.ClusterSpec{Project: "proj-a"}, Desired: controller.DesiredRunning, ObservedState: stateStr(core.ClusterStateRunning)},
		{ID: "c2", Spec: core.ClusterSpec{Project: "proj-a"}, Desired: controller.DesiredSuspended, ObservedState: stateStr(core.ClusterStateSuspended)},
		{ID: "c3", Spec: core.ClusterSpec{Project: "proj-b"}, Desired: controller.DesiredRunning, ObservedState: nil},
		// Stopped and awaiting purge: a tombstone, whatever it last observed.
		{ID: "c4", Spec: core.ClusterSpec{Project: "proj-a"}, Desired: controller.DesiredTerminated, ObservedState: nil},
		{ID: "c5", Spec: core.ClusterSpec{Project: "proj-a"}, Desired: controller.DesiredTerminated, ObservedState: stateStr(core.ClusterStateRunning)},
	}
	text := renderClusterGauges(clusters)
	if !strings.Contains(text, "# TYPE bifrost_clusters_total gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `bifrost_clusters_total{state="running"} 1`) {
		t.Errorf("running count wrong: %s", text)
	}
	if !strings.Contains(text, `bifrost_clusters_total{state="suspended"} 1`) {
		t.Errorf("suspended count wrong: %s", text)
	}
	if !strings.Contains(text, `bifrost_clusters_total{state="unknown"} 1`) {
		t.Errorf("unobserved clusters should count as unknown: %s", text)
	}
	if !strings.Contains(text, "# TYPE bifrost_clusters_by_project gauge") {
		t.Errorf("missing by-project TYPE line: %s", text)
	}
	if !strings.Contains(text, `bifrost_clusters_total{state="terminated"} 2`) {
		t.Errorf("tombstones must report terminated, not unknown or their stale observation: %s", text)
	}
	// Per project AND state, so a dashboard can sum live clusters without
	// counting the stopped ones a project has not purged (#37).
	for _, want := range []string{
		`bifrost_clusters_by_project{project="proj-a",state="running"} 1`,
		`bifrost_clusters_by_project{project="proj-a",state="suspended"} 1`,
		`bifrost_clusters_by_project{project="proj-a",state="terminated"} 2`,
		`bifrost_clusters_by_project{project="proj-b",state="unknown"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in:\n%s", want, text)
		}
	}
	if strings.Contains(text, `bifrost_clusters_by_project{project="proj-a"} `) {
		t.Errorf("the old stateless series must not be emitted alongside the new one: %s", text)
	}
}

func TestRenderPoolNominalGauge_SumsFlavorsAndOmitsUnparseable(t *testing.T) {
	pool := func(name string, resources map[string]string) controller.StoredPool {
		return controller.StoredPool{
			Name: name,
			Spec: core.PoolSpec{
				Name:    name,
				Flavors: []core.FlavorSpec{{Name: "f", Resources: resources}},
				Cohort:  "c",
			},
		}
	}
	text := renderPoolNominalGauge([]controller.StoredPool{
		pool("gpu", map[string]string{"cpu": "64", "memory": "256Gi"}),
		pool("bad", map[string]string{"cpu": "not-a-quantity"}),
	})
	if !strings.Contains(text, "# TYPE bifrost_pool_nominal gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `bifrost_pool_nominal{pool="gpu",resource="cpu"} 64`) {
		t.Errorf("gpu cpu total wrong: %s", text)
	}
	if strings.Contains(text, `pool="bad"`) {
		t.Errorf("unparseable quantity should omit the resource: %s", text)
	}
}

// --- Handler-level branch coverage ---

func TestUsageReport_DefaultWindowNoSamples(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	resp, err := s.UsageReport(ctxWithIdentity(testIdentity("v", auth.RoleViewer)), UsageReportRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	report := mustResponse[UsageReport200JSONResponse](t, resp)
	if report.To-report.From != 86_400 {
		t.Errorf("default window = %d, want 86400", report.To-report.From)
	}
	if len(report.Groups) != 0 {
		t.Errorf("groups = %+v, want empty", report.Groups)
	}
}

func TestUsageReport_DeniedWithoutRead(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.UsageReport(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), UsageReportRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

func TestUsageReport_FromMustBeBeforeTo(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	from, to := int64(1000), int64(500)
	_, err := s.UsageReport(ctxWithIdentity(admin()), UsageReportRequestObject{Params: UsageReportParams{From: &from, To: &to}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestUsageReport_GroupsSamplesAndComputesCost(t *testing.T) {
	ctx := context.Background()
	store := controller.NewMemoryStore()
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		usageSample(0, "gpu", "proj-a", "cpu", 1.0),
		usageSample(3600, "gpu", "proj-a", "cpu", 1.0),
	}); err != nil {
		t.Fatalf("record samples: %v", err)
	}
	prices := map[string]float64{"cpu": 2.0}
	quotas := map[string]map[string]float64{}
	if _, err := (&Server{Store: store}).UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{
		Body: &UpdatePolicy{Prices: &prices, Quotas: &quotas},
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	s := &Server{Store: store}
	from, to := int64(0), int64(7200)
	resp, err := s.UsageReport(ctxWithIdentity(admin()), UsageReportRequestObject{Params: UsageReportParams{From: &from, To: &to}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	report := mustResponse[UsageReport200JSONResponse](t, resp)
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %+v, want 1", report.Groups)
	}
	g := report.Groups[0]
	if g.Project != "proj-a" || g.Pool != "gpu" {
		t.Errorf("group = %+v", g)
	}
	if g.ResourceHours["cpu"] != 2.0 {
		t.Errorf("resource_hours[cpu] = %v, want 2.0 (1 cpu held for 2h)", g.ResourceHours["cpu"])
	}
	if g.CostUsd == nil || *g.CostUsd != 4.0 {
		t.Errorf("cost_usd = %v, want 4.0 (2h * $2/hr)", g.CostUsd)
	}
}

// usageReportOK runs the report as admin and fails the test on any error.
func usageReportOK(t *testing.T, s *Server, params UsageReportParams) UsageReport200JSONResponse {
	t.Helper()
	resp, err := s.UsageReport(ctxWithIdentity(admin()), UsageReportRequestObject{Params: params})
	if err != nil {
		t.Fatalf("usage report: %v", err)
	}
	return mustResponse[UsageReport200JSONResponse](t, resp)
}

func TestUsageReport_GroupsByOwnerAndFiltersOnIt(t *testing.T) {
	ctx := context.Background()
	store := controller.NewMemoryStore()
	owned := func(ts uint64, owner string, qty float64) controller.UsageSample {
		s := usageSample(ts, "gpu", "proj-a", "cpu", qty)
		s.Owner = owner
		return s
	}
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		owned(0, "dev-a", 1.0), owned(0, "dev-b", 2.0), owned(0, "", 4.0),
	}); err != nil {
		t.Fatalf("record samples: %v", err)
	}
	s := &Server{Store: store}
	from, to := int64(0), int64(3600)
	report := usageReportOK(t, s, UsageReportParams{From: &from, To: &to})
	if len(report.Groups) != 3 {
		t.Fatalf("groups = %+v, want one per owner", report.Groups)
	}
	// Same project and pool: sorted by owner, "" first.
	wantOwners := []string{"", "dev-a", "dev-b"}
	wantCPU := []float64{4.0, 1.0, 2.0}
	for i, g := range report.Groups {
		if g.Owner == nil || *g.Owner != wantOwners[i] || g.ResourceHours["cpu"] != wantCPU[i] {
			t.Errorf("group[%d] = %+v (owner %v), want owner %q cpu %v", i, g, g.Owner, wantOwners[i], wantCPU[i])
		}
	}

	devA := "dev-a"
	report = usageReportOK(t, s, UsageReportParams{From: &from, To: &to, Owner: &devA})
	if len(report.Groups) != 1 || report.Groups[0].Owner == nil || *report.Groups[0].Owner != "dev-a" {
		t.Fatalf("owner=dev-a groups = %+v, want only dev-a", report.Groups)
	}
	// owner="" is a real filter (unattributed), not "no filter".
	empty := ""
	report = usageReportOK(t, s, UsageReportParams{From: &from, To: &to, Owner: &empty})
	if len(report.Groups) != 1 || *report.Groups[0].Owner != "" || report.Groups[0].ResourceHours["cpu"] != 4.0 {
		t.Fatalf("owner=\"\" groups = %+v, want only the unattributed group", report.Groups)
	}
	nobody := "nobody"
	report = usageReportOK(t, s, UsageReportParams{From: &from, To: &to, Owner: &nobody})
	if len(report.Groups) != 0 {
		t.Fatalf("owner=nobody groups = %+v, want none", report.Groups)
	}
}

func TestRenderUsageGauge_CarriesOwnerLabel(t *testing.T) {
	s := usageSample(100, "gpu", "proj-a", "cpu", 4.0)
	s.Owner = "dev-a"
	text := renderUsageGauge([]controller.UsageSample{s})
	if !strings.Contains(text, `bifrost_pool_resource_usage{pool="gpu",project="proj-a",owner="dev-a",resource="cpu"} 4`) {
		t.Errorf("owner label missing: %s", text)
	}
}

func TestUsageReport_BudgetStatus(t *testing.T) {
	ctx := context.Background()
	store := controller.NewMemoryStore()
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		usageSample(0, "gpu", "proj-a", "cpu", 5.0),
	}); err != nil {
		t.Fatalf("record samples: %v", err)
	}
	budgets := map[string]BudgetView{"proj-a": {WindowSecs: 86_400, AdditionalProperties: map[string]float64{"cpu": 1.0}}}
	if _, err := (&Server{Store: store}).UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{
		Body: &UpdatePolicy{Budgets: &budgets},
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	s := &Server{Store: store}
	resp, err := s.UsageReport(ctxWithIdentity(admin()), UsageReportRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	report := mustResponse[UsageReport200JSONResponse](t, resp)
	if len(report.Budgets) != 1 {
		t.Fatalf("budgets = %+v, want 1", report.Budgets)
	}
	b := report.Budgets[0]
	if b.Project != "proj-a" || !b.Exhausted {
		t.Errorf("budget = %+v, want exhausted (5h consumed > 1h cap, carry-in from ts=0)", b)
	}
}

func TestMetrics_RendersAllThreeGaugeSections(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	resp, err := s.Metrics(ctxWithIdentity(admin()), MetricsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := string(mustResponse[Metrics200TextResponse](t, resp))
	for _, want := range []string{"bifrost_pool_resource_usage", "bifrost_clusters_total", "bifrost_pool_nominal"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q section: %s", want, text)
		}
	}
}

func TestMetrics_DeniedWithoutRead(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.Metrics(ctxWithIdentity(&auth.Identity{Subject: "nobody"}), MetricsRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}

// The scoping tests. The report used to gate on a global Read, which refused
// the project-only member (roles [], one project grant) the ledger is for
// and left the dashboard's Usage page empty for the people whose usage it
// shows (#39). It is a list endpoint now: readGate in, tenant access per row.
func seedTenantUsage(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	store := controller.NewMemoryStore()
	owned := func(project, owner string, qty float64) controller.UsageSample {
		smp := usageSample(0, "gpu", project, "cpu", qty)
		smp.Owner = owner
		return smp
	}
	if err := store.RecordUsageSamples(ctx, []controller.UsageSample{
		owned("team-a", "pam", 1.0),  // pam's own cluster in her project
		owned("team-a", "pete", 2.0), // a teammate's
		owned("team-b", "bob", 4.0),  // another project entirely
		owned("", "", 8.0),           // pool-level, unattributed
	}); err != nil {
		t.Fatal(err)
	}
	return &Server{Store: store}
}

func usageReportAs(t *testing.T, s *Server, id *auth.Identity) UsageReport200JSONResponse {
	t.Helper()
	from, to := int64(0), int64(3600)
	resp, err := s.UsageReport(ctxWithIdentity(id), UsageReportRequestObject{Params: UsageReportParams{From: &from, To: &to}})
	if err != nil {
		t.Fatalf("usage report as %s: %v", id.Subject, err)
	}
	return mustResponse[UsageReport200JSONResponse](t, resp)
}

func ownersOf(r UsageReport200JSONResponse) []string {
	out := []string{}
	for _, g := range r.Groups {
		out = append(out, g.Project+"/"+*g.Owner)
	}
	return out
}

func TestUsageReport_ProjectOnlyMemberSeesTheirProject(t *testing.T) {
	s := seedTenantUsage(t)
	// No global role at all; operator on team-a. The identity every
	// Keycloak group mapping produces.
	got := ownersOf(usageReportAs(t, s, projectMember("pam", auth.RoleOperator, "team-a")))
	want := []string{"team-a/pam", "team-a/pete"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("project member sees %v, want %v (her project, all of it; not team-b, not the pool row)", got, want)
	}
}

func TestUsageReport_GlobalViewerSeesOnlyWhatTheyOwn(t *testing.T) {
	s := seedTenantUsage(t)
	got := ownersOf(usageReportAs(t, s, testIdentity("pam", auth.RoleViewer)))
	if strings.Join(got, ",") != "team-a/pam" {
		t.Fatalf("global viewer with no project ties sees %v, want only their own rows", got)
	}
}

func TestUsageReport_AdminAndAuditorSeeEverything(t *testing.T) {
	s := seedTenantUsage(t)
	for _, id := range []*auth.Identity{admin(), testIdentity("audit", auth.RoleAuditor)} {
		if got := ownersOf(usageReportAs(t, s, id)); len(got) != 4 {
			t.Fatalf("%s sees %v, want all four groups", id.Subject, got)
		}
	}
}

func TestUsageReport_BudgetsFollowTheSameScope(t *testing.T) {
	s := seedTenantUsage(t)
	budgets := map[string]BudgetView{
		"team-a": {WindowSecs: 3600, AdditionalProperties: map[string]float64{"cpu": 100}},
		"team-b": {WindowSecs: 3600, AdditionalProperties: map[string]float64{"cpu": 100}},
	}
	if _, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Budgets: &budgets}}); err != nil {
		t.Fatal(err)
	}
	r := usageReportAs(t, s, projectMember("pam", auth.RoleOperator, "team-a"))
	if len(r.Budgets) != 1 || r.Budgets[0].Project != "team-a" {
		t.Fatalf("project member's budgets = %+v, want team-a only", r.Budgets)
	}
	if r := usageReportAs(t, s, admin()); len(r.Budgets) != 2 {
		t.Fatalf("admin's budgets = %+v, want both", r.Budgets)
	}
}
