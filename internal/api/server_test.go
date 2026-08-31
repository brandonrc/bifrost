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

// TestUnimplementedOperationsReturn501 spot-checks a representative
// sample of the still-unported strict-server operations (Wave 1
// T11/T12 burn these down) for the canonical 501 envelope. The full
// count (45 = 47 spec operations minus healthz/version) is asserted
// directly against the interface in TestNotImplementedCount.
func TestUnimplementedOperationsReturn501(t *testing.T) {
	h := devHandler()
	for _, req := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/clusters", ""},
		{http.MethodPost, "/api/v1/clusters", "{}"},
		{http.MethodGet, "/api/v1/pools", ""},
		{http.MethodGet, "/api/v1/identity", ""},
		{http.MethodGet, "/api/v1/usage", ""},
		{http.MethodPost, "/api/v1/auth/login", "{}"},
	} {
		var body io.Reader
		if req.body != "" {
			body = strings.NewReader(req.body)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(req.method, req.path, body))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want 501, body=%s", req.method, req.path, rec.Code, rec.Body.String())
			continue
		}
		assertErrorBody(t, rec, "not_implemented")
	}
}

// TestNotImplementedCount asserts the exact burn-down count: every
// StrictServerInterface method returns ErrNotImplemented except Healthz
// and Version. 47 spec operations - 2 implemented = 45.
func TestNotImplementedCount(t *testing.T) {
	s := NewServer()
	ctx := context.Background()
	implemented := 0
	notImplemented := 0

	check := func(name string, resp any, err error) {
		t.Helper()
		if err == ErrNotImplemented {
			notImplemented++
			if resp != nil {
				t.Errorf("%s: expected nil response alongside ErrNotImplemented, got %#v", name, resp)
			}
			return
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			return
		}
		implemented++
	}

	r1, e1 := s.Healthz(ctx, HealthzRequestObject{})
	check("Healthz", r1, e1)
	r2, e2 := s.Version(ctx, VersionRequestObject{})
	check("Version", r2, e2)

	r, e := s.ListAssignments(ctx, ListAssignmentsRequestObject{})
	check("ListAssignments", r, e)
	r3, e3 := s.DeleteAssignment(ctx, DeleteAssignmentRequestObject{})
	check("DeleteAssignment", r3, e3)
	r4, e4 := s.UpsertAssignment(ctx, UpsertAssignmentRequestObject{})
	check("UpsertAssignment", r4, e4)
	r5, e5 := s.ListRoles(ctx, ListRolesRequestObject{})
	check("ListRoles", r5, e5)
	r6, e6 := s.ListAuditEvents(ctx, ListAuditEventsRequestObject{})
	check("ListAuditEvents", r6, e6)
	r7, e7 := s.VerifyAuditTrail(ctx, VerifyAuditTrailRequestObject{})
	check("VerifyAuditTrail", r7, e7)
	r8, e8 := s.Login(ctx, LoginRequestObject{})
	check("Login", r8, e8)
	r9, e9 := s.Logout(ctx, LogoutRequestObject{})
	check("Logout", r9, e9)
	r10, e10 := s.Providers(ctx, ProvidersRequestObject{})
	check("Providers", r10, e10)
	r11, e11 := s.ListTokens(ctx, ListTokensRequestObject{})
	check("ListTokens", r11, e11)
	r12, e12 := s.CreateToken(ctx, CreateTokenRequestObject{})
	check("CreateToken", r12, e12)
	r13, e13 := s.RevokeToken(ctx, RevokeTokenRequestObject{})
	check("RevokeToken", r13, e13)
	r14, e14 := s.ListUsers(ctx, ListUsersRequestObject{})
	check("ListUsers", r14, e14)
	r15, e15 := s.CreateUser(ctx, CreateUserRequestObject{})
	check("CreateUser", r15, e15)
	r16, e16 := s.UpdateUser(ctx, UpdateUserRequestObject{})
	check("UpdateUser", r16, e16)
	r17, e17 := s.ListClusters(ctx, ListClustersRequestObject{})
	check("ListClusters", r17, e17)
	r18, e18 := s.CreateCluster(ctx, CreateClusterRequestObject{})
	check("CreateCluster", r18, e18)
	r19, e19 := s.DeleteCluster(ctx, DeleteClusterRequestObject{})
	check("DeleteCluster", r19, e19)
	r20, e20 := s.GetCluster(ctx, GetClusterRequestObject{})
	check("GetCluster", r20, e20)
	r21, e21 := s.ClusterEvents(ctx, ClusterEventsRequestObject{})
	check("ClusterEvents", r21, e21)
	r22, e22 := s.ClusterJobs(ctx, ClusterJobsRequestObject{})
	check("ClusterJobs", r22, e22)
	r23, e23 := s.ClusterLogs(ctx, ClusterLogsRequestObject{})
	check("ClusterLogs", r23, e23)
	r24, e24 := s.ClusterMetrics(ctx, ClusterMetricsRequestObject{})
	check("ClusterMetrics", r24, e24)
	r25, e25 := s.ClusterNodes(ctx, ClusterNodesRequestObject{})
	check("ClusterNodes", r25, e25)
	r26, e26 := s.ResumeCluster(ctx, ResumeClusterRequestObject{})
	check("ResumeCluster", r26, e26)
	r27, e27 := s.SuspendCluster(ctx, SuspendClusterRequestObject{})
	check("SuspendCluster", r27, e27)
	r28, e28 := s.Identity(ctx, IdentityRequestObject{})
	check("Identity", r28, e28)
	r29, e29 := s.ListJobs(ctx, ListJobsRequestObject{})
	check("ListJobs", r29, e29)
	r30, e30 := s.Metrics(ctx, MetricsRequestObject{})
	check("Metrics", r30, e30)
	r31, e31 := s.ListPools(ctx, ListPoolsRequestObject{})
	check("ListPools", r31, e31)
	r32, e32 := s.CreatePool(ctx, CreatePoolRequestObject{})
	check("CreatePool", r32, e32)
	r33, e33 := s.DeletePool(ctx, DeletePoolRequestObject{})
	check("DeletePool", r33, e33)
	r34, e34 := s.GetPool(ctx, GetPoolRequestObject{})
	check("GetPool", r34, e34)
	r35, e35 := s.ListAllocations(ctx, ListAllocationsRequestObject{})
	check("ListAllocations", r35, e35)
	r36, e36 := s.DeleteAllocation(ctx, DeleteAllocationRequestObject{})
	check("DeleteAllocation", r36, e36)
	r37, e37 := s.PutAllocation(ctx, PutAllocationRequestObject{})
	check("PutAllocation", r37, e37)
	r38, e38 := s.PoolUsage(ctx, PoolUsageRequestObject{})
	check("PoolUsage", r38, e38)
	r39, e39 := s.ListRegistry(ctx, ListRegistryRequestObject{})
	check("ListRegistry", r39, e39)
	r40, e40 := s.ListServices(ctx, ListServicesRequestObject{})
	check("ListServices", r40, e40)
	r41, e41 := s.DeployService(ctx, DeployServiceRequestObject{})
	check("DeployService", r41, e41)
	r42, e42 := s.DeleteService(ctx, DeleteServiceRequestObject{})
	check("DeleteService", r42, e42)
	r43, e43 := s.GetService(ctx, GetServiceRequestObject{})
	check("GetService", r43, e43)
	r44, e44 := s.GetPolicy(ctx, GetPolicyRequestObject{})
	check("GetPolicy", r44, e44)
	r45, e45 := s.UpdatePolicy(ctx, UpdatePolicyRequestObject{})
	check("UpdatePolicy", r45, e45)
	r46, e46 := s.UsageReport(ctx, UsageReportRequestObject{})
	check("UsageReport", r46, e46)

	if implemented != 2 {
		t.Errorf("implemented count = %d, want 2 (Healthz, Version)", implemented)
	}
	if notImplemented != 45 {
		t.Errorf("not-implemented count = %d, want 45", notImplemented)
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
