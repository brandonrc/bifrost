package controller

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/brandonrc/bifrost/internal/core"
)

// MemoryStore is an in-memory Store for tests and single-node dev, ported
// from mobula-controller's `memory::InMemoryStore` (store.rs:946-1554):
// mutex-guarded maps per collection, mirroring the Rust reference's
// field-level `Mutex`/`AtomicBool`/`AtomicU64` granularity rather than one
// coarse lock, so port fidelity extends to locking shape as well as
// behavior.
type MemoryStore struct {
	clustersMu sync.Mutex
	clusters   map[core.ClusterId]StoredCluster

	intentsMu sync.Mutex
	intents   map[string]IntentRecord

	quarantined atomic.Bool

	jobsMu sync.Mutex
	jobs   map[string]core.JobRecord

	poolsMu sync.Mutex
	pools   map[string]StoredPool

	allocationsMu sync.Mutex
	allocations   map[allocationKey]core.AllocationSpec

	usageMu sync.Mutex
	usage   []UsageSample

	servicesMu sync.Mutex
	services   map[string]StoredService

	rayJobsMu sync.Mutex
	rayJobs   map[core.ClusterId]StoredRayJob

	// policy is the singleton governance-policy row; nil = never seeded,
	// never edited.
	policyMu sync.Mutex
	policy   *StoredPolicy

	// audit holds (seq, event, chain_hash) in insertion order; seq is
	// 1-based from auditSeq. The chain hash is computed at append time.
	auditMu  sync.Mutex
	audit    []ChainedAuditRow
	auditSeq atomic.Uint64

	localUsersMu sync.Mutex
	localUsers   map[string]core.LocalUserRecord

	apiTokensMu sync.Mutex
	apiTokens   map[string]core.ApiTokenRecord

	// assignments holds scoped role assignments keyed by (principal,
	// role, scope).
	assignmentsMu sync.Mutex
	assignments   map[assignmentKey]RoleAssignment
}

type allocationKey struct {
	pool    string
	project string
}

type assignmentKey struct {
	principal string
	role      string
	scope     string
}

// NewMemoryStore returns a ready-to-use in-memory Store.
func NewMemoryStore() Store {
	return &MemoryStore{
		clusters:    make(map[core.ClusterId]StoredCluster),
		intents:     make(map[string]IntentRecord),
		jobs:        make(map[string]core.JobRecord),
		pools:       make(map[string]StoredPool),
		allocations: make(map[allocationKey]core.AllocationSpec),
		services:    make(map[string]StoredService),
		rayJobs:     make(map[core.ClusterId]StoredRayJob),
		localUsers:  make(map[string]core.LocalUserRecord),
		apiTokens:   make(map[string]core.ApiTokenRecord),
		assignments: make(map[assignmentKey]RoleAssignment),
	}
}

var _ Store = (*MemoryStore)(nil)

// --- Clusters ---

func (s *MemoryStore) UpsertDesired(_ context.Context, id core.ClusterId, spec core.ClusterSpec) (uint64, error) {
	// Deep-copy on ingress: spec is caller-owned memory (WorkerGroups,
	// TtlSeconds/IdleTimeoutSecs/Owner pointers); storing it by shallow
	// copy would let the caller mutate stored state after the call
	// returns.
	spec = cloneClusterSpec(spec)

	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()

	existing, ok := s.clusters[id]
	revive := ok && existing.Desired == DesiredTerminated
	var generation uint64
	switch {
	case revive:
		// A terminated record is re-created, never edited: bump the
		// generation so the provisioner sees a new apply, and start from a
		// fresh record below.
		generation = existing.Generation + 1
	case ok && !specChanged(&existing.Spec, &spec):
		generation = existing.Generation
	case ok:
		generation = existing.Generation + 1
	default:
		generation = 1
	}

	record := StoredCluster{
		ID:         id,
		Spec:       spec,
		Generation: generation,
		Desired:    DesiredRunning,
		CreatedAt:  NowUnix(),
	}
	if ok && !revive {
		record.Desired = existing.Desired
		record.ObservedState = existing.ObservedState
		record.ObservedGeneration = existing.ObservedGeneration
		record.Condition = existing.Condition
		record.FailureCount = existing.FailureCount
		record.NextAttemptAt = existing.NextAttemptAt
		record.CreatedAt = existing.CreatedAt
		record.TerminatedAt = existing.TerminatedAt
	}
	s.clusters[id] = record
	return generation, nil
}

func (s *MemoryStore) Get(_ context.Context, id core.ClusterId) (*StoredCluster, error) {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return nil, nil
	}
	// Deep-copy on egress: c's Spec/pointer fields must not alias the
	// stored map entry, or a caller mutating the returned value would
	// mutate the store.
	cc := cloneStoredCluster(c)
	return &cc, nil
}

func (s *MemoryStore) List(_ context.Context) ([]StoredCluster, error) {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	out := make([]StoredCluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		out = append(out, cloneStoredCluster(c))
	}
	return out, nil
}

func (s *MemoryStore) SetDesired(_ context.Context, id core.ClusterId, desired DesiredState) error {
	if !desired.isValid() {
		return errBadDesiredState(string(desired))
	}

	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return errNoSuchCluster(string(id))
	}
	c.Desired = desired
	// Anchor (or clear) the tombstone-retention clock: first transition
	// into DesiredTerminated stamps the time; any move away from it
	// clears it.
	if desired == DesiredTerminated {
		if c.TerminatedAt == nil {
			now := NowUnix()
			c.TerminatedAt = &now
		}
	} else {
		c.TerminatedAt = nil
	}
	s.clusters[id] = c
	return nil
}

func (s *MemoryStore) RemoveCluster(_ context.Context, id core.ClusterId) (bool, error) {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	_, ok := s.clusters[id]
	if ok {
		delete(s.clusters, id)
	}
	return ok, nil
}

func (s *MemoryStore) RecordObservation(_ context.Context, id core.ClusterId, observed *core.ClusterState, observedGeneration uint64) error {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return nil
	}
	// Deep-copy on ingress: observed is a caller-owned pointer.
	c.ObservedState = clonePtr(observed)
	// Monotonic fence: never roll the observed generation backwards (a
	// stale-restore observation must not overwrite a newer one).
	if observedGeneration > c.ObservedGeneration {
		c.ObservedGeneration = observedGeneration
	}
	s.clusters[id] = c
	return nil
}

func (s *MemoryStore) SetCondition(_ context.Context, id core.ClusterId, condition *core.DriftCondition) error {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return nil
	}
	// Deep-copy on ingress: condition is a caller-owned pointer.
	c.Condition = clonePtr(condition)
	s.clusters[id] = c
	return nil
}

func (s *MemoryStore) IsQuarantined(_ context.Context) (bool, error) {
	return s.quarantined.Load(), nil
}

func (s *MemoryStore) SetQuarantine(_ context.Context, quarantined bool) error {
	s.quarantined.Store(quarantined)
	return nil
}

func (s *MemoryStore) RecordAttempt(_ context.Context, id core.ClusterId, failureCount uint32, nextAttemptAt uint64) error {
	s.clustersMu.Lock()
	defer s.clustersMu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return nil
	}
	c.FailureCount = failureCount
	c.NextAttemptAt = nextAttemptAt
	s.clusters[id] = c
	return nil
}

// --- Transactional outbox ---

func (s *MemoryStore) BeginIntent(_ context.Context, key, fingerprint string) (IntentOutcome, error) {
	s.intentsMu.Lock()
	defer s.intentsMu.Unlock()
	existing, ok := s.intents[key]
	switch {
	case ok && existing.ParamsFingerprint != fingerprint:
		return IntentOutcome{Kind: IntentOutcomeParamMismatch}, nil
	case ok:
		return IntentOutcome{Kind: IntentOutcomeProceed, Replay: true}, nil
	default:
		s.intents[key] = IntentRecord{
			Key:               key,
			ParamsFingerprint: fingerprint,
			Status:            IntentStatusPending,
			CreatedAt:         NowUnix(),
		}
		return IntentOutcome{Kind: IntentOutcomeProceed, Replay: false}, nil
	}
}

// CompleteIntent reviewed for the same aliasing class as GetIntent/
// RecordJob/etc. (round 2): no fix needed here. responseJSON is a
// by-value string parameter — &responseJSON takes the address of this
// call's own local copy, not the caller's variable — and completedAt is
// a freshly computed local. Both pointers this method stores are
// already self-owned; nothing here can alias caller memory. (GetIntent
// still needs cloneIntentRecord on its egress path, since its map
// lookup returns a shallow copy of this method's already-safe pointers,
// which then alias the *store's* copy.)
func (s *MemoryStore) CompleteIntent(_ context.Context, key, responseJSON string) error {
	s.intentsMu.Lock()
	defer s.intentsMu.Unlock()
	rec, ok := s.intents[key]
	if !ok {
		return nil
	}
	rec.Status = IntentStatusApplied
	rec.ResponseJSON = &responseJSON
	completedAt := NowUnix()
	rec.CompletedAt = &completedAt
	s.intents[key] = rec
	return nil
}

func (s *MemoryStore) GetIntent(_ context.Context, key string) (*IntentRecord, error) {
	s.intentsMu.Lock()
	defer s.intentsMu.Unlock()
	rec, ok := s.intents[key]
	if !ok {
		return nil, nil
	}
	// Deep-copy on egress: ResponseJSON/CompletedAt are pointers.
	rr := cloneIntentRecord(rec)
	return &rr, nil
}

func (s *MemoryStore) ReapIntents(_ context.Context, appliedBefore uint64) (uint64, error) {
	s.intentsMu.Lock()
	defer s.intentsMu.Unlock()
	var removed uint64
	for k, rec := range s.intents {
		if rec.Status == IntentStatusApplied && rec.CompletedAt != nil && *rec.CompletedAt < appliedBefore {
			delete(s.intents, k)
			removed++
		}
	}
	return removed, nil
}

// --- Jobs ---

func (s *MemoryStore) RecordJob(_ context.Context, job core.JobRecord) error {
	// Deep-copy on ingress: job.DurationSecs is a caller-owned pointer.
	job = cloneJobRecord(job)

	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	s.jobs[job.Id] = job
	return nil
}

func (s *MemoryStore) ListJobs(_ context.Context) ([]core.JobRecord, error) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	out := make([]core.JobRecord, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, cloneJobRecord(j))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SubmittedAt > out[j].SubmittedAt
	})
	return out, nil
}

// --- Pools ---

func (s *MemoryStore) UpsertPool(_ context.Context, name string, spec core.PoolSpec) (uint64, error) {
	// Deep-copy on ingress: spec's Flavors/GpuSharing are caller-owned.
	spec = clonePoolSpec(spec)

	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	existing, ok := s.pools[name]
	var generation uint64
	switch {
	case ok && !poolSpecChanged(&existing.Spec, &spec):
		generation = existing.Generation
	case ok:
		generation = existing.Generation + 1
	default:
		generation = 1
	}
	record := StoredPool{
		Name:       name,
		Spec:       spec,
		Generation: generation,
		CreatedAt:  NowUnix(),
	}
	if ok {
		// Observations survive spec updates, like cluster observed state.
		record.ObservedJSON = existing.ObservedJSON
		record.ObservedAt = existing.ObservedAt
		record.CreatedAt = existing.CreatedAt
	}
	s.pools[name] = record
	return generation, nil
}

func (s *MemoryStore) GetPool(_ context.Context, name string) (*StoredPool, error) {
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	p, ok := s.pools[name]
	if !ok {
		return nil, nil
	}
	// Deep-copy on egress.
	pp := cloneStoredPool(p)
	return &pp, nil
}

func (s *MemoryStore) ListPools(_ context.Context) ([]StoredPool, error) {
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	out := make([]StoredPool, 0, len(s.pools))
	for _, p := range s.pools {
		out = append(out, cloneStoredPool(p))
	}
	return out, nil
}

func (s *MemoryStore) DeletePool(_ context.Context, name string) error {
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	if _, ok := s.pools[name]; !ok {
		return errNoSuchPool(name)
	}
	delete(s.pools, name)
	return nil
}

func (s *MemoryStore) RecordPoolObservation(_ context.Context, name, observedJSON string) error {
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	p, ok := s.pools[name]
	if !ok {
		return nil
	}
	p.ObservedJSON = &observedJSON
	observedAt := NowUnix()
	p.ObservedAt = &observedAt
	s.pools[name] = p
	return nil
}

// --- Allocations ---

func (s *MemoryStore) UpsertAllocation(_ context.Context, alloc core.AllocationSpec) error {
	// Deep-copy on ingress: alloc's three maps are caller-owned.
	alloc = cloneAllocationSpec(alloc)

	s.allocationsMu.Lock()
	defer s.allocationsMu.Unlock()
	s.allocations[allocationKey{pool: alloc.Pool, project: alloc.Project}] = alloc
	return nil
}

func (s *MemoryStore) ListAllocations(_ context.Context, pool string) ([]core.AllocationSpec, error) {
	s.allocationsMu.Lock()
	defer s.allocationsMu.Unlock()
	out := make([]core.AllocationSpec, 0)
	for _, a := range s.allocations {
		if a.Pool == pool {
			out = append(out, cloneAllocationSpec(a))
		}
	}
	return out, nil
}

func (s *MemoryStore) DeleteAllocation(_ context.Context, pool, project string) error {
	s.allocationsMu.Lock()
	defer s.allocationsMu.Unlock()
	key := allocationKey{pool: pool, project: project}
	if _, ok := s.allocations[key]; !ok {
		return errNoSuchAllocation(pool, project)
	}
	delete(s.allocations, key)
	return nil
}

// --- Usage samples ---

func (s *MemoryStore) RecordUsageSamples(_ context.Context, samples []UsageSample) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.usage = append(s.usage, samples...)
	return nil
}

func (s *MemoryStore) UsageSamples(_ context.Context, project, pool, owner *string, from, to uint64) ([]UsageSample, error) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	out := make([]UsageSample, 0)
	for _, u := range s.usage {
		if u.Ts < from || u.Ts > to {
			continue
		}
		if project != nil && u.Project != *project {
			continue
		}
		if pool != nil && u.Pool != *pool {
			continue
		}
		if owner != nil && u.Owner != *owner {
			continue
		}
		out = append(out, u)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Ts < out[j].Ts
	})
	return out, nil
}

// --- Governance policy ---

func (s *MemoryStore) GetPolicy(_ context.Context) (*StoredPolicy, error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.policy == nil {
		return nil, nil
	}
	// Deep-copy on egress: Prices/Quotas/Budgets are maps.
	p := cloneStoredPolicy(*s.policy)
	return &p, nil
}

func (s *MemoryStore) SetPolicy(_ context.Context, policy *StoredPolicy) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	// Deep-copy on ingress: policy is a caller-owned pointer.
	p := cloneStoredPolicy(*policy)
	s.policy = &p
	return nil
}

func (s *MemoryStore) SeedPolicy(_ context.Context, policy *StoredPolicy) (bool, error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.policy != nil {
		return false, nil
	}
	// Deep-copy on ingress.
	p := cloneStoredPolicy(*policy)
	s.policy = &p
	return true, nil
}

// --- Audit ---

func (s *MemoryStore) RecordAudit(_ context.Context, event *core.AuditEvent) (uint64, error) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	seq := s.auditSeq.Add(1)
	prev := AuditGenesisHash
	if n := len(s.audit); n > 0 {
		prev = s.audit[n-1].ChainHash
	}
	chainHash := AuditChainHash(prev, event)
	// Deep-copy on ingress: *event is a caller-owned value whose pointer
	// fields (Subject, Required, ...) and GrantedRoles slice still alias
	// caller memory after a plain dereference. Storing an alias here
	// means a later caller mutation of *event silently rewrites the
	// stored row without touching chainHash, which VerifyAuditChain then
	// reports as a tamper that never happened.
	s.audit = append(s.audit, ChainedAuditRow{Seq: seq, Event: cloneAuditEvent(*event), ChainHash: chainHash})
	return seq, nil
}

func (s *MemoryStore) AuditChain(_ context.Context, fromSeq *uint64, limit uint32) (AuditChainWindow, error) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	from := uint64(1)
	if fromSeq != nil {
		from = *fromSeq
	}

	head := AuditGenesisHash
	for i := len(s.audit) - 1; i >= 0; i-- {
		if s.audit[i].Seq < from {
			head = s.audit[i].ChainHash
			break
		}
	}

	window := make([]ChainedAuditRow, 0)
	for _, r := range s.audit {
		if r.Seq < from {
			continue
		}
		if uint32(len(window)) >= limit {
			break
		}
		// Deep-copy on egress: r.Event's pointer fields/GrantedRoles
		// must not alias the stored row.
		window = append(window, ChainedAuditRow{Seq: r.Seq, Event: cloneAuditEvent(r.Event), ChainHash: r.ChainHash})
	}

	return AuditChainWindow{Head: head, Rows: window}, nil
}

func (s *MemoryStore) ListAudit(_ context.Context, filter core.AuditFilter) ([]AuditRow, *uint64, error) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	limit := int(filter.EffectiveLimit())
	rows := make([]AuditRow, 0)
	for _, r := range s.audit {
		if filter.Cursor != nil && r.Seq >= *filter.Cursor {
			continue
		}
		if !filter.Matches(&r.Event) {
			continue
		}
		// Deep-copy on egress.
		rows = append(rows, AuditRow{Seq: r.Seq, Event: cloneAuditEvent(r.Event)})
	}
	// Newest first; insertion order is ascending seq.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	var nextCursor *uint64
	if len(rows) > limit {
		rows = rows[:limit]
		seq := rows[len(rows)-1].Seq
		nextCursor = &seq
	}
	return rows, nextCursor, nil
}

// --- Local auth: users ---

func (s *MemoryStore) CreateLocalUser(_ context.Context, username string, email *string, passwordHash string, role core.LocalRole) error {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	if _, ok := s.localUsers[username]; ok {
		return errLocalUserAlreadyExists(username)
	}
	s.localUsers[username] = core.LocalUserRecord{
		Username: username,
		// Deep-copy on ingress: email is a caller-owned pointer.
		Email:        clonePtr(email),
		PasswordHash: passwordHash,
		Role:         role,
		CreatedAt:    NowUnix(),
	}
	return nil
}

func (s *MemoryStore) GetLocalUser(_ context.Context, username string) (*core.LocalUserRecord, error) {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	u, ok := s.localUsers[username]
	if !ok {
		return nil, nil
	}
	// Deep-copy on egress: Email/LockedUntil are pointers.
	uu := cloneLocalUserRecord(u)
	return &uu, nil
}

func (s *MemoryStore) ListLocalUsers(_ context.Context) ([]core.LocalUserRecord, error) {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	out := make([]core.LocalUserRecord, 0, len(s.localUsers))
	for _, u := range s.localUsers {
		out = append(out, cloneLocalUserRecord(u))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Username < out[j].Username
	})
	return out, nil
}

func (s *MemoryStore) SetLocalUserPassword(_ context.Context, username, passwordHash string) error {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	u, ok := s.localUsers[username]
	if !ok {
		return errNoSuchLocalUser(username)
	}
	u.PasswordHash = passwordHash
	s.localUsers[username] = u
	return nil
}

func (s *MemoryStore) SetLocalUserRole(_ context.Context, username string, role core.LocalRole) error {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	u, ok := s.localUsers[username]
	if !ok {
		return errNoSuchLocalUser(username)
	}
	u.Role = role
	s.localUsers[username] = u
	return nil
}

func (s *MemoryStore) SetLocalUserDisabled(_ context.Context, username string, disabled bool) error {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	u, ok := s.localUsers[username]
	if !ok {
		return errNoSuchLocalUser(username)
	}
	u.Disabled = disabled
	s.localUsers[username] = u
	return nil
}

func (s *MemoryStore) SetLoginLockout(_ context.Context, username string, failedLogins uint32, lockedUntil *uint64) error {
	s.localUsersMu.Lock()
	defer s.localUsersMu.Unlock()
	u, ok := s.localUsers[username]
	if !ok {
		return errNoSuchLocalUser(username)
	}
	u.FailedLogins = failedLogins
	// Deep-copy on ingress: lockedUntil is a caller-owned pointer.
	u.LockedUntil = clonePtr(lockedUntil)
	s.localUsers[username] = u
	return nil
}

// RecordLoginFailure implements the shared lockout state machine
// (NextLoginFailureState) against this store's own Get/SetLoginLockout —
// the Go equivalent of the Rust trait's default method body, which every
// Store implementation there gets for free but Go interfaces cannot
// express, so each backend (this one now; SQLite/Postgres in Tasks 3-4)
// implements the same three-line body.
func (s *MemoryStore) RecordLoginFailure(ctx context.Context, username string) error {
	user, err := s.GetLocalUser(ctx, username)
	if err != nil {
		return err
	}
	if user == nil {
		return errNoSuchLocalUser(username)
	}
	failed, locked := NextLoginFailureState(user.FailedLogins, NowUnix())
	return s.SetLoginLockout(ctx, username, failed, locked)
}

func (s *MemoryStore) RecordLoginSuccess(ctx context.Context, username string) error {
	return s.SetLoginLockout(ctx, username, 0, nil)
}

// --- Local auth: API tokens ---

func (s *MemoryStore) CreateApiToken(_ context.Context, record core.ApiTokenRecord) error {
	s.apiTokensMu.Lock()
	defer s.apiTokensMu.Unlock()
	if _, ok := s.apiTokens[record.Prefix]; ok {
		return errApiTokenAlreadyExists(record.Prefix)
	}
	// Deep-copy on ingress: record.LastUsedAt is a caller-owned pointer.
	s.apiTokens[record.Prefix] = cloneApiTokenRecord(record)
	return nil
}

func (s *MemoryStore) GetApiTokenByPrefix(_ context.Context, prefix string) (*core.ApiTokenRecord, error) {
	s.apiTokensMu.Lock()
	defer s.apiTokensMu.Unlock()
	t, ok := s.apiTokens[prefix]
	if !ok {
		return nil, nil
	}
	// Deep-copy on egress: LastUsedAt is a pointer.
	tt := cloneApiTokenRecord(t)
	return &tt, nil
}

func (s *MemoryStore) ListApiTokens(_ context.Context, username string) ([]core.ApiTokenRecord, error) {
	s.apiTokensMu.Lock()
	defer s.apiTokensMu.Unlock()
	out := make([]core.ApiTokenRecord, 0)
	for _, t := range s.apiTokens {
		if t.Username == username {
			out = append(out, cloneApiTokenRecord(t))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

func (s *MemoryStore) RevokeApiToken(_ context.Context, prefix, username string) error {
	s.apiTokensMu.Lock()
	defer s.apiTokensMu.Unlock()
	t, ok := s.apiTokens[prefix]
	if !ok || t.Username != username {
		return errNoSuchApiToken(prefix)
	}
	t.Revoked = true
	s.apiTokens[prefix] = t
	return nil
}

func (s *MemoryStore) TouchApiToken(_ context.Context, prefix string, now uint64) error {
	s.apiTokensMu.Lock()
	defer s.apiTokensMu.Unlock()
	t, ok := s.apiTokens[prefix]
	if !ok {
		return nil
	}
	t.LastUsedAt = &now
	s.apiTokens[prefix] = t
	return nil
}

// --- Scoped role assignments ---

func (s *MemoryStore) UpsertRoleAssignment(_ context.Context, principal, role, scope string) error {
	s.assignmentsMu.Lock()
	defer s.assignmentsMu.Unlock()
	key := assignmentKey{principal: principal, role: role, scope: scope}
	// Re-upsert preserves the original created_at.
	createdAt := NowUnix()
	if existing, ok := s.assignments[key]; ok {
		createdAt = existing.CreatedAt
	}
	s.assignments[key] = RoleAssignment{
		Principal: principal,
		Role:      role,
		Scope:     scope,
		CreatedAt: createdAt,
	}
	return nil
}

func (s *MemoryStore) ListRoleAssignments(_ context.Context, principal *string) ([]RoleAssignment, error) {
	s.assignmentsMu.Lock()
	defer s.assignmentsMu.Unlock()
	out := make([]RoleAssignment, 0)
	for _, a := range s.assignments {
		if principal != nil && a.Principal != *principal {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Principal != out[j].Principal {
			return out[i].Principal < out[j].Principal
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

func (s *MemoryStore) DeleteRoleAssignment(_ context.Context, principal, role, scope string) error {
	s.assignmentsMu.Lock()
	defer s.assignmentsMu.Unlock()
	key := assignmentKey{principal: principal, role: role, scope: scope}
	if _, ok := s.assignments[key]; !ok {
		return errNoSuchAssignment(principal, role, scope)
	}
	delete(s.assignments, key)
	return nil
}

// --- Services ---

func (s *MemoryStore) UpsertService(_ context.Context, name string, spec core.ServiceSpec, owner *string) (uint64, error) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	// Deep-copy on ingress: spec/owner are caller-owned.
	spec = cloneServiceSpec(spec)
	owner = clonePtr(owner)
	cur, ok := s.services[name]
	switch {
	case !ok:
		s.services[name] = StoredService{
			Name: name, Spec: spec, Owner: owner, Generation: 1,
			Desired: DesiredRunning, CreatedAt: NowUnix(),
		}
		return 1, nil
	case cur.Desired == DesiredTerminated:
		// Store.UpsertService: a terminated record is re-created.
		gen := cur.Generation + 1
		s.services[name] = StoredService{
			Name: name, Spec: spec, Owner: owner, Generation: gen,
			Desired: DesiredRunning, CreatedAt: NowUnix(),
		}
		return gen, nil
	default:
		if serviceSpecChanged(&cur.Spec, &spec) {
			cur.Generation++
		}
		cur.Spec = spec
		s.services[name] = cur
		return cur.Generation, nil
	}
}

func (s *MemoryStore) GetService(_ context.Context, name string) (*StoredService, error) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	c, ok := s.services[name]
	if !ok {
		return nil, nil
	}
	cc := cloneStoredService(c)
	return &cc, nil
}

func (s *MemoryStore) ListServices(_ context.Context) ([]StoredService, error) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	out := make([]StoredService, 0, len(s.services))
	for _, c := range s.services {
		out = append(out, cloneStoredService(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *MemoryStore) SetServiceDesired(_ context.Context, name string, desired DesiredState) error {
	if !desired.isValid() {
		return errBadDesiredState(string(desired))
	}
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	c, ok := s.services[name]
	if !ok {
		return errNoSuchService(name)
	}
	c.Desired = desired
	if desired == DesiredTerminated {
		if c.TerminatedAt == nil {
			now := NowUnix()
			c.TerminatedAt = &now
		}
	} else {
		c.TerminatedAt = nil
	}
	s.services[name] = c
	return nil
}

func (s *MemoryStore) RecordServiceObservation(_ context.Context, name string, observed *core.ClusterState, url *string) error {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	c, ok := s.services[name]
	if !ok {
		return nil
	}
	c.ObservedState = clonePtr(observed)
	c.ObservedURL = clonePtr(url)
	s.services[name] = c
	return nil
}

func (s *MemoryStore) RemoveService(_ context.Context, name string) (bool, error) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	_, ok := s.services[name]
	if ok {
		delete(s.services, name)
	}
	return ok, nil
}

// --- Ephemeral Ray jobs ---

func (s *MemoryStore) UpsertRayJob(_ context.Context, id core.ClusterId, spec core.RayJobSpec, owner *string) error {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	spec = cloneRayJobSpec(spec)
	owner = clonePtr(owner)
	cur, ok := s.rayJobs[id]
	if !ok {
		s.rayJobs[id] = StoredRayJob{ID: id, Spec: spec, Owner: owner, Desired: DesiredRunning, SubmittedAt: NowUnix()}
		return nil
	}
	cur.Spec = spec
	cur.Owner = owner
	s.rayJobs[id] = cur
	return nil
}

func (s *MemoryStore) GetRayJob(_ context.Context, id core.ClusterId) (*StoredRayJob, error) {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	j, ok := s.rayJobs[id]
	if !ok {
		return nil, nil
	}
	jj := cloneStoredRayJob(j)
	return &jj, nil
}

func (s *MemoryStore) ListRayJobs(_ context.Context) ([]StoredRayJob, error) {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	out := make([]StoredRayJob, 0, len(s.rayJobs))
	for _, j := range s.rayJobs {
		out = append(out, cloneStoredRayJob(j))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubmittedAt != out[j].SubmittedAt {
			return out[i].SubmittedAt > out[j].SubmittedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *MemoryStore) SetRayJobDesired(_ context.Context, id core.ClusterId, desired DesiredState) error {
	if !desired.isValid() {
		return errBadDesiredState(string(desired))
	}
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	j, ok := s.rayJobs[id]
	if !ok {
		return errNoSuchRayJob(string(id))
	}
	j.Desired = desired
	s.rayJobs[id] = j
	return nil
}

func (s *MemoryStore) RecordRayJobObservation(_ context.Context, id core.ClusterId, obs RayJobObservation) error {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	j, ok := s.rayJobs[id]
	if !ok {
		return nil
	}
	obs = cloneRayJobObservation(obs)
	j.Status = obs.Status
	j.DeploymentStatus = obs.DeploymentStatus
	j.ClusterName = obs.ClusterName
	j.DashboardURL = obs.DashboardURL
	j.Message = obs.Message
	j.StartedAt = obs.StartedAt
	j.FinishedAt = obs.FinishedAt
	s.rayJobs[id] = j
	return nil
}

func (s *MemoryStore) RecordRayJobAttempt(_ context.Context, id core.ClusterId, failureCount uint32, nextAttemptAt uint64) error {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	j, ok := s.rayJobs[id]
	if !ok {
		return nil
	}
	j.FailureCount = failureCount
	j.NextAttemptAt = nextAttemptAt
	s.rayJobs[id] = j
	return nil
}

func (s *MemoryStore) RemoveRayJob(_ context.Context, id core.ClusterId) (bool, error) {
	s.rayJobsMu.Lock()
	defer s.rayJobsMu.Unlock()
	_, ok := s.rayJobs[id]
	if ok {
		delete(s.rayJobs, id)
	}
	return ok, nil
}
