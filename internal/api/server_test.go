package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
)

func devHandler() http.Handler {
	return NewHandler(NewServer(), HandlerOptions{AllowUnauthenticated: true})
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	devHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

func TestVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	devHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var v VersionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Name != "bifrost" {
		t.Errorf("name = %q, want %q", v.Name, "bifrost")
	}
	if v.Version == "" {
		t.Error("version should not be empty")
	}
}

func TestSpecIsServedAtDocumentedPath(t *testing.T) {
	rec := httptest.NewRecorder()
	devHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, SpecPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), specJSON) {
		t.Error("served spec does not match the embedded openapi.json")
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served spec is not valid JSON: %v", err)
	}
	if doc["openapi"] == nil {
		t.Error("served spec has no openapi version field")
	}
}

// TestOperationsAreNoLongerUnimplemented is TestUnimplementedOperationsReturn501's
// T12 successor: T11's version of this test spot-checked a representative
// sample of still-unported operations for the canonical 501 envelope. T12
// ported the last of them (pools/services/cluster_obs/usage/audit/
// local_auth), so the same five routes now must NOT 501 — this asserts the
// burn-down actually happened at the wire level, not just that some other
// status was returned.
func TestOperationsAreNoLongerUnimplemented(t *testing.T) {
	// Unlike devHandler() (a bare NewServer(), nil Store), these routes now
	// have real logic that touches s.Store — a fully-wired Server is
	// required or ListPools/UsageReport/Login panic on the nil dependency
	// before this test gets to assert anything about their status code.
	store := controller.NewMemoryStore()
	s := &Server{Store: store, Local: auth.NewLocalAuthenticator(store, 3600, 90)}
	h := NewHandler(s, HandlerOptions{AllowUnauthenticated: true})
	for _, req := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/pools", ""},
		{http.MethodPost, "/api/v1/pools", "{}"},
		{http.MethodGet, "/api/v1/services", ""},
		{http.MethodGet, "/api/v1/usage", ""},
		{http.MethodPost, "/api/v1/auth/login", "{}"},
	} {
		var body io.Reader
		if req.body != "" {
			body = strings.NewReader(req.body)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(req.method, req.path, body))
		if rec.Code == http.StatusNotImplemented {
			t.Errorf("%s %s: still 501 (body=%s)", req.method, req.path, rec.Body.String())
		}
	}
}

// TestAllOperationsImplemented is TestNotImplementedCount's T12 successor:
// with every operation now ported, the burn-down invariant flips from
// "exactly 30 unported" to "exactly zero". Unlike T11's version, this
// wires a FULLY-dependency-injected Server (a real memory store, fake
// Provisioner/ServiceProvisioner, a local authenticator) so every one of
// the 47 operations can be safely invoked with a zero-value request object
// instead of skipping the ones that would panic on a bare NewServer() —
// every handler in this package nil-checks its dependencies/req.Body
// before touching them, so a zero-value request against a real Server
// answers with a graceful error (400/401/403/404/...) or a real success,
// NEVER ErrNotImplemented and never a panic. A recover() per call turns an
// accidental panic into a normal test failure naming the operation,
// instead of aborting the whole suite.
func TestAllOperationsImplemented(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{
		Store:              store,
		Provisioner:        fakeProvisioner{},
		ServiceProvisioner: fakeServiceProvisioner{},
		Local:              auth.NewLocalAuthenticator(store, 3600, 90),
	}
	ctx := context.Background()

	notImplemented := 0
	panicked := 0
	call := func(name string, fn func() (any, error)) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				panicked++
				t.Errorf("%s: panicked: %v", name, r)
			}
		}()
		_, err := fn()
		if err == ErrNotImplemented {
			notImplemented++
			t.Errorf("%s: still returns ErrNotImplemented", name)
		}
	}

	call("Healthz", func() (any, error) { return s.Healthz(ctx, HealthzRequestObject{}) })
	call("Version", func() (any, error) { return s.Version(ctx, VersionRequestObject{}) })
	call("ListClusters", func() (any, error) { return s.ListClusters(ctx, ListClustersRequestObject{}) })
	call("CreateCluster", func() (any, error) { return s.CreateCluster(ctx, CreateClusterRequestObject{}) })
	call("DeleteCluster", func() (any, error) { return s.DeleteCluster(ctx, DeleteClusterRequestObject{}) })
	call("GetCluster", func() (any, error) { return s.GetCluster(ctx, GetClusterRequestObject{}) })
	call("ResumeCluster", func() (any, error) { return s.ResumeCluster(ctx, ResumeClusterRequestObject{}) })
	call("SuspendCluster", func() (any, error) { return s.SuspendCluster(ctx, SuspendClusterRequestObject{}) })
	call("ListJobs", func() (any, error) { return s.ListJobs(ctx, ListJobsRequestObject{}) })
	call("ListRegistry", func() (any, error) { return s.ListRegistry(ctx, ListRegistryRequestObject{}) })
	call("GetPolicy", func() (any, error) { return s.GetPolicy(ctx, GetPolicyRequestObject{}) })
	call("UpdatePolicy", func() (any, error) { return s.UpdatePolicy(ctx, UpdatePolicyRequestObject{}) })
	call("ListAssignments", func() (any, error) { return s.ListAssignments(ctx, ListAssignmentsRequestObject{}) })
	call("DeleteAssignment", func() (any, error) { return s.DeleteAssignment(ctx, DeleteAssignmentRequestObject{}) })
	call("UpsertAssignment", func() (any, error) { return s.UpsertAssignment(ctx, UpsertAssignmentRequestObject{}) })
	call("ListRoles", func() (any, error) { return s.ListRoles(ctx, ListRolesRequestObject{}) })
	call("Identity", func() (any, error) { return s.Identity(ctx, IdentityRequestObject{}) })

	call("ListAuditEvents", func() (any, error) { return s.ListAuditEvents(ctx, ListAuditEventsRequestObject{}) })
	call("VerifyAuditTrail", func() (any, error) { return s.VerifyAuditTrail(ctx, VerifyAuditTrailRequestObject{}) })
	call("Login", func() (any, error) { return s.Login(ctx, LoginRequestObject{}) })
	call("Logout", func() (any, error) { return s.Logout(ctx, LogoutRequestObject{}) })
	call("Providers", func() (any, error) { return s.Providers(ctx, ProvidersRequestObject{}) })
	call("ListTokens", func() (any, error) { return s.ListTokens(ctx, ListTokensRequestObject{}) })
	call("CreateToken", func() (any, error) { return s.CreateToken(ctx, CreateTokenRequestObject{}) })
	call("RevokeToken", func() (any, error) { return s.RevokeToken(ctx, RevokeTokenRequestObject{}) })
	call("ListUsers", func() (any, error) { return s.ListUsers(ctx, ListUsersRequestObject{}) })
	call("CreateUser", func() (any, error) { return s.CreateUser(ctx, CreateUserRequestObject{}) })
	call("UpdateUser", func() (any, error) { return s.UpdateUser(ctx, UpdateUserRequestObject{}) })
	call("ClusterEvents", func() (any, error) { return s.ClusterEvents(ctx, ClusterEventsRequestObject{}) })
	call("ClusterJobs", func() (any, error) { return s.ClusterJobs(ctx, ClusterJobsRequestObject{}) })
	call("ClusterLogs", func() (any, error) { return s.ClusterLogs(ctx, ClusterLogsRequestObject{}) })
	call("ClusterMetrics", func() (any, error) { return s.ClusterMetrics(ctx, ClusterMetricsRequestObject{}) })
	call("ClusterNodes", func() (any, error) { return s.ClusterNodes(ctx, ClusterNodesRequestObject{}) })
	call("Metrics", func() (any, error) { return s.Metrics(ctx, MetricsRequestObject{}) })
	call("ListPools", func() (any, error) { return s.ListPools(ctx, ListPoolsRequestObject{}) })
	call("CreatePool", func() (any, error) { return s.CreatePool(ctx, CreatePoolRequestObject{}) })
	call("DeletePool", func() (any, error) { return s.DeletePool(ctx, DeletePoolRequestObject{}) })
	call("GetPool", func() (any, error) { return s.GetPool(ctx, GetPoolRequestObject{}) })
	call("ListAllocations", func() (any, error) { return s.ListAllocations(ctx, ListAllocationsRequestObject{}) })
	call("DeleteAllocation", func() (any, error) { return s.DeleteAllocation(ctx, DeleteAllocationRequestObject{}) })
	call("PutAllocation", func() (any, error) { return s.PutAllocation(ctx, PutAllocationRequestObject{}) })
	call("PoolUsage", func() (any, error) { return s.PoolUsage(ctx, PoolUsageRequestObject{}) })
	call("ListServices", func() (any, error) { return s.ListServices(ctx, ListServicesRequestObject{}) })
	call("DeployService", func() (any, error) { return s.DeployService(ctx, DeployServiceRequestObject{}) })
	call("DeleteService", func() (any, error) { return s.DeleteService(ctx, DeleteServiceRequestObject{}) })
	call("GetService", func() (any, error) { return s.GetService(ctx, GetServiceRequestObject{}) })
	call("UsageReport", func() (any, error) { return s.UsageReport(ctx, UsageReportRequestObject{}) })

	if notImplemented != 0 {
		t.Errorf("not-implemented count = %d, want 0 (every strict-server operation is ported)", notImplemented)
	}
	if panicked != 0 {
		t.Errorf("%d operation(s) panicked against a fully-wired Server", panicked)
	}
}

func TestWriteError(t *testing.T) {
	t.Run("HTTPError carries its own status/code/message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, nil, ErrNotImplemented)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
		assertErrorBody(t, rec, "not_implemented")
	})

	t.Run("plain error falls back to 500 internal_error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteError(rec, nil, context.DeadlineExceeded)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		assertErrorBody(t, rec, "internal_error")
	})
}
