package provision

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	"github.com/brandonrc/bifrost/internal/core"
)

func testJobSpec(groups ...core.WorkerGroup) *core.RayJobSpec {
	owner := "alice"
	return &core.RayJobSpec{
		Project:      "p",
		Entrypoint:   "python -c 1",
		Image:        "rayproject/ray:2.57.0",
		RayVersion:   "2.57.0",
		HeadCpu:      "1",
		HeadMemory:   "2Gi",
		WorkerGroups: groups,
		Owner:        &owner,
	}
}

func TestRayJobManifestShapeAndLabels(t *testing.T) {
	rj, err := RayJobFor("job-1", testJobSpec(wg("cpu", 1, 2, 1)), 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rj.APIVersion != APIVersion || rj.Kind != JobKind || rj.Name != "job-1" {
		t.Fatalf("type/name = %s %s %s", rj.APIVersion, rj.Kind, rj.Name)
	}
	for k, want := range map[string]string{ManagedByLabel: FieldManager, ClusterIDLabel: "job-1", OwnerLabel: "alice"} {
		if got := rj.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if rj.Annotations[GenerationAnnotation] != "3" {
		t.Errorf("generation annotation = %q", rj.Annotations[GenerationAnnotation])
	}
	if !rj.Spec.ShutdownAfterJobFinishes {
		t.Error("ShutdownAfterJobFinishes must be true: the cluster exists for the job only")
	}
	if rj.Spec.TTLSecondsAfterFinished != int32(core.DefaultRayJobTtlSecondsAfterFinished) {
		t.Errorf("ttl = %d, want default %d", rj.Spec.TTLSecondsAfterFinished, core.DefaultRayJobTtlSecondsAfterFinished)
	}
	if rj.Spec.SubmissionMode != rayv1.K8sJobMode {
		t.Errorf("submission mode = %q", rj.Spec.SubmissionMode)
	}
	if rj.Spec.Entrypoint != "python -c 1" {
		t.Errorf("entrypoint = %q", rj.Spec.Entrypoint)
	}
	if rj.Spec.RayClusterSpec == nil {
		t.Fatal("RayClusterSpec is nil")
	}
	head := rj.Spec.RayClusterSpec.HeadGroupSpec.Template
	if head.Labels[ClusterIDLabel] != "job-1" || head.Labels[OwnerLabel] != "alice" {
		t.Errorf("head pod labels = %v", head.Labels)
	}
	if len(rj.Spec.RayClusterSpec.WorkerGroupSpecs) != 1 || rj.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.Labels[ClusterIDLabel] != "job-1" {
		t.Errorf("worker group labels = %+v", rj.Spec.RayClusterSpec.WorkerGroupSpecs)
	}
	if head.Spec.Containers[0].LivenessProbe == nil {
		t.Error("head probe missing: the job cluster must not inherit KubeRay's wget probes")
	}
}

func TestRayJobSubmitterIsATenantPeer(t *testing.T) {
	rj, err := RayJobFor("job-1", testJobSpec(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := rj.Spec.SubmitterPodTemplate
	if sub == nil {
		t.Fatal("submitter template missing")
	}
	if sub.Labels[ClusterIDLabel] != "job-1" || sub.Labels[OwnerLabel] != "alice" {
		t.Errorf("submitter labels = %v: without the cluster-id label the default-deny posture cuts the submitter off from the head", sub.Labels)
	}
	if len(sub.Spec.Containers) != 1 || sub.Spec.Containers[0].Name != SubmitterContainerName || sub.Spec.Containers[0].Image != "rayproject/ray:2.57.0" {
		t.Errorf("submitter container = %+v", sub.Spec.Containers)
	}
}

func TestRayJobTTLAndRuntimeEnvPassThrough(t *testing.T) {
	spec := testJobSpec()
	ttl := uint32(5)
	spec.TtlSecondsAfterFinished = &ttl
	spec.RuntimeEnvYaml = "pip: [requests]\n"
	rj, err := RayJobFor("job-1", spec, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rj.Spec.TTLSecondsAfterFinished != 5 || rj.Spec.RuntimeEnvYAML != "pip: [requests]\n" {
		t.Errorf("ttl=%d runtime_env=%q", rj.Spec.TTLSecondsAfterFinished, rj.Spec.RuntimeEnvYAML)
	}
}

func TestRayJobQueueAssignmentStampsQueueLabel(t *testing.T) {
	rj, err := RayJobFor("job-1", testJobSpec(), 1, &QueueAssignment{QueueName: "team-a", Elastic: true})
	if err != nil {
		t.Fatal(err)
	}
	if rj.Labels[QueueLabel] != "team-a" || rj.Annotations[ElasticJobAnnotation] != "true" {
		t.Errorf("labels=%v annotations=%v", rj.Labels, rj.Annotations)
	}
	if rj.Spec.RayClusterSpec.EnableInTreeAutoscaling == nil || !*rj.Spec.RayClusterSpec.EnableInTreeAutoscaling {
		t.Error("elastic queue must force the in-tree autoscaler")
	}
}

func TestRayJobForRejectsBadInput(t *testing.T) {
	spec := testJobSpec()
	spec.Entrypoint = ""
	if _, err := RayJobFor("job-1", spec, 1, nil); err == nil {
		t.Error("empty entrypoint accepted")
	}
	spec = testJobSpec()
	spec.HeadCpu = "lots"
	if _, err := RayJobFor("job-1", spec, 1, nil); err == nil {
		t.Error("unparseable head cpu accepted")
	}
}

func TestRayJobForNeverPopulatesStatus(t *testing.T) {
	rj, err := RayJobFor("job-1", testJobSpec(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ZeroStatus(rj)
	st, ok := marshal(t, rj)["status"].(map[string]any)
	if !ok {
		t.Fatal("status is not an object")
	}
	for _, k := range []string{"jobStatus", "jobDeploymentStatus", "rayClusterName", "dashboardURL", "startTime", "endTime"} {
		if v, ok := st[k]; ok && v != "" && v != nil {
			t.Errorf("status.%s = %v, want unset", k, v)
		}
	}
}

func TestJobStatusToState(t *testing.T) {
	cases := []struct {
		job  rayv1.JobStatus
		dep  rayv1.JobDeploymentStatus
		want core.RayJobState
	}{
		{rayv1.JobStatusRunning, rayv1.JobDeploymentStatusRunning, core.RayJobStateRunning},
		{rayv1.JobStatusSucceeded, rayv1.JobDeploymentStatusComplete, core.RayJobStateSucceeded},
		{rayv1.JobStatusFailed, rayv1.JobDeploymentStatusFailed, core.RayJobStateFailed},
		{rayv1.JobStatusStopped, rayv1.JobDeploymentStatusComplete, core.RayJobStateStopped},
		{rayv1.JobStatusNew, rayv1.JobDeploymentStatusInitializing, core.RayJobStatePending},
		{rayv1.JobStatusNew, rayv1.JobDeploymentStatusFailed, core.RayJobStateFailed},
		{rayv1.JobStatusNew, rayv1.JobDeploymentStatusValidationFailed, core.RayJobStateFailed},
		{rayv1.JobStatusNew, rayv1.JobDeploymentStatusNew, core.RayJobStatePending},
	}
	for _, c := range cases {
		got := JobStatusToState(rayv1.RayJobStatus{JobStatus: c.job, JobDeploymentStatus: c.dep})
		if got != c.want {
			t.Errorf("(%q,%q) = %s, want %s", c.job, c.dep, got, c.want)
		}
	}
}

func TestObservedJobFromRayJob(t *testing.T) {
	start := metav1.NewTime(time.Unix(1_700_000_000, 0))
	end := metav1.NewTime(time.Unix(1_700_000_090, 0))
	rj := &rayv1.RayJob{
		ObjectMeta: metav1.ObjectMeta{Name: "job-1"},
		Status: rayv1.RayJobStatus{
			JobStatus:           rayv1.JobStatusSucceeded,
			JobDeploymentStatus: rayv1.JobDeploymentStatusComplete,
			RayClusterName:      "job-1-raycluster-abcde",
			DashboardURL:        "job-1-raycluster-abcde-head-svc.bifrost.svc.cluster.local:8265",
			Message:             "done",
			StartTime:           &start,
			EndTime:             &end,
		},
	}
	obs := ObservedJobFromRayJob(rj)
	if obs.ID != "job-1" || obs.JobStatus != "SUCCEEDED" || obs.DeploymentStatus != "Complete" {
		t.Errorf("obs = %+v", obs)
	}
	if obs.ClusterName == nil || *obs.ClusterName != "job-1-raycluster-abcde" {
		t.Errorf("cluster name = %v", obs.ClusterName)
	}
	if obs.DashboardURL == nil || *obs.DashboardURL != "http://job-1-raycluster-abcde-head-svc.bifrost.svc.cluster.local:8265" {
		t.Errorf("dashboard url = %v: KubeRay writes it scheme-less, the gateway needs http://", obs.DashboardURL)
	}
	if obs.StartTime == nil || *obs.StartTime != 1_700_000_000 || obs.EndTime == nil || *obs.EndTime != 1_700_000_090 {
		t.Errorf("times = %v %v", obs.StartTime, obs.EndTime)
	}
	if obs.Message == nil || *obs.Message != "done" {
		t.Errorf("message = %v", obs.Message)
	}

	empty := ObservedJobFromRayJob(&rayv1.RayJob{ObjectMeta: metav1.ObjectMeta{Name: "job-2"}})
	if empty.ClusterName != nil || empty.DashboardURL != nil || empty.Message != nil || empty.StartTime != nil || empty.EndTime != nil {
		t.Errorf("fresh RayJob must observe as all-nil optionals: %+v", empty)
	}
}

func TestJobDashboardBaseURL(t *testing.T) {
	if got := JobDashboardBaseURL("h:8265"); got != "http://h:8265" {
		t.Errorf("got %q", got)
	}
	if got := JobDashboardBaseURL("https://h:8265"); got != "https://h:8265" {
		t.Errorf("got %q", got)
	}
	if got := JobDashboardBaseURL(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestRayVersionFromImage(t *testing.T) {
	cases := map[string]struct {
		v  string
		ok bool
	}{
		"rayproject/ray:2.56.0":            {"2.56.0", true},
		"rayproject/ray:2.56.0-py311-gpu":  {"2.56.0", true},
		"registry:5000/team/ray:2.9.0":     {"2.9.0", true},
		"rayproject/ray":                   {"", false},
		"registry:5000/team/ray":           {"", false},
		"rayproject/ray:latest":            {"", false},
		"rayproject/ray@sha256:abcdef0123": {"", false},
	}
	for image, want := range cases {
		v, ok := RayVersionFromImage(image)
		if v != want.v || ok != want.ok {
			t.Errorf("%s = (%q,%v), want (%q,%v)", image, v, ok, want.v, want.ok)
		}
	}
}

func TestClusterSpecForJobCarriesTheJobShape(t *testing.T) {
	spec := testJobSpec(wg("cpu", 1, 2, 1))
	cs := ClusterSpecForJob("job-1", spec)
	if cs.Name != "job-1" || cs.Project != "p" || cs.Engine != core.EngineRay || cs.Image != spec.Image ||
		cs.HeadCpu != "1" || cs.HeadMemory != "2Gi" || len(cs.WorkerGroups) != 1 || cs.Owner == nil || *cs.Owner != "alice" {
		t.Errorf("cs = %+v", cs)
	}
}

func TestJobDeploymentIsTerminal(t *testing.T) {
	for status, want := range map[string]bool{"Complete": true, "Failed": true, "ValidationFailed": true, "Running": false, "Initializing": false, "": false} {
		if got := JobDeploymentIsTerminal(status); got != want {
			t.Errorf("%q = %v, want %v", status, got, want)
		}
	}
}
