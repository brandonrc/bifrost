package api

import (
	"context"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
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
	if !strings.Contains(text, "# TYPE mobula_pool_resource_usage gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `mobula_pool_resource_usage{pool="gpu",project="proj-a",resource="cpu"} 8`) {
		t.Errorf("stale sample not overwritten: %s", text)
	}
	if !strings.Contains(text, `mobula_pool_resource_usage{pool="gpu",project="",resource="cpu"} 16`) {
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
	if strings.Contains(text, "mobula_pool_resource_usage{") {
		t.Errorf("should have no data rows: %s", text)
	}
}

func stateStr(s core.ClusterState) *core.ClusterState { return &s }

func TestRenderClusterGauges_CountsByStateAndProject(t *testing.T) {
	clusters := []controller.StoredCluster{
		{ID: "c1", Spec: core.ClusterSpec{Project: "proj-a"}, ObservedState: stateStr(core.ClusterStateRunning)},
		{ID: "c2", Spec: core.ClusterSpec{Project: "proj-a"}, ObservedState: stateStr(core.ClusterStateSuspended)},
		{ID: "c3", Spec: core.ClusterSpec{Project: "proj-b"}, ObservedState: nil},
	}
	text := renderClusterGauges(clusters)
	if !strings.Contains(text, "# TYPE mobula_clusters_total gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `mobula_clusters_total{state="running"} 1`) {
		t.Errorf("running count wrong: %s", text)
	}
	if !strings.Contains(text, `mobula_clusters_total{state="suspended"} 1`) {
		t.Errorf("suspended count wrong: %s", text)
	}
	if !strings.Contains(text, `mobula_clusters_total{state="unknown"} 1`) {
		t.Errorf("unobserved clusters should count as unknown: %s", text)
	}
	if !strings.Contains(text, "# TYPE mobula_clusters_by_project gauge") {
		t.Errorf("missing by-project TYPE line: %s", text)
	}
	if !strings.Contains(text, `mobula_clusters_by_project{project="proj-a"} 2`) {
		t.Errorf("proj-a count wrong: %s", text)
	}
	if !strings.Contains(text, `mobula_clusters_by_project{project="proj-b"} 1`) {
		t.Errorf("proj-b count wrong: %s", text)
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
	if !strings.Contains(text, "# TYPE mobula_pool_nominal gauge") {
		t.Errorf("missing TYPE line: %s", text)
	}
	if !strings.Contains(text, `mobula_pool_nominal{pool="gpu",resource="cpu"} 64`) {
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
	for _, want := range []string{"mobula_pool_resource_usage", "mobula_clusters_total", "mobula_pool_nominal"} {
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
