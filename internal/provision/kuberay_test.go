package provision

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	"github.com/brandonrc/bifrost/internal/core"
)

// Test parity: this file ports mobula-provision/src/kuberay.rs's `tests`
// module (kuberay.rs:1065-1872, the manifest-construction subset — see
// task-5-report.md's parity table for the node/events/logs functions this
// port deliberately leaves for Task 6, which does the Kubernetes reads
// they consume).

// marshal round-trips v through JSON and unmarshals into a generic map so
// tests can assert on the wire shape, matching the Rust reference's
// assertions against a serde_json::Value.
func marshal(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func testSpec(t *testing.T, groups ...core.WorkerGroup) *core.ClusterSpec {
	t.Helper()
	return &core.ClusterSpec{
		Name:         "demo",
		Project:      "p",
		RayVersion:   "2.57.0",
		Image:        "rayproject/ray:2.57.0",
		HeadCpu:      "1",
		HeadMemory:   "2Gi",
		WorkerGroups: groups,
	}
}

func wg(name string, min, max, replicas uint32) core.WorkerGroup {
	return core.WorkerGroup{Name: name, Cpu: "1", Memory: "2Gi", MinReplicas: min, MaxReplicas: max, Replicas: replicas}
}

// kuberay.rs: manifest_shape_and_labels
func TestManifestShapeAndLabels(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if rc.APIVersion != "ray.io/v1" || rc.Kind != "RayCluster" {
		t.Fatalf("apiVersion/kind = %q/%q", rc.APIVersion, rc.Kind)
	}
	if rc.Name != "demo" {
		t.Fatalf("name = %q", rc.Name)
	}
	if rc.Labels[ManagedByLabel] != "bifrost" {
		t.Fatalf("managed-by label = %q", rc.Labels[ManagedByLabel])
	}
	if rc.Labels[ClusterIDLabel] != "demo" {
		t.Fatalf("cluster-id label = %q", rc.Labels[ClusterIDLabel])
	}
	if rc.Spec.RayVersion != "2.57.0" {
		t.Fatalf("rayVersion = %q", rc.Spec.RayVersion)
	}
	if rc.Spec.HeadGroupSpec.Template.Spec.Containers[0].Image != "rayproject/ray:2.57.0" {
		t.Fatalf("head image = %q", rc.Spec.HeadGroupSpec.Template.Spec.Containers[0].Image)
	}

	m := marshal(t, rc)
	spec2, ok := m["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec = %#v", m["spec"])
	}
	if spec2["rayVersion"] != "2.57.0" {
		t.Fatalf("marshaled rayVersion = %v", spec2["rayVersion"])
	}
}

// kuberay.rs: autoscaling_off_sets_replicas
func TestAutoscalingOffSetsReplicas(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if got := *rc.Spec.EnableInTreeAutoscaling; got != false {
		t.Fatalf("enableInTreeAutoscaling = %v", got)
	}
	w := rc.Spec.WorkerGroupSpecs[0]
	if w.Replicas == nil || *w.Replicas != 2 {
		t.Fatalf("replicas = %v", w.Replicas)
	}
	if *w.MinReplicas != 0 || *w.MaxReplicas != 4 {
		t.Fatalf("min/max = %d/%d", *w.MinReplicas, *w.MaxReplicas)
	}
	// Workers must carry the cluster image or the API server rejects the
	// pod.
	if w.Template.Spec.Containers[0].Image != "rayproject/ray:2.57.0" {
		t.Fatalf("worker image = %q", w.Template.Spec.Containers[0].Image)
	}
}

// kuberay.rs: autoscaling_on_omits_replicas_adr_0007 — the CRITICAL
// ADR-0007 invariant: with the in-tree autoscaler on, `replicas` MUST be
// absent from the marshaled worker group so the autoscaler sidecar keeps
// sole ownership.
func TestAutoscalingOnOmitsReplicasADR0007(t *testing.T) {
	spec := testSpec(t, wg("cpu", 1, 8, 3))
	rc, err := RayClusterFor("demo", spec, true, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	w := rc.Spec.WorkerGroupSpecs[0]
	if got := *rc.Spec.EnableInTreeAutoscaling; got != true {
		t.Fatalf("enableInTreeAutoscaling = %v", got)
	}
	if w.Replicas != nil {
		t.Fatalf("replicas must be unset when autoscaling, got %v", *w.Replicas)
	}
	if *w.MinReplicas != 1 || *w.MaxReplicas != 8 {
		t.Fatalf("min/max = %d/%d", *w.MinReplicas, *w.MaxReplicas)
	}

	// Assert against the marshaled JSON too: the field must be entirely
	// absent, not present-and-null (pins shape fidelity, not just the Go
	// pointer being nil).
	m := marshal(t, rc)
	specMap, ok := m["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec = %#v", m["spec"])
	}
	wgs, ok := specMap["workerGroupSpecs"].([]any)
	if !ok || len(wgs) == 0 {
		t.Fatalf("workerGroupSpecs = %#v", specMap["workerGroupSpecs"])
	}
	w0, ok := wgs[0].(map[string]any)
	if !ok {
		t.Fatalf("workerGroupSpecs[0] = %#v", wgs[0])
	}
	if _, present := w0["replicas"]; present {
		t.Fatalf("marshaled manifest must omit replicas entirely, got %v", w0["replicas"])
	}
	// scaleStrategy is the sidecar's: we must never populate
	// WorkersToDelete. Unlike Replicas (a pointer field, fully omittable),
	// ScaleStrategy is a non-pointer struct field on WorkerGroupSpec, so
	// Go's encoding/json cannot omit the key itself even tagged
	// `omitempty` (that only elides zero scalars/nil pointers/empty
	// collections, never a zero-value struct) — a typed-API impedance
	// mismatch the untyped Rust reference didn't have. The key is present
	// as `{}`, which KubeRay treats identically to absent; the real
	// invariant is that we never write workersToDelete.
	scaleStrategy, ok := w0["scaleStrategy"].(map[string]any)
	if !ok {
		t.Fatalf("scaleStrategy = %#v", w0["scaleStrategy"])
	}
	if _, present := scaleStrategy["workersToDelete"]; present {
		t.Fatalf("workersToDelete must be unset: the sidecar owns it")
	}
}

// kuberay.rs: gpu_workers_get_resource_limits
func TestGPUWorkersGetResourceLimits(t *testing.T) {
	g := wg("gpu", 0, 2, 1)
	gpu := "1"
	g.Gpu = &gpu
	spec := testSpec(t, g)
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	res := rc.Spec.WorkerGroupSpecs[0].Template.Spec.Containers[0].Resources
	if q := res.Limits[corev1.ResourceName("nvidia.com/gpu")]; q.String() != "1" {
		t.Fatalf("gpu limit = %q", q.String())
	}
	if q := res.Requests[corev1.ResourceName("nvidia.com/gpu")]; q.String() != "1" {
		t.Fatalf("gpu request = %q", q.String())
	}
}

// kuberay.rs: to_raycluster_sets_suspend_false
func TestRayClusterSetsSuspendFalse(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if rc.Spec.Suspend == nil || *rc.Spec.Suspend != false {
		t.Fatalf("suspend = %v", rc.Spec.Suspend)
	}
}

// kuberay.rs: suspend_patch_flips_only_the_suspend_field
func TestSuspendPatchFlipsOnlyTheSuspendField(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal(SuspendPatch(true), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	specMap, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec = %#v", got["spec"])
	}
	if specMap["suspend"] != true {
		t.Fatalf("suspend = %v", specMap["suspend"])
	}
	if len(specMap) != 1 {
		t.Fatalf("patch must carry nothing else, got %v", specMap)
	}
	if err := json.Unmarshal(SuspendPatch(false), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	specMap, ok = got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec = %#v", got["spec"])
	}
	if specMap["suspend"] != false {
		t.Fatalf("suspend = %v", specMap)
	}
}

// kuberay.rs: owned_fingerprint_round_trips_through_the_manifest
func TestOwnedFingerprintRoundTripsThroughTheManifest(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	fromCR, ok := FingerprintFromRayCluster(&rc.Spec)
	if !ok {
		t.Fatalf("FingerprintFromRayCluster: not ok")
	}
	if want := OwnedSpecFingerprint(spec); want != fromCR {
		t.Fatalf("fingerprint mismatch:\nwant %s\ngot  %s", want, fromCR)
	}
}

// kuberay.rs: owned_fingerprint_ignores_replicas_but_catches_image
func TestOwnedFingerprintIgnoresReplicasButCatchesImage(t *testing.T) {
	a := testSpec(t, wg("cpu", 0, 4, 2))
	b := testSpec(t, wg("cpu", 0, 4, 9)) // only replicas differ
	if OwnedSpecFingerprint(a) != OwnedSpecFingerprint(b) {
		t.Fatalf("replica delta must not change the fingerprint")
	}
	b.Image = "rayproject/ray:9.9.9"
	if OwnedSpecFingerprint(a) == OwnedSpecFingerprint(b) {
		t.Fatalf("an image edit must change the fingerprint")
	}
}

// kuberay.rs: status_mapping
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		state string
		want  core.ClusterState
	}{
		{"ready", core.ClusterStateRunning},
		{"suspended", core.ClusterStateSuspended},
		{"unhealthy", core.ClusterStateDegraded},
		{"", core.ClusterStateProvisioning},
	}
	for _, c := range cases {
		got := StatusToState(rayv1.RayClusterStatus{State: rayv1.ClusterState(c.state)}) //nolint:staticcheck // SA1019: exercising the ported (deprecated) field, see StatusToState's doc comment
		if got != c.want {
			t.Errorf("StatusToState(%q) = %q, want %q", c.state, got, c.want)
		}
	}
}

// kuberay.rs: no_queue_assignment_is_byte_identical_to_before
func TestNoQueueAssignmentIsByteIdenticalToBefore(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if _, ok := rc.Labels[QueueLabel]; ok {
		t.Fatalf("queue label must be absent")
	}
	if _, ok := rc.Annotations[ElasticJobAnnotation]; ok {
		t.Fatalf("elastic annotation must be absent")
	}
	if *rc.Spec.EnableInTreeAutoscaling != false {
		t.Fatalf("autoscaling flag must be untouched")
	}
}

// kuberay.rs: queue_assignment_stamps_queue_label
func TestQueueAssignmentStampsQueueLabel(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	q := &QueueAssignment{QueueName: "proj-a", Elastic: false}
	rc, err := RayClusterFor("demo", spec, false, 1, q)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if rc.Labels[QueueLabel] != "proj-a" {
		t.Fatalf("queue label = %q", rc.Labels[QueueLabel])
	}
	if _, ok := rc.Annotations[ElasticJobAnnotation]; ok {
		t.Fatalf("non-elastic must not stamp the elastic annotation")
	}
	if *rc.Spec.EnableInTreeAutoscaling != false {
		t.Fatalf("autoscaling flag must be untouched")
	}
	// replicas still owned by Bifrost (ADR-0007 unchanged).
	if *rc.Spec.WorkerGroupSpecs[0].Replicas != 2 {
		t.Fatalf("replicas = %v", rc.Spec.WorkerGroupSpecs[0].Replicas)
	}
}

// kuberay.rs: elastic_assignment_forces_autoscaling_and_annotation
func TestElasticAssignmentForcesAutoscalingAndAnnotation(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	q := &QueueAssignment{QueueName: "proj-a", Elastic: true}
	// autoscaling=false passed, but elastic mode requires the in-tree
	// autoscaler — it must win.
	rc, err := RayClusterFor("demo", spec, false, 1, q)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if rc.Labels[QueueLabel] != "proj-a" {
		t.Fatalf("queue label = %q", rc.Labels[QueueLabel])
	}
	if rc.Annotations[ElasticJobAnnotation] != "true" {
		t.Fatalf("elastic annotation = %q", rc.Annotations[ElasticJobAnnotation])
	}
	if *rc.Spec.EnableInTreeAutoscaling != true {
		t.Fatalf("autoscaling must be forced on")
	}
	// ADR-0007 unaffected: with autoscaling on, Bifrost never writes
	// replicas.
	if rc.Spec.WorkerGroupSpecs[0].Replicas != nil {
		t.Fatalf("replicas must be nil, got %v", *rc.Spec.WorkerGroupSpecs[0].Replicas)
	}
	if *rc.Spec.WorkerGroupSpecs[0].MaxReplicas != 4 {
		t.Fatalf("maxReplicas = %d", *rc.Spec.WorkerGroupSpecs[0].MaxReplicas)
	}
}

// kuberay.rs: pod_templates_carry_the_cluster_id_label
func TestPodTemplatesCarryTheClusterIDLabel(t *testing.T) {
	spec := testSpec(t, wg("cpu", 0, 4, 2))
	rc, err := RayClusterFor("demo", spec, false, 1, nil)
	if err != nil {
		t.Fatalf("RayClusterFor: %v", err)
	}
	if rc.Spec.HeadGroupSpec.Template.Labels[ClusterIDLabel] != "demo" {
		t.Fatalf("head pod template label = %q", rc.Spec.HeadGroupSpec.Template.Labels[ClusterIDLabel])
	}
	if rc.Spec.WorkerGroupSpecs[0].Template.Labels[ClusterIDLabel] != "demo" {
		t.Fatalf("worker pod template label = %q", rc.Spec.WorkerGroupSpecs[0].Template.Labels[ClusterIDLabel])
	}
	// The generation annotation still rides the same metadata.
	if rc.Spec.HeadGroupSpec.Template.Annotations[GenerationAnnotation] != "1" {
		t.Fatalf("generation annotation = %q", rc.Spec.HeadGroupSpec.Template.Annotations[GenerationAnnotation])
	}

	svcSpec := testServiceSpec(core.UpgradeStrategyCanary)
	rs, err := RayServiceFor("svc", svcSpec)
	if err != nil {
		t.Fatalf("RayServiceFor: %v", err)
	}
	if rs.Spec.RayClusterSpec.HeadGroupSpec.Template.Labels[ClusterIDLabel] != "svc" {
		t.Fatalf("service head pod template label wrong")
	}
	if rs.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.Labels[ClusterIDLabel] != "svc" {
		t.Fatalf("service worker pod template label wrong")
	}
}

func testServiceSpec(upgrade core.UpgradeStrategy) *core.ServiceSpec {
	return &core.ServiceSpec{
		Name: "svc", Project: "p", RayVersion: "2.57.0", Image: "rayproject/ray:2.57.0",
		ServeConfigV2: "applications:\n  - name: app\n",
		HeadCpu:       "1", HeadMemory: "2Gi",
		WorkerReplicas: 2, WorkerCpu: "1", WorkerMemory: "2Gi",
		Upgrade: upgrade,
	}
}

// kuberay.rs: rayservice_canary_vs_inplace_upgrade_strategy
func TestRayServiceCanaryVsInplaceUpgradeStrategy(t *testing.T) {
	canary, err := RayServiceFor("svc", testServiceSpec(core.UpgradeStrategyCanary))
	if err != nil {
		t.Fatalf("RayServiceFor: %v", err)
	}
	if canary.Kind != "RayService" {
		t.Fatalf("kind = %q", canary.Kind)
	}
	if *canary.Spec.UpgradeStrategy.Type != rayv1.RayServiceNewCluster {
		t.Fatalf("upgrade type = %q", *canary.Spec.UpgradeStrategy.Type)
	}
	if canary.Spec.ServeConfigV2 == "" {
		t.Fatalf("serveConfigV2 must be carried verbatim")
	}
	if *canary.Spec.RayClusterSpec.WorkerGroupSpecs[0].Replicas != 2 {
		t.Fatalf("service worker replicas = %v", canary.Spec.RayClusterSpec.WorkerGroupSpecs[0].Replicas)
	}

	inplace, err := RayServiceFor("svc", testServiceSpec(core.UpgradeStrategyInPlace))
	if err != nil {
		t.Fatalf("RayServiceFor: %v", err)
	}
	if *inplace.Spec.UpgradeStrategy.Type != rayv1.RayServiceUpgradeNone {
		t.Fatalf("upgrade type = %q", *inplace.Spec.UpgradeStrategy.Type)
	}
}

// kuberay.rs: rayservice_status_mapping
func TestRayServiceStatusMapping(t *testing.T) {
	cases := []struct {
		status string
		want   core.ClusterState
	}{
		{"Running", core.ClusterStateRunning},
		{"Restarting", core.ClusterStateUpdating},
		{"", core.ClusterStateProvisioning},
		{"Preparing", core.ClusterStateProvisioning}, // unrecognized: still coming up
	}
	for _, c := range cases {
		got := ServiceStatusToState(rayv1.RayServiceStatuses{ServiceStatus: rayv1.ServiceStatus(c.status)}) //nolint:staticcheck // SA1019: exercising the ported (deprecated) field, see ServiceStatusToState's doc comment
		if got != c.want {
			t.Errorf("ServiceStatusToState(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

func tenantSelectorJSON() map[string]any {
	return map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": ClusterIDLabel, "operator": "Exists"},
		},
	}
}

// kuberay.rs: default_deny_policy_shape
func TestDefaultDenyPolicyShape(t *testing.T) {
	p := DefaultDenyNetworkPolicy()
	if p.APIVersion != "networking.k8s.io/v1" || p.Kind != "NetworkPolicy" {
		t.Fatalf("apiVersion/kind = %q/%q", p.APIVersion, p.Kind)
	}
	if p.Name != DefaultDenyPolicyName {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Labels[ManagedByLabel] != "bifrost" {
		t.Fatalf("managed-by label = %q", p.Labels[ManagedByLabel])
	}
	m := marshal(t, p)
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec = %#v", m["spec"])
	}
	if got := spec["podSelector"]; !jsonEqual(t, got, tenantSelectorJSON()) {
		t.Fatalf("podSelector = %#v", got)
	}
	if jsonEqual(t, spec["podSelector"], map[string]any{}) {
		t.Fatalf("the deny must never be namespace-wide")
	}
	if _, ok := spec["ingress"]; ok {
		t.Fatalf("ingress must be absent")
	}
	if _, ok := spec["egress"]; ok {
		t.Fatalf("egress must be absent")
	}
	if IsDefaultDeny(p) {
		t.Fatalf("the scoped deny is NOT a namespace-wide default-deny")
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// mp asserts v is a JSON object (decoded as map[string]any), failing the
// test otherwise. Centralizes the checked type assertion so callers below
// can navigate a marshaled manifest's nested shape tersely.
func mp(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON object, got %#v", v)
	}
	return m
}

// arr asserts v is a JSON array (decoded as []any), failing the test
// otherwise.
func arr(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a JSON array, got %#v", v)
	}
	return a
}

// kuberay.rs: tenant_allow_policy_shape
func TestTenantAllowPolicyShape(t *testing.T) {
	p := TenantAllowNetworkPolicy()
	if p.Name != TenantAllowPolicyName {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Labels[ManagedByLabel] != "bifrost" {
		t.Fatalf("managed-by label = %q", p.Labels[ManagedByLabel])
	}
	if IsDefaultDeny(p) {
		t.Fatalf("an allow policy is not a default-deny")
	}
	if len(p.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want 2", len(p.Spec.Ingress))
	}
	m := marshal(t, p)
	ingress := arr(t, mp(t, m["spec"])["ingress"])
	ingress0 := mp(t, ingress[0])
	ingress1 := mp(t, ingress[1])

	want0From := []any{
		map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{ControlPlanePodLabel: "true"}}},
		map[string]any{
			"namespaceSelector": map[string]any{"matchLabels": map[string]any{ControlPlaneNamespaceLabel: "true"}},
			"podSelector":       map[string]any{"matchLabels": map[string]any{ControlPlanePodLabel: "true"}},
		},
	}
	if !jsonEqual(t, ingress0["from"], want0From) {
		t.Fatalf("ingress[0].from = %#v", ingress0["from"])
	}
	want0Ports := []any{
		map[string]any{"protocol": "TCP", "port": float64(8265)},
		map[string]any{"protocol": "TCP", "port": float64(10001)},
	}
	if !jsonEqual(t, ingress0["ports"], want0Ports) {
		t.Fatalf("ingress[0].ports = %#v", ingress0["ports"])
	}

	want1From := []any{
		map[string]any{
			"namespaceSelector": map[string]any{},
			"podSelector":       map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "kuberay-operator"}},
		},
	}
	if !jsonEqual(t, ingress1["from"], want1From) {
		t.Fatalf("ingress[1].from = %#v", ingress1["from"])
	}
	want1Ports := []any{
		map[string]any{"protocol": "TCP", "port": float64(8265)},
		map[string]any{"protocol": "TCP", "port": float64(52365)},
		map[string]any{"protocol": "TCP", "port": float64(8000)},
	}
	if !jsonEqual(t, ingress1["ports"], want1Ports) {
		t.Fatalf("ingress[1].ports = %#v", ingress1["ports"])
	}

	egress := arr(t, mp(t, m["spec"])["egress"])
	if len(egress) != 1 {
		t.Fatalf("egress rules = %d, want 1", len(egress))
	}
	egress0 := mp(t, egress[0])
	wantTo := []any{
		map[string]any{
			"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
			"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}},
		},
	}
	if !jsonEqual(t, egress0["to"], wantTo) {
		t.Fatalf("egress[0].to = %#v", egress0["to"])
	}
	wantEgressPorts := []any{
		map[string]any{"protocol": "UDP", "port": float64(53)},
		map[string]any{"protocol": "TCP", "port": float64(53)},
	}
	if !jsonEqual(t, egress0["ports"], wantEgressPorts) {
		t.Fatalf("egress[0].ports = %#v", egress0["ports"])
	}
}

// kuberay.rs: cluster_allow_policy_is_scoped_to_one_cluster
func TestClusterAllowPolicyIsScopedToOneCluster(t *testing.T) {
	p := ClusterAllowNetworkPolicy("tenant-a", nil)
	if p.APIVersion != "networking.k8s.io/v1" || p.Kind != "NetworkPolicy" {
		t.Fatalf("apiVersion/kind = %q/%q", p.APIVersion, p.Kind)
	}
	if p.Name != "bifrost-cluster-tenant-a" {
		t.Fatalf("name = %q", p.Name)
	}
	if p.Labels[ManagedByLabel] != "bifrost" || p.Labels[ClusterIDLabel] != "tenant-a" {
		t.Fatalf("labels = %#v", p.Labels)
	}
	own := map[string]any{"matchLabels": map[string]any{ClusterIDLabel: "tenant-a"}}
	m := marshal(t, p)
	spec := mp(t, m["spec"])
	if !jsonEqual(t, spec["podSelector"], own) {
		t.Fatalf("podSelector = %#v", spec["podSelector"])
	}
	ingress := arr(t, spec["ingress"])
	if len(ingress) != 1 {
		t.Fatalf("ingress rules = %d, want 1", len(ingress))
	}
	ingress0 := mp(t, ingress[0])
	if !jsonEqual(t, ingress0["from"], []any{map[string]any{"podSelector": own}}) {
		t.Fatalf("ingress[0].from = %#v", ingress0["from"])
	}
	if _, ok := ingress0["ports"]; ok {
		t.Fatalf("intra-cluster ingress must allow all ports (no ports field)")
	}
	egress := arr(t, spec["egress"])
	if len(egress) != 1 {
		t.Fatalf("egress rules = %d, want 1", len(egress))
	}
	egress0 := mp(t, egress[0])
	if !jsonEqual(t, egress0["to"], []any{map[string]any{"podSelector": own}}) {
		t.Fatalf("egress[0].to = %#v", egress0["to"])
	}
	if _, ok := egress0["ports"]; ok {
		t.Fatalf("intra-cluster egress must allow all ports (no ports field)")
	}
	if IsDefaultDeny(p) {
		t.Fatalf("must not be a default-deny")
	}
}

// kuberay.rs: per_owner_rule_pins_ray_client_to_owner_notebook
func TestPerOwnerRulePinsRayClientToOwnerNotebook(t *testing.T) {
	owner := "bob"
	p := ClusterAllowNetworkPolicy("sess-bob", &owner)
	if len(p.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want 2 (intra-cluster + per-owner)", len(p.Spec.Ingress))
	}
	m := marshal(t, p)
	ingress := arr(t, mp(t, m["spec"])["ingress"])
	rule0 := mp(t, ingress[0])
	if !jsonEqual(t, rule0["from"], []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{ClusterIDLabel: "sess-bob"}}}}) {
		t.Fatalf("ingress[0].from = %#v", rule0["from"])
	}
	if _, ok := rule0["ports"]; ok {
		t.Fatalf("intra-cluster rule must allow all ports")
	}
	rule1 := mp(t, ingress[1])
	wantPorts := []any{
		map[string]any{"protocol": "TCP", "port": float64(10001)},
		map[string]any{"protocol": "TCP", "port": float64(8265)},
	}
	if !jsonEqual(t, rule1["ports"], wantPorts) {
		t.Fatalf("ingress[1].ports = %#v", rule1["ports"])
	}
	peers := arr(t, rule1["from"])
	if len(peers) != 1 {
		t.Fatalf("owner rule must have one ANDed peer, not two ORed ones, got %d", len(peers))
	}
	peer := mp(t, peers[0])
	ns := mp(t, mp(t, peer["namespaceSelector"])["matchLabels"])
	if ns["kubernetes.io/metadata.name"] != NotebookNamespace {
		t.Fatalf("namespace selector = %#v", ns)
	}
	pod := mp(t, mp(t, peer["podSelector"])["matchLabels"])
	if pod[OwnerLabel] != "bob" {
		t.Fatalf("owner pod selector = %#v", pod)
	}
}

// kuberay.rs: non_owner_notebook_is_not_a_peer
func TestNonOwnerNotebookIsNotAPeer(t *testing.T) {
	owner := "bob"
	p := ClusterAllowNetworkPolicy("sess-bob", &owner)
	for _, rule := range p.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector == nil {
				continue
			}
			if v, ok := peer.PodSelector.MatchLabels[OwnerLabel]; ok && v == "alice" {
				t.Fatalf("alice must never be an allowed peer")
			}
		}
	}
	none := ClusterAllowNetworkPolicy("sess-x", nil)
	if len(none.Spec.Ingress) != 1 {
		t.Fatalf("ownerless clusters get no owner rule at all, got %d ingress rules", len(none.Spec.Ingress))
	}
}

// kuberay.rs: tenant_clusters_stay_isolated_from_each_other
func TestTenantClustersStayIsolatedFromEachOther(t *testing.T) {
	a := ClusterAllowNetworkPolicy("tenant-a", nil)
	for _, rule := range a.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector == nil {
				t.Fatalf("every peer must be pod-selector-scoped")
			}
			if peer.PodSelector.MatchLabels[ClusterIDLabel] != "tenant-a" {
				t.Fatalf("tenant-b must not be an allowed peer: %#v", peer.PodSelector.MatchLabels)
			}
		}
	}
	shared := TenantAllowNetworkPolicy()
	for _, rule := range shared.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.PodSelector == nil {
				t.Fatalf("every peer must be pod-label-scoped: %#v", peer)
			}
			if len(peer.PodSelector.MatchLabels) == 0 {
				t.Fatalf("no allow rule may admit arbitrary same-namespace pods: %#v", peer)
			}
		}
	}
}

// kuberay.rs: no_mobula_policy_selects_namespace_wide (renamed: bifrost)
func TestNoBifrostPolicySelectsNamespaceWide(t *testing.T) {
	policies := []*networkingv1.NetworkPolicy{
		DefaultDenyNetworkPolicy(),
		TenantAllowNetworkPolicy(),
		ClusterAllowNetworkPolicy("demo", nil),
	}
	for _, p := range policies {
		sel := p.Spec.PodSelector
		if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
			t.Fatalf("%s must not select the whole namespace", p.Name)
		}
	}
}

// kuberay.rs: control_plane_reaches_the_head_dashboard
func TestControlPlaneReachesTheHeadDashboard(t *testing.T) {
	p := TenantAllowNetworkPolicy()
	rule := p.Spec.Ingress[0]
	sameNSPeer := rule.From[0]
	if sameNSPeer.PodSelector.MatchLabels[ControlPlanePodLabel] != "true" {
		t.Fatalf("control-plane peer label wrong: %#v", sameNSPeer.PodSelector.MatchLabels)
	}
	found := false
	for _, port := range rule.Ports {
		if port.Port != nil && port.Port.IntValue() == 8265 {
			found = true
		}
	}
	if !found {
		t.Fatalf("dashboard/job port 8265 must be allowed from the control plane")
	}
}

// kuberay.rs: pss_labels_enforce_baseline_warn_audit_restricted
func TestPSSLabelsEnforceBaselineWarnAuditRestricted(t *testing.T) {
	labels := NamespacePSSLabels()
	if labels[pssEnforceLabel] != "baseline" {
		t.Fatalf("enforce = %q", labels[pssEnforceLabel])
	}
	if labels[pssWarnLabel] != "restricted" {
		t.Fatalf("warn = %q", labels[pssWarnLabel])
	}
	if labels[pssAuditLabel] != "restricted" {
		t.Fatalf("audit = %q", labels[pssAuditLabel])
	}
}

// kuberay.rs: is_default_deny_recognizes_foreign_deny_all
func TestIsDefaultDenyRecognizesForeignDenyAll(t *testing.T) {
	foreign := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "org-deny-all"},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeIngress},
		},
	}
	if !IsDefaultDeny(foreign) {
		t.Fatalf("an admin-managed deny-all must be detected")
	}

	explicitEmpty := &networkingv1.NetworkPolicy{
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		},
	}
	if !IsDefaultDeny(explicitEmpty) {
		t.Fatalf("explicit empty rule arrays still count as deny-all")
	}

	withRules := &networkingv1.NetworkPolicy{
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
		},
	}
	if IsDefaultDeny(withRules) {
		t.Fatalf("a policy with allow rules is not a default-deny")
	}

	selective := &networkingv1.NetworkPolicy{
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	if IsDefaultDeny(selective) {
		t.Fatalf("a policy that selects specific pods is not a default-deny")
	}

	ingressOnly := &networkingv1.NetworkPolicy{
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
	}
	if IsDefaultDeny(ingressOnly) {
		t.Fatalf("covering only one direction is not a default-deny")
	}
	if IsDefaultDeny(&networkingv1.NetworkPolicy{}) {
		t.Fatalf("a policy with no policyTypes is not a default-deny")
	}
	if IsDefaultDeny(nil) {
		t.Fatalf("nil is not a default-deny")
	}
}
