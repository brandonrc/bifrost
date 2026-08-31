package core

// Persistent job history (PLAN §Phase 3, spec §5.5).
//
// Ray dashboards forget a job the moment its cluster goes away. Bifrost
// records each job it sees submitted through the gateway into its own
// store, so the history outlives the clusters that ran it. This is the
// record shape; the store persists it (SQLite dev, Postgres prod) and the
// API lists it at GET /api/v1/jobs.

// JobRecord is a single job Bifrost has observed, independent of its
// cluster's lifecycle.
type JobRecord struct {
	// Id is the Ray submission id (stable across the job's life).
	Id string `json:"id"`
	// Cluster is the id of the cluster the job ran on (may since be
	// terminated/gone).
	Cluster string `json:"cluster"`
	// Submitter is the authenticated subject that submitted it ("-" in
	// dev-unauthenticated).
	Submitter string `json:"submitter"`
	// Status is the Ray job status, verbatim: PENDING | RUNNING |
	// SUCCEEDED | FAILED | STOPPED. Kept as a string so a Ray status
	// rename doesn't break the store.
	Status string `json:"status"`
	// DurationSecs is the wall-clock duration in seconds once the job
	// reaches a terminal state; nil while it is still running.
	DurationSecs *uint64 `json:"duration_secs"`
	// SubmittedAt is unix seconds when the job was submitted.
	SubmittedAt uint64 `json:"submitted_at"`
}
