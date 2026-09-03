// Ephemeral RayJob reconcile engine (requirement 5).
//
// Same posture as the cluster engine in reconcile.go — observation-first,
// level-triggered, idempotency-keyed actuation through the outbox — but
// KubeRay's RayJob controller owns most of the lifecycle: it provisions
// the job's cluster, submits the entrypoint and tears the cluster down
// when the job finishes. Bifrost therefore applies intent once, then
// observes: it records what KubeRay reports, keeps the gateway routing
// table in step with the job's cluster (registered while the job runs,
// deregistered when it ends), writes the finished job into the persistent
// job history (the "job-history side-write" SPEC row 14 owed), and tears
// down + tombstones on a user delete.
package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// JobGeneration is the spec generation stamped on every RayJob: a job spec
// is submitted once and never edited (StoredRayJob has no generation), so
// the outbox key `job/<id>/<JobGeneration>` is stable for the job's life.
const JobGeneration uint64 = 1

// jobIntentKey is the outbox idempotency key for actuating a job.
func jobIntentKey(id core.ClusterId) string {
	return "job/" + id.String() + "/1"
}

// jobParamsFingerprint is the outbox fingerprint of a job's actuation
// parameters — the whole spec, like paramsFingerprint for clusters.
func jobParamsFingerprint(spec *core.RayJobSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}

// JobReconciler is the RayJob reconcile engine.
type JobReconciler struct {
	store Store
	jobs  provision.JobProvisioner
	// registrar + gatewayHostname make a running job's cluster routable
	// through the gateway; either nil disables registration.
	registrar               Registrar
	gatewayHostname         func(core.ClusterId) string
	terminatedRetentionSecs uint64
}

// NewJobReconciler returns a JobReconciler with no gateway registration and
// the default tombstone retention.
func NewJobReconciler(store Store, jobs provision.JobProvisioner) *JobReconciler {
	return &JobReconciler{store: store, jobs: jobs, terminatedRetentionSecs: TerminatedRetentionSecs}
}

// WithRegistrar enables gateway registration of running jobs' clusters
// under hostname(id). Returns r for chaining.
func (r *JobReconciler) WithRegistrar(registrar Registrar, hostname func(core.ClusterId) string) *JobReconciler {
	r.registrar = registrar
	r.gatewayHostname = hostname
	return r
}

// WithTerminatedRetention overrides the tombstone retention window.
func (r *JobReconciler) WithTerminatedRetention(secs uint64) *JobReconciler {
	r.terminatedRetentionSecs = secs
	return r
}

// ReconcileAll reconciles every known job once at the current time.
func (r *JobReconciler) ReconcileAll(ctx context.Context) []ReconcileResult {
	return r.ReconcileAllAt(ctx, NowUnix())
}

// ReconcileAllAt reconciles every known job once at time now (unix secs).
// Errors on individual jobs are collected, never fatal.
func (r *JobReconciler) ReconcileAllAt(ctx context.Context, now uint64) []ReconcileResult {
	jobs, err := r.store.ListRayJobs(ctx)
	if err != nil {
		return []ReconcileResult{{ID: "<list>", Err: wrapStoreErr(err)}}
	}
	out := make([]ReconcileResult, 0, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		action, err := r.reconcileOne(ctx, j, now)
		out = append(out, ReconcileResult{ID: j.ID.String(), Action: action, Err: err})
	}
	return out
}

// observe reads the job's backing resource. found is false for
// ProvisionErrNotFound; any other provisioner error is returned wrapped.
func (r *JobReconciler) observe(ctx context.Context, id core.ClusterId) (obs provision.ObservedJob, found bool, err error) {
	obs, oerr := r.jobs.ObserveJob(ctx, id)
	switch {
	case oerr == nil:
		return obs, true, nil
	case isProvisionNotFound(oerr):
		return provision.ObservedJob{}, false, nil
	default:
		return provision.ObservedJob{}, false, wrapProvisionErr(oerr)
	}
}

func (r *JobReconciler) reconcileOne(ctx context.Context, j *StoredRayJob, now uint64) (Action, error) {
	// Backoff gate (#43), same as clusters: a job whose apply keeps
	// failing is left alone until its next-attempt time.
	if j.Desired == DesiredRunning && now < j.NextAttemptAt {
		return ActionBackoff, nil
	}

	obs, found, err := r.observe(ctx, j.ID)
	if err != nil {
		return 0, err
	}

	quarantined, err := r.store.IsQuarantined(ctx)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	if quarantined {
		if found {
			if err := r.recordObserved(ctx, j, &obs); err != nil {
				return 0, err
			}
		}
		return ActionNoOp, nil
	}

	action := ActionNoOp
	switch j.Desired {
	case DesiredRunning:
		if found {
			break
		}
		// A finished job whose RayJob is gone (reaped out of band, or the
		// CR swept after completion) must never be re-run: the row is
		// history now, not intent.
		if jobIsTerminal(j.Status) || j.FinishedAt != nil {
			break
		}
		key := jobIntentKey(j.ID)
		outcome, err := r.store.BeginIntent(ctx, key, jobParamsFingerprint(&j.Spec))
		if err != nil {
			return 0, wrapStoreErr(err)
		}
		if outcome.Kind == IntentOutcomeParamMismatch {
			return 0, ReconcileError{Kind: ReconcileErrStaleIntent, Key: key}
		}
		queue, err := queueAssignmentForProject(ctx, r.store, j.Spec.Project)
		if err != nil {
			return 0, err
		}
		if err := r.jobs.ApplyJob(ctx, j.ID, &j.Spec, JobGeneration, queue); err != nil {
			// Unlike a cluster, whose apply failure is retried every tick,
			// a job that cannot be applied backs off: its spec never
			// changes, so the same failure would repeat unbounded.
			if berr := r.recordFailure(ctx, j, now); berr != nil {
				return 0, berr
			}
			return 0, wrapProvisionErr(err)
		}
		if err := r.store.CompleteIntent(ctx, key, ""); err != nil {
			return 0, wrapStoreErr(err)
		}
		if outcome.Replay {
			slog.Debug("re-applied existing job intent (replay)", "job", j.ID.String(), "key", key)
		}
		action = ActionApplied
		if obs, found, err = r.observe(ctx, j.ID); err != nil {
			return 0, err
		}
	case DesiredTerminated:
		if !found {
			break
		}
		if err := r.jobs.DeleteJob(ctx, j.ID); err != nil {
			return 0, wrapProvisionErr(err)
		}
		action = ActionTerminated
		if obs, found, err = r.observe(ctx, j.ID); err != nil {
			return 0, err
		}
	case DesiredSuspended:
		// Jobs are run-to-completion; there is nothing to suspend. The
		// API never sets this on a job.
	}

	// Persist status reconstructed from reality, keep the gateway in step,
	// and write the history record when the job just finished.
	if found {
		if err := r.recordObserved(ctx, j, &obs); err != nil {
			return 0, err
		}
	} else {
		r.deregister(j.ID)
		if j.Desired == DesiredTerminated {
			if err := r.recordGone(ctx, j, now); err != nil {
				return 0, err
			}
		}
	}

	// Backoff accounting (#43): an apply that left nothing observable
	// behind is no progress.
	switch {
	case action == ActionApplied && !found:
		if err := r.recordFailure(ctx, j, now); err != nil {
			return 0, err
		}
	case found && j.Desired == DesiredRunning && (j.FailureCount != 0 || j.NextAttemptAt != 0):
		if err := r.store.RecordRayJobAttempt(ctx, j.ID, 0, 0); err != nil {
			return 0, wrapStoreErr(err)
		}
	}
	return action, nil
}

// recordFailure bumps the job's failure count and pushes its next attempt
// out by the exponential backoff.
func (r *JobReconciler) recordFailure(ctx context.Context, j *StoredRayJob, now uint64) error {
	failureCount := satAddU32(j.FailureCount, 1)
	nextAttemptAt := satAddU64(now, backoffSecs(failureCount))
	if err := r.store.RecordRayJobAttempt(ctx, j.ID, failureCount, nextAttemptAt); err != nil {
		return wrapStoreErr(err)
	}
	slog.Warn("job made no progress — backing off",
		"target", "bifrost::audit", "job", j.ID.String(),
		"failure_count", failureCount, "retry_in_secs", backoffSecs(failureCount))
	return nil
}

// jobObservedTerminal reports whether an observation says the job is over:
// Ray's job status is terminal, or KubeRay's deployment status is (a job
// whose cluster never came up ends Failed without Ray ever reporting a
// status).
func jobObservedTerminal(obs *provision.ObservedJob) bool {
	return jobIsTerminal(obs.JobStatus) || provision.JobDeploymentIsTerminal(obs.DeploymentStatus)
}

// historyStatus is the Ray-vocabulary status the job history records for a
// terminal observation: Ray's own when it has one, else derived from
// KubeRay's deployment status.
func historyStatus(obs *provision.ObservedJob) string {
	if obs.JobStatus != "" {
		return obs.JobStatus
	}
	switch obs.DeploymentStatus {
	case provision.JobCompleteDeploymentStatus:
		return "SUCCEEDED"
	case provision.JobFailedDeploymentStatus, provision.JobValidationFailedDeploymentStatus:
		return "FAILED"
	default:
		return ""
	}
}

// recordObserved persists a found job's observation, side-writes the job
// history the first time the job is seen terminal, and registers or
// deregisters its cluster with the gateway.
func (r *JobReconciler) recordObserved(ctx context.Context, j *StoredRayJob, obs *provision.ObservedJob) error {
	if jobObservedTerminal(obs) && !jobIsTerminal(j.Status) && j.FinishedAt == nil {
		// History first, observation second: a crash in between replays
		// the side-write (RecordJob upserts by id), never loses it.
		if err := r.store.RecordJob(ctx, jobHistoryRecord(j, historyStatus(obs), obs.ClusterName, obs.StartTime, obs.EndTime)); err != nil {
			return wrapStoreErr(err)
		}
	}
	if err := r.store.RecordRayJobObservation(ctx, j.ID, RayJobObservation{
		Status:           obs.JobStatus,
		DeploymentStatus: obs.DeploymentStatus,
		ClusterName:      obs.ClusterName,
		DashboardURL:     obs.DashboardURL,
		Message:          obs.Message,
		StartedAt:        obs.StartTime,
		FinishedAt:       obs.EndTime,
	}); err != nil {
		return wrapStoreErr(err)
	}
	if j.Desired == DesiredRunning && obs.DeploymentStatus == provision.JobRunningDeploymentStatus && obs.DashboardURL != nil {
		r.register(j, *obs.DashboardURL)
	} else {
		r.deregister(j.ID)
	}
	return nil
}

// recordGone tombstones a desired-terminated job whose RayJob no longer
// exists: cluster name and dashboard cleared (RayJobObservedGone keys on
// them), a finish time stamped if the job never reached one (a job deleted
// mid-run is STOPPED at deletion), and the history written for a job that
// never got to report its own end.
func (r *JobReconciler) recordGone(ctx context.Context, j *StoredRayJob, now uint64) error {
	if RayJobObservedGone(j) {
		return nil
	}
	status := j.Status
	finishedAt := j.FinishedAt
	if !jobIsTerminal(status) {
		status = "STOPPED"
	}
	if finishedAt == nil {
		t := now
		finishedAt = &t
	}
	if !jobIsTerminal(j.Status) && j.FinishedAt == nil {
		if err := r.store.RecordJob(ctx, jobHistoryRecord(j, status, j.ClusterName, j.StartedAt, finishedAt)); err != nil {
			return wrapStoreErr(err)
		}
	}
	return wrapStoreErr(r.store.RecordRayJobObservation(ctx, j.ID, RayJobObservation{
		Status:           status,
		DeploymentStatus: j.DeploymentStatus,
		Message:          j.Message,
		StartedAt:        j.StartedAt,
		FinishedAt:       finishedAt,
	}))
}

// jobHistoryRecord is the JobRecord a finished ephemeral job leaves in the
// persistent history: submitter = the job's owner ("-" when
// unattributed, as the gateway records dev-mode submissions), cluster =
// the RayCluster that ran it, duration from the observed start/end.
func jobHistoryRecord(j *StoredRayJob, status string, clusterName *string, start, end *uint64) core.JobRecord {
	rec := core.JobRecord{Id: j.ID.String(), Submitter: "-", Status: status, SubmittedAt: j.SubmittedAt}
	if j.Owner != nil {
		rec.Submitter = *j.Owner
	}
	if clusterName != nil {
		rec.Cluster = *clusterName
	}
	if start != nil && end != nil {
		d := satSubU64(*end, *start)
		rec.DurationSecs = &d
	}
	return rec
}

func (r *JobReconciler) register(j *StoredRayJob, apiBaseURL string) {
	if r.registrar == nil || r.gatewayHostname == nil {
		return
	}
	err := r.registrar.Register(core.ClusterEndpoint{
		Id:         j.ID,
		Hostname:   r.gatewayHostname(j.ID),
		ApiBaseUrl: apiBaseURL,
		Project:    j.Spec.Project,
		Target:     core.RegistryTargetJobs,
		Source:     core.RegistrySourceDynamic,
	})
	if err != nil {
		slog.Warn("job cluster could not be registered with the gateway",
			"target", "bifrost::audit", "job", j.ID.String(), "error", err)
	}
}

func (r *JobReconciler) deregister(id core.ClusterId) {
	if r.registrar == nil {
		return
	}
	r.registrar.Deregister(id)
}

// RayJobObservedGone reports whether a job row is a dead tombstone: its
// backing resource has been observed absent (no cluster, no dashboard) and
// it carries a finish time. Shared by the retention sweep and the API's
// purge guard (DELETE /api/v1/jobs/{id}?purge=true).
func RayJobObservedGone(j *StoredRayJob) bool {
	return j.ClusterName == nil && j.DashboardURL == nil && j.FinishedAt != nil
}

// ReapTerminatedJobs is the tombstone retention sweep for jobs: hard-
// deletes rows desired=Terminated and observed gone for longer than the
// retention window, anchored on the job's finish time. Returns the ids
// removed.
func (r *JobReconciler) ReapTerminatedJobs(ctx context.Context, now uint64) ([]string, error) {
	jobs, err := r.store.ListRayJobs(ctx)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	removed := make([]string, 0)
	for i := range jobs {
		j := &jobs[i]
		if j.Desired != DesiredTerminated || !RayJobObservedGone(j) || satSubU64(now, *j.FinishedAt) < r.terminatedRetentionSecs {
			continue
		}
		ok, err := r.store.RemoveRayJob(ctx, j.ID)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		if ok {
			r.deregister(j.ID)
			slog.Info("terminated job row reaped (retention window elapsed)",
				"target", "bifrost::audit", "job", j.ID.String(), "age", satSubU64(now, *j.FinishedAt))
			removed = append(removed, j.ID.String())
		}
	}
	return removed, nil
}

// Run runs the job control loop until ctx is done: each tick reaps job
// tombstones then reconciles every job. The first pass runs immediately,
// like Reconciler.Run.
func (r *JobReconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("job reconcile loop started", "interval_secs", interval.Seconds())
	tick := func() {
		now := NowUnix()
		if ids, err := r.ReapTerminatedJobs(ctx, now); err != nil {
			slog.Warn("job tombstone reap pass failed", "error", err)
		} else if len(ids) > 0 {
			slog.Info("terminated job rows reaped", "reaped", len(ids))
		}
		for _, res := range r.ReconcileAllAt(ctx, now) {
			if res.Err != nil {
				slog.Warn("job reconcile failed", "job", res.ID, "error", res.Err)
			}
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ticker.C:
			tick()
		case <-ctx.Done():
			slog.Info("job reconcile loop shutting down")
			return
		}
	}
}

// RunJobReconciler constructs a JobReconciler from store/jobs/opts (the
// same Options the cluster loop takes: Registrar + GatewayHostname enable
// gateway registration, TerminatedRetentionSecs the tombstone window) and
// runs it until ctx is done. Symmetric with RunReconciler: the returned
// error is always nil today.
func RunJobReconciler(ctx context.Context, store Store, jobs provision.JobProvisioner, opts Options) error {
	r := NewJobReconciler(store, jobs)
	if opts.Registrar != nil && opts.GatewayHostname != nil {
		r.WithRegistrar(opts.Registrar, opts.GatewayHostname)
	}
	if opts.TerminatedRetentionSecs != nil {
		r.WithTerminatedRetention(*opts.TerminatedRetentionSecs)
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	r.Run(ctx, interval)
	return nil
}
