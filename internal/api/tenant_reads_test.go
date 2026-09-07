// Tenant read-scoping tests for the red-team findings: observability
// reads (cluster list/get, the five obs sub-endpoints, the global job
// history) must not cross tenant boundaries. The rule under test
// (clusterTenantAccess): a caller reads a stored cluster only as Admin,
// Auditor (reads only), the recorded owner, or a holder of a project-
// scoped assignment covering the cluster's project whose role grants the
// read — a global viewer/developer/operator with zero project ties reads
// only what they own. Denials follow the existing conventions: 404 for
// by-name reads (never leak existence), filtering for lists, 403 for the
// gateway (matching the mutation convention). Style mirrors
// clusters_test.go / cluster_obs_test.go / rayjobs_test.go.
package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
)

// seedOwnedCluster seeds a store-backed cluster with a recorded owner (the
// server-side-stamped ownership the read-scope matches against).
func seedOwnedCluster(t *testing.T, s *Server, id, project, owner string) {
	t.Helper()
	if _, err := s.Store.UpsertDesired(context.Background(), core.ClusterId(id), core.ClusterSpec{
		Name: id, Project: project, Owner: &owner, RayVersion: "2.9.0", Image: "x", HeadCpu: "1", HeadMemory: "1Gi",
	}); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

// projectMember returns an identity with a group-derived project-scoped
// assignment and no global roles — the self-service shape the findings
// call "project member".
func projectMember(subject string, role auth.Role, project string) *auth.Identity {
	id := testIdentity(subject)
	id.ProjectRoles = []auth.RoleScope{{Role: role, Scope: "project:" + project}}
	return id
}

// --- GetCluster: the by-name read must not leak foreign clusters ---

func TestGetCluster_TenantMatrix(t *testing.T) {
	store := newMemStore(t)
	s := &Server{Store: store}
	seedOwnedCluster(t, s, "c1", "proj-a", "owen")

	cases := []struct {
		name string
		id   *auth.Identity
		err  bool // true → expect a 404 (never leaks existence)
	}{
		{"foreign viewer with no projects", testIdentity("vicky", auth.RoleViewer), true},
		{"foreign developer with no projects", testIdentity("dave", auth.RoleDeveloper), true},
		{"foreign operator with no projects", testIdentity("olga", auth.RoleOperator), true},
		{"owner", testIdentity("owen", auth.RoleViewer), false},
		{"project member (operator on proj-a)", projectMember("pam", auth.RoleOperator, "proj-a"), false},
		{"member of a different project", projectMember("paul", auth.RoleOperator, "proj-b"), true},
		{"admin", testIdentity("root", auth.RoleAdmin), false},
		{"auditor", testIdentity("audit-1", auth.RoleAuditor), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := s.GetCluster(ctxWithIdentity(c.id), GetClusterRequestObject{Id: "c1"})
			if c.err {
				mustHTTPError(t, err, 404)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			view := mustResponse[GetCluster200JSONResponse](t, resp)
			if view.Id != "c1" {
				t.Errorf("view.Id = %q, want c1", view.Id)
			}
		})
	}
}

// --- ListClusters: filter, never deny (except the no-grants gate) ---

func TestListClusters_ForeignClustersFilteredForEveryGlobalRole(t *testing.T) {
	store := newMemStore(t)
	s := &Server{Store: store}
	seedOwnedCluster(t, s, "mine", "proj-a", "owen")
	seedOwnedCluster(t, s, "foreign", "proj-b", "bob")

	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleDeveloper, auth.RoleOperator} {
		id := testIdentity("owen", role)
		resp, err := s.ListClusters(ctxWithIdentity(id), ListClustersRequestObject{})
		if err != nil {
			t.Fatalf("role %v: unexpected error: %v", role, err)
		}
		views := mustResponse[ListClusters200JSONResponse](t, resp)
		if len(views) != 1 || views[0].Id != "mine" {
			t.Fatalf("role %v: views = %+v, want exactly the owned cluster 'mine'", role, views)
		}
	}
}

func TestListClusters_AuditorAndAdminSeeEverything(t *testing.T) {
	store := newMemStore(t)
	s := &Server{Store: store}
	seedOwnedCluster(t, s, "c1", "proj-a", "owen")
	seedOwnedCluster(t, s, "c2", "proj-b", "bob")

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleAuditor} {
		resp, err := s.ListClusters(ctxWithIdentity(testIdentity("root", role)), ListClustersRequestObject{})
		if err != nil {
			t.Fatalf("role %v: unexpected error: %v", role, err)
		}
		if views := mustResponse[ListClusters200JSONResponse](t, resp); len(views) != 2 {
			t.Fatalf("role %v: got %d clusters, want 2", role, len(views))
		}
	}
}

// --- ClusterLogs: the obs sub-endpoints get the same scope ---

func TestClusterLogs_TenantMatrix(t *testing.T) {
	logs := &core.ClusterLogs{ClusterId: "c1", Pod: "c1-head", Pods: []string{"c1-head"}, Lines: []string{"a"}, Tail: 1}
	cases := []struct {
		name    string
		id      *auth.Identity
		foreign bool
	}{
		{"foreign viewer with no projects", testIdentity("vicky", auth.RoleViewer), true},
		{"owner", testIdentity("owen", auth.RoleViewer), false},
		{"project member (operator on proj-a)", projectMember("pam", auth.RoleOperator, "proj-a"), false},
		{"member of a different project", projectMember("paul", auth.RoleOperator, "proj-b"), true},
		{"admin", testIdentity("root", auth.RoleAdmin), false},
		{"auditor", testIdentity("audit-1", auth.RoleAuditor), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{Store: newMemStore(t), Provisioner: &scriptedProvisioner{logs: logs}}
			seedOwnedCluster(t, s, "c1", "proj-a", "owen")

			resp, err := s.ClusterLogs(ctxWithIdentity(c.id), ClusterLogsRequestObject{Id: "c1"})
			if c.foreign {
				mustHTTPError(t, err, 404)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			mustResponse[ClusterLogs200JSONResponse](t, resp)
		})
	}
}

// --- ListJobs: the global job history is scoped the same way ---

func TestListJobs_TenantScoping(t *testing.T) {
	store := newMemStore(t)
	s := &Server{Store: store}
	seedOwnedCluster(t, s, "c1", "proj-a", "owen")
	seedOwnedCluster(t, s, "c2", "proj-b", "bob")
	for _, j := range []core.JobRecord{
		{Id: "j1", Cluster: "c1", Submitter: "owen", Status: "SUCCEEDED", SubmittedAt: 100},
		{Id: "j2", Cluster: "c2", Submitter: "bob", Status: "RUNNING", SubmittedAt: 200},
		{Id: "j3", Cluster: "c1", Submitter: "bob", Status: "FAILED", SubmittedAt: 300},
	} {
		if err := store.RecordJob(context.Background(), j); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}

	// Viewer with no projects sees nothing (no submissions, no clusters).
	resp, err := s.ListJobs(ctxWithIdentity(testIdentity("vicky", auth.RoleViewer)), ListJobsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if views := mustResponse[ListJobs200JSONResponse](t, resp); len(views) != 0 {
		t.Fatalf("foreign viewer: views = %+v, want empty", views)
	}

	// The cluster owner sees everything submitted to their own cluster
	// (j3 then j1, newest first), never the foreign cluster's job (j2).
	resp, err = s.ListJobs(ctxWithIdentity(testIdentity("owen", auth.RoleViewer)), ListJobsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views := mustResponse[ListJobs200JSONResponse](t, resp)
	if len(views) != 2 || views[0].Id != "j3" || views[1].Id != "j1" {
		t.Fatalf("owner: views = %+v, want j3+j1 (newest first)", views)
	}

	// A project member sees their project's jobs only.
	resp, err = s.ListJobs(ctxWithIdentity(projectMember("pam", auth.RoleViewer, "proj-b")), ListJobsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	views = mustResponse[ListJobs200JSONResponse](t, resp)
	if len(views) != 1 || views[0].Id != "j2" {
		t.Fatalf("project member: views = %+v, want j2 only", views)
	}

	// Admin and auditor see the whole history.
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleAuditor} {
		resp, err := s.ListJobs(ctxWithIdentity(testIdentity("root", role)), ListJobsRequestObject{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if views := mustResponse[ListJobs200JSONResponse](t, resp); len(views) != 3 {
			t.Fatalf("role %v: got %d jobs, want 3", role, len(views))
		}
	}
}

// --- Gateway: the tenant denial is audited distinctly ---
//
// The decision table itself is pinned by
// TestGatewayAuthorizationIsProjectAndTargetScoped (rayjobs_test.go); this
// pins the audit shape of the tenant denial.
func TestGatewayTenantDenialAudited(t *testing.T) {
	buf := captureLogs(t)
	store := newMemStore(t)
	seedOwnedCluster(t, &Server{Store: store}, "c1", "proj-a", "dev1")
	endpoint := core.ClusterEndpoint{Id: "c1", Hostname: "c1.gw", ApiBaseUrl: "http://h:8265", Project: "proj-a", Target: core.RegistryTargetJobs}

	// Foreign developer: role check passes, tenant boundary refuses —
	// reason=foreign_cluster.
	r := httptest.NewRequest("POST", "/api/jobs/", nil)
	if err := authorizeGatewayRequest(store, testIdentity("dev2", auth.RoleDeveloper), r, endpoint); err != ErrForbidden {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	logged := buf.String()
	for _, want := range []string{"decision=deny", "reason=foreign_cluster", "method=POST", "path=/api/jobs/"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q, got: %s", want, logged)
		}
	}
}
