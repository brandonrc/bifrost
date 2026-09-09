package provision

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/brandonrc/bifrost/internal/core"
)

// testScheduling is the atep-dev shape: a tainted user node group that
// tenant pods must both select and tolerate.
func testScheduling() Scheduling {
	return Scheduling{
		NodeSelector: map[string]string{"hub.jupyter.org/node-purpose": "user"},
		Tolerations: []corev1.Toleration{{
			Key: "hub.jupyter.org/dedicated", Operator: corev1.TolerationOpEqual,
			Value: "user", Effect: corev1.TaintEffectNoSchedule,
		}},
	}
}

func assertScheduled(t *testing.T, what string, spec corev1.PodSpec, want Scheduling) {
	t.Helper()
	if !reflect.DeepEqual(spec.NodeSelector, want.NodeSelector) {
		t.Errorf("%s: nodeSelector = %v, want %v", what, spec.NodeSelector, want.NodeSelector)
	}
	if !reflect.DeepEqual(spec.Tolerations, want.Tolerations) {
		t.Errorf("%s: tolerations = %v, want %v", what, spec.Tolerations, want.Tolerations)
	}
}

func TestZeroSchedulingIsByteIdentical(t *testing.T) {
	spec := testSpec(t, wg("w", 1, 2, 1))
	plain, err := RayClusterFor("c1", spec, false, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := RayClusterForScheduled("c1", spec, false, 1, nil, Scheduling{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(scheduled)
	if string(a) != string(b) {
		t.Fatalf("zero Scheduling changed the manifest:\n%s\n%s", a, b)
	}
	if plain.Spec.HeadGroupSpec.Template.Spec.NodeSelector != nil || plain.Spec.HeadGroupSpec.Template.Spec.Tolerations != nil {
		t.Fatalf("unscheduled manifest must carry no nodeSelector/tolerations")
	}
}

func TestRayClusterScheduledStampsHeadAndEveryWorkerGroup(t *testing.T) {
	want := testScheduling()
	rc, err := RayClusterForScheduled("c1", testSpec(t, wg("cpu", 1, 2, 1), wg("gpu", 0, 1, 0)), false, 1, nil, want)
	if err != nil {
		t.Fatal(err)
	}
	assertScheduled(t, "head", rc.Spec.HeadGroupSpec.Template.Spec, want)
	if len(rc.Spec.WorkerGroupSpecs) != 2 {
		t.Fatalf("want 2 worker groups, got %d", len(rc.Spec.WorkerGroupSpecs))
	}
	for _, ws := range rc.Spec.WorkerGroupSpecs {
		assertScheduled(t, "worker "+ws.GroupName, ws.Template.Spec, want)
	}
}

func TestRayServiceScheduledStampsServeHeadAndWorkers(t *testing.T) {
	want := testScheduling()
	rs, err := RayServiceForScheduled("svc", testServiceSpec(core.UpgradeStrategyCanary), 1, nil, want)
	if err != nil {
		t.Fatal(err)
	}
	assertScheduled(t, "serve head", rs.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec, want)
	assertScheduled(t, "serve worker", rs.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.Spec, want)
}

func TestRayJobScheduledStampsClusterAndSubmitter(t *testing.T) {
	want := testScheduling()
	rj, err := RayJobForScheduled("j1", testJobSpec(wg("w", 1, 1, 1)), 1, nil, want)
	if err != nil {
		t.Fatal(err)
	}
	assertScheduled(t, "job head", rj.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec, want)
	assertScheduled(t, "job worker", rj.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.Spec, want)
	if rj.Spec.SubmitterPodTemplate == nil {
		t.Fatal("submitter template missing")
	}
	assertScheduled(t, "submitter", rj.Spec.SubmitterPodTemplate.Spec, want)
}

func TestSchedulingDoesNotEnterTheOwnedFingerprint(t *testing.T) {
	spec := testSpec(t, wg("w", 1, 2, 1))
	rc, err := RayClusterForScheduled("c1", spec, false, 1, nil, testScheduling())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FingerprintFromRayCluster(&rc.Spec)
	if !ok {
		t.Fatal("fingerprint not recoverable from the scheduled manifest")
	}
	if want := OwnedSpecFingerprint(spec); got != want {
		t.Fatalf("scheduling changed the fingerprint: %s != %s", got, want)
	}
}

func TestParseNodeSelector(t *testing.T) {
	got, err := ParseNodeSelector(" a=b, c = d ,,")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"a": "b", "c": "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got, err := ParseNodeSelector(""); err != nil || got != nil {
		t.Fatalf("empty: got %v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "=v", "a=b,broken"} {
		if _, err := ParseNodeSelector(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestParseTolerations(t *testing.T) {
	got, err := ParseTolerations(`[{"key":"hub.jupyter.org/dedicated","operator":"Equal","value":"user","effect":"NoSchedule"},{"operator":"Exists"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "hub.jupyter.org/dedicated" || got[0].Effect != corev1.TaintEffectNoSchedule || got[1].Operator != corev1.TolerationOpExists {
		t.Fatalf("unexpected parse: %+v", got)
	}
	if got, err := ParseTolerations("  "); err != nil || got != nil {
		t.Fatalf("blank: got %v, %v", got, err)
	}
	for name, bad := range map[string]string{
		"not json":         `{"key":"a"}`,
		"unknown operator": `[{"key":"a","operator":"Like"}]`,
		"unknown effect":   `[{"key":"a","effect":"Never"}]`,
		"empty key equal":  `[{"operator":"Equal","value":"x"}]`,
	} {
		if _, err := ParseTolerations(bad); err == nil {
			t.Errorf("%s: want error for %s", name, bad)
		}
	}
}

func TestSchedulingString(t *testing.T) {
	s := testScheduling()
	if got, want := s.String(), "hub.jupyter.org/node-purpose=user tolerate:hub.jupyter.org/dedicated=user:NoSchedule"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if (Scheduling{}).String() != "" {
		t.Fatal("zero Scheduling should render empty")
	}
}
