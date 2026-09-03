package controller

import (
	"context"
	"sync"

	"github.com/brandonrc/bifrost/internal/core"
)

// FailingStore is a test-only Store that delegates to a MemoryStore but
// fails the named methods with an injected backend error — for exercising
// the reconcile/pool-reconcile loops' per-tick error discipline (log,
// skip, never fatal). Ported from the predecessor's controller crate, src/store.rs's
// testkit::FailingStore.
//
// Unlike the Rust reference (which fails by matching its own snake_case
// Store trait method names), method names here are this file's Go
// method names ("List", "ReapIntents", "ListPools", ...) — this testkit
// is internal to this package's own tests, so there is no cross-language
// name to stay faithful to.
type FailingStore struct {
	inner Store

	mu   sync.Mutex
	fail map[string]bool
}

// NewFailingStore returns a FailingStore wrapping a fresh MemoryStore.
func NewFailingStore() *FailingStore {
	return &FailingStore{inner: NewMemoryStore(), fail: make(map[string]bool)}
}

// Fail makes method (a Store method name, matching this type's own method
// names) fail from now on.
func (s *FailingStore) Fail(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = true
}

func (s *FailingStore) check(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[method] {
		return storeErrorf("injected %s failure", method)
	}
	return nil
}

var _ Store = (*FailingStore)(nil)

func (s *FailingStore) UpsertDesired(ctx context.Context, id core.ClusterId, spec core.ClusterSpec) (uint64, error) {
	if err := s.check("UpsertDesired"); err != nil {
		return 0, err
	}
	return s.inner.UpsertDesired(ctx, id, spec)
}

func (s *FailingStore) Get(ctx context.Context, id core.ClusterId) (*StoredCluster, error) {
	if err := s.check("Get"); err != nil {
		return nil, err
	}
	return s.inner.Get(ctx, id)
}

func (s *FailingStore) List(ctx context.Context) ([]StoredCluster, error) {
	if err := s.check("List"); err != nil {
		return nil, err
	}
	return s.inner.List(ctx)
}

func (s *FailingStore) SetDesired(ctx context.Context, id core.ClusterId, desired DesiredState) error {
	if err := s.check("SetDesired"); err != nil {
		return err
	}
	return s.inner.SetDesired(ctx, id, desired)
}

func (s *FailingStore) RemoveCluster(ctx context.Context, id core.ClusterId) (bool, error) {
	if err := s.check("RemoveCluster"); err != nil {
		return false, err
	}
	return s.inner.RemoveCluster(ctx, id)
}

func (s *FailingStore) RecordObservation(ctx context.Context, id core.ClusterId, observed *core.ClusterState, observedGeneration uint64) error {
	if err := s.check("RecordObservation"); err != nil {
		return err
	}
	return s.inner.RecordObservation(ctx, id, observed, observedGeneration)
}

func (s *FailingStore) SetCondition(ctx context.Context, id core.ClusterId, condition *core.DriftCondition) error {
	if err := s.check("SetCondition"); err != nil {
		return err
	}
	return s.inner.SetCondition(ctx, id, condition)
}

func (s *FailingStore) IsQuarantined(ctx context.Context) (bool, error) {
	if err := s.check("IsQuarantined"); err != nil {
		return false, err
	}
	return s.inner.IsQuarantined(ctx)
}

func (s *FailingStore) SetQuarantine(ctx context.Context, quarantined bool) error {
	if err := s.check("SetQuarantine"); err != nil {
		return err
	}
	return s.inner.SetQuarantine(ctx, quarantined)
}

func (s *FailingStore) RecordAttempt(ctx context.Context, id core.ClusterId, failureCount uint32, nextAttemptAt uint64) error {
	if err := s.check("RecordAttempt"); err != nil {
		return err
	}
	return s.inner.RecordAttempt(ctx, id, failureCount, nextAttemptAt)
}

func (s *FailingStore) BeginIntent(ctx context.Context, key, fingerprint string) (IntentOutcome, error) {
	if err := s.check("BeginIntent"); err != nil {
		return IntentOutcome{}, err
	}
	return s.inner.BeginIntent(ctx, key, fingerprint)
}

func (s *FailingStore) CompleteIntent(ctx context.Context, key, responseJSON string) error {
	if err := s.check("CompleteIntent"); err != nil {
		return err
	}
	return s.inner.CompleteIntent(ctx, key, responseJSON)
}

func (s *FailingStore) GetIntent(ctx context.Context, key string) (*IntentRecord, error) {
	if err := s.check("GetIntent"); err != nil {
		return nil, err
	}
	return s.inner.GetIntent(ctx, key)
}

func (s *FailingStore) ReapIntents(ctx context.Context, appliedBefore uint64) (uint64, error) {
	if err := s.check("ReapIntents"); err != nil {
		return 0, err
	}
	return s.inner.ReapIntents(ctx, appliedBefore)
}

func (s *FailingStore) RecordJob(ctx context.Context, job core.JobRecord) error {
	if err := s.check("RecordJob"); err != nil {
		return err
	}
	return s.inner.RecordJob(ctx, job)
}

func (s *FailingStore) ListJobs(ctx context.Context) ([]core.JobRecord, error) {
	if err := s.check("ListJobs"); err != nil {
		return nil, err
	}
	return s.inner.ListJobs(ctx)
}

func (s *FailingStore) UpsertPool(ctx context.Context, name string, spec core.PoolSpec) (uint64, error) {
	if err := s.check("UpsertPool"); err != nil {
		return 0, err
	}
	return s.inner.UpsertPool(ctx, name, spec)
}

func (s *FailingStore) GetPool(ctx context.Context, name string) (*StoredPool, error) {
	if err := s.check("GetPool"); err != nil {
		return nil, err
	}
	return s.inner.GetPool(ctx, name)
}

func (s *FailingStore) ListPools(ctx context.Context) ([]StoredPool, error) {
	if err := s.check("ListPools"); err != nil {
		return nil, err
	}
	return s.inner.ListPools(ctx)
}

func (s *FailingStore) DeletePool(ctx context.Context, name string) error {
	if err := s.check("DeletePool"); err != nil {
		return err
	}
	return s.inner.DeletePool(ctx, name)
}

func (s *FailingStore) RecordPoolObservation(ctx context.Context, name, observedJSON string) error {
	if err := s.check("RecordPoolObservation"); err != nil {
		return err
	}
	return s.inner.RecordPoolObservation(ctx, name, observedJSON)
}

func (s *FailingStore) UpsertAllocation(ctx context.Context, alloc core.AllocationSpec) error {
	if err := s.check("UpsertAllocation"); err != nil {
		return err
	}
	return s.inner.UpsertAllocation(ctx, alloc)
}

func (s *FailingStore) ListAllocations(ctx context.Context, pool string) ([]core.AllocationSpec, error) {
	if err := s.check("ListAllocations"); err != nil {
		return nil, err
	}
	return s.inner.ListAllocations(ctx, pool)
}

func (s *FailingStore) DeleteAllocation(ctx context.Context, pool, project string) error {
	if err := s.check("DeleteAllocation"); err != nil {
		return err
	}
	return s.inner.DeleteAllocation(ctx, pool, project)
}

func (s *FailingStore) RecordUsageSamples(ctx context.Context, samples []UsageSample) error {
	if err := s.check("RecordUsageSamples"); err != nil {
		return err
	}
	return s.inner.RecordUsageSamples(ctx, samples)
}

func (s *FailingStore) UsageSamples(ctx context.Context, project, pool, owner *string, from, to uint64) ([]UsageSample, error) {
	if err := s.check("UsageSamples"); err != nil {
		return nil, err
	}
	return s.inner.UsageSamples(ctx, project, pool, owner, from, to)
}

func (s *FailingStore) UpsertService(ctx context.Context, name string, spec core.ServiceSpec, owner *string) (uint64, error) {
	if err := s.check("UpsertService"); err != nil {
		return 0, err
	}
	return s.inner.UpsertService(ctx, name, spec, owner)
}

func (s *FailingStore) GetService(ctx context.Context, name string) (*StoredService, error) {
	if err := s.check("GetService"); err != nil {
		return nil, err
	}
	return s.inner.GetService(ctx, name)
}

func (s *FailingStore) ListServices(ctx context.Context) ([]StoredService, error) {
	if err := s.check("ListServices"); err != nil {
		return nil, err
	}
	return s.inner.ListServices(ctx)
}

func (s *FailingStore) SetServiceDesired(ctx context.Context, name string, desired DesiredState) error {
	if err := s.check("SetServiceDesired"); err != nil {
		return err
	}
	return s.inner.SetServiceDesired(ctx, name, desired)
}

func (s *FailingStore) RecordServiceObservation(ctx context.Context, name string, observed *core.ClusterState, url *string) error {
	if err := s.check("RecordServiceObservation"); err != nil {
		return err
	}
	return s.inner.RecordServiceObservation(ctx, name, observed, url)
}

func (s *FailingStore) RemoveService(ctx context.Context, name string) (bool, error) {
	if err := s.check("RemoveService"); err != nil {
		return false, err
	}
	return s.inner.RemoveService(ctx, name)
}

func (s *FailingStore) UpsertRayJob(ctx context.Context, id core.ClusterId, spec core.RayJobSpec, owner *string) error {
	if err := s.check("UpsertRayJob"); err != nil {
		return err
	}
	return s.inner.UpsertRayJob(ctx, id, spec, owner)
}

func (s *FailingStore) GetRayJob(ctx context.Context, id core.ClusterId) (*StoredRayJob, error) {
	if err := s.check("GetRayJob"); err != nil {
		return nil, err
	}
	return s.inner.GetRayJob(ctx, id)
}

func (s *FailingStore) ListRayJobs(ctx context.Context) ([]StoredRayJob, error) {
	if err := s.check("ListRayJobs"); err != nil {
		return nil, err
	}
	return s.inner.ListRayJobs(ctx)
}

func (s *FailingStore) SetRayJobDesired(ctx context.Context, id core.ClusterId, desired DesiredState) error {
	if err := s.check("SetRayJobDesired"); err != nil {
		return err
	}
	return s.inner.SetRayJobDesired(ctx, id, desired)
}

func (s *FailingStore) RecordRayJobObservation(ctx context.Context, id core.ClusterId, obs RayJobObservation) error {
	if err := s.check("RecordRayJobObservation"); err != nil {
		return err
	}
	return s.inner.RecordRayJobObservation(ctx, id, obs)
}

func (s *FailingStore) RecordRayJobAttempt(ctx context.Context, id core.ClusterId, failureCount uint32, nextAttemptAt uint64) error {
	if err := s.check("RecordRayJobAttempt"); err != nil {
		return err
	}
	return s.inner.RecordRayJobAttempt(ctx, id, failureCount, nextAttemptAt)
}

func (s *FailingStore) RemoveRayJob(ctx context.Context, id core.ClusterId) (bool, error) {
	if err := s.check("RemoveRayJob"); err != nil {
		return false, err
	}
	return s.inner.RemoveRayJob(ctx, id)
}

func (s *FailingStore) GetPolicy(ctx context.Context) (*StoredPolicy, error) {
	if err := s.check("GetPolicy"); err != nil {
		return nil, err
	}
	return s.inner.GetPolicy(ctx)
}

func (s *FailingStore) SetPolicy(ctx context.Context, policy *StoredPolicy) error {
	if err := s.check("SetPolicy"); err != nil {
		return err
	}
	return s.inner.SetPolicy(ctx, policy)
}

func (s *FailingStore) SeedPolicy(ctx context.Context, policy *StoredPolicy) (bool, error) {
	if err := s.check("SeedPolicy"); err != nil {
		return false, err
	}
	return s.inner.SeedPolicy(ctx, policy)
}

func (s *FailingStore) RecordAudit(ctx context.Context, event *core.AuditEvent) (uint64, error) {
	if err := s.check("RecordAudit"); err != nil {
		return 0, err
	}
	return s.inner.RecordAudit(ctx, event)
}

func (s *FailingStore) ListAudit(ctx context.Context, filter core.AuditFilter) ([]AuditRow, *uint64, error) {
	if err := s.check("ListAudit"); err != nil {
		return nil, nil, err
	}
	return s.inner.ListAudit(ctx, filter)
}

func (s *FailingStore) AuditChain(ctx context.Context, fromSeq *uint64, limit uint32) (AuditChainWindow, error) {
	if err := s.check("AuditChain"); err != nil {
		return AuditChainWindow{}, err
	}
	return s.inner.AuditChain(ctx, fromSeq, limit)
}

func (s *FailingStore) CreateLocalUser(ctx context.Context, username string, email *string, passwordHash string, role core.LocalRole) error {
	if err := s.check("CreateLocalUser"); err != nil {
		return err
	}
	return s.inner.CreateLocalUser(ctx, username, email, passwordHash, role)
}

func (s *FailingStore) GetLocalUser(ctx context.Context, username string) (*core.LocalUserRecord, error) {
	if err := s.check("GetLocalUser"); err != nil {
		return nil, err
	}
	return s.inner.GetLocalUser(ctx, username)
}

func (s *FailingStore) ListLocalUsers(ctx context.Context) ([]core.LocalUserRecord, error) {
	if err := s.check("ListLocalUsers"); err != nil {
		return nil, err
	}
	return s.inner.ListLocalUsers(ctx)
}

func (s *FailingStore) SetLocalUserPassword(ctx context.Context, username, passwordHash string) error {
	if err := s.check("SetLocalUserPassword"); err != nil {
		return err
	}
	return s.inner.SetLocalUserPassword(ctx, username, passwordHash)
}

func (s *FailingStore) SetLocalUserRole(ctx context.Context, username string, role core.LocalRole) error {
	if err := s.check("SetLocalUserRole"); err != nil {
		return err
	}
	return s.inner.SetLocalUserRole(ctx, username, role)
}

func (s *FailingStore) SetLocalUserDisabled(ctx context.Context, username string, disabled bool) error {
	if err := s.check("SetLocalUserDisabled"); err != nil {
		return err
	}
	return s.inner.SetLocalUserDisabled(ctx, username, disabled)
}

func (s *FailingStore) SetLoginLockout(ctx context.Context, username string, failedLogins uint32, lockedUntil *uint64) error {
	if err := s.check("SetLoginLockout"); err != nil {
		return err
	}
	return s.inner.SetLoginLockout(ctx, username, failedLogins, lockedUntil)
}

func (s *FailingStore) RecordLoginFailure(ctx context.Context, username string) error {
	if err := s.check("RecordLoginFailure"); err != nil {
		return err
	}
	return s.inner.RecordLoginFailure(ctx, username)
}

func (s *FailingStore) RecordLoginSuccess(ctx context.Context, username string) error {
	if err := s.check("RecordLoginSuccess"); err != nil {
		return err
	}
	return s.inner.RecordLoginSuccess(ctx, username)
}

func (s *FailingStore) CreateApiToken(ctx context.Context, record core.ApiTokenRecord) error {
	if err := s.check("CreateApiToken"); err != nil {
		return err
	}
	return s.inner.CreateApiToken(ctx, record)
}

func (s *FailingStore) GetApiTokenByPrefix(ctx context.Context, prefix string) (*core.ApiTokenRecord, error) {
	if err := s.check("GetApiTokenByPrefix"); err != nil {
		return nil, err
	}
	return s.inner.GetApiTokenByPrefix(ctx, prefix)
}

func (s *FailingStore) ListApiTokens(ctx context.Context, username string) ([]core.ApiTokenRecord, error) {
	if err := s.check("ListApiTokens"); err != nil {
		return nil, err
	}
	return s.inner.ListApiTokens(ctx, username)
}

func (s *FailingStore) RevokeApiToken(ctx context.Context, prefix, username string) error {
	if err := s.check("RevokeApiToken"); err != nil {
		return err
	}
	return s.inner.RevokeApiToken(ctx, prefix, username)
}

func (s *FailingStore) TouchApiToken(ctx context.Context, prefix string, now uint64) error {
	if err := s.check("TouchApiToken"); err != nil {
		return err
	}
	return s.inner.TouchApiToken(ctx, prefix, now)
}

func (s *FailingStore) UpsertRoleAssignment(ctx context.Context, principal, role, scope string) error {
	if err := s.check("UpsertRoleAssignment"); err != nil {
		return err
	}
	return s.inner.UpsertRoleAssignment(ctx, principal, role, scope)
}

func (s *FailingStore) ListRoleAssignments(ctx context.Context, principal *string) ([]RoleAssignment, error) {
	if err := s.check("ListRoleAssignments"); err != nil {
		return nil, err
	}
	return s.inner.ListRoleAssignments(ctx, principal)
}

func (s *FailingStore) DeleteRoleAssignment(ctx context.Context, principal, role, scope string) error {
	if err := s.check("DeleteRoleAssignment"); err != nil {
		return err
	}
	return s.inner.DeleteRoleAssignment(ctx, principal, role, scope)
}
