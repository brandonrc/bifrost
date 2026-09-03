package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
)

// The A1 stubs answer 501 only AFTER the authorization the real handlers
// will run; these pin the per-role outcome so replacing a stub with its
// handler (packages B and F) cannot loosen the rule unnoticed. r03's
// TestPermissionMatrix asserts the same thing at the wire against
// permissions.yaml; this is the unit-level, store-free version.
func TestJobStubsAuthorizeBeforeAnswering501(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	projectDev := func(project string) *auth.Identity {
		id := testIdentity("dev-"+project, auth.RoleDeveloper)
		id.ProjectRoles = []auth.RoleScope{{Role: auth.RoleOperator, Scope: "project:" + project}}
		return id
	}
	submit := func(id *auth.Identity, project string) error {
		body := SubmitJobJSONRequestBody{Spec: RayJobSpec{Project: project, Entrypoint: "python -c 1", Image: "rayproject/ray:2.56.0"}}
		_, err := s.SubmitJob(ctxWithIdentity(id), SubmitJobRequestObject{Body: &body})
		return err
	}
	get := func(id *auth.Identity) error {
		_, err := s.GetJob(ctxWithIdentity(id), GetJobRequestObject{Id: "job-1"})
		return err
	}
	del := func(id *auth.Identity) error {
		_, err := s.DeleteJob(ctxWithIdentity(id), DeleteJobRequestObject{Id: "job-1"})
		return err
	}

	admin := testIdentity("admin", auth.RoleAdmin)
	operator := testIdentity("op", auth.RoleOperator)
	viewer := testIdentity("viewer", auth.RoleViewer)
	devA := projectDev("team-a")

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"admin submit", submit(admin, "team-a"), http.StatusNotImplemented},
		{"admin get", get(admin), http.StatusNotImplemented},
		{"admin delete", del(admin), http.StatusNotImplemented},
		{"operator submit (jobs are code)", submit(operator, "team-a"), http.StatusForbidden},
		{"operator get (read)", get(operator), http.StatusNotImplemented},
		{"operator delete", del(operator), http.StatusForbidden},
		{"viewer submit", submit(viewer, "team-a"), http.StatusForbidden},
		{"viewer get", get(viewer), http.StatusNotImplemented},
		{"viewer delete", del(viewer), http.StatusForbidden},
		{"project dev submits into own project", submit(devA, "team-a"), http.StatusNotImplemented},
		{"project dev submits into another project", submit(devA, "team-b"), http.StatusForbidden},
		{"project dev get: narrowed, no row can be in scope", get(devA), http.StatusNotFound},
		{"project dev delete: narrowed, no row can be in scope", del(devA), http.StatusNotFound},
	}
	for _, tc := range cases {
		if got := statusOf(t, tc.err); got != tc.want {
			t.Errorf("%s = %d (%v), want %d", tc.name, got, tc.err, tc.want)
		}
	}

	// Dev mode (no identity) is unrestricted, exactly like Authorize.
	if _, err := s.GetJob(ctxWithIdentity(nil), GetJobRequestObject{Id: "job-1"}); statusOf(t, err) != http.StatusNotImplemented {
		t.Errorf("get without identity = %v, want 501", err)
	}
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var he HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("error %v is not an HTTPError", err)
	}
	return he.Status
}
