package api

import (
	"context"

	"github.com/brandonrc/bifrost/internal/auth"
)

// This file holds the 501 stubs for the operations the 0.2.0 contract adds
// ahead of their handlers (build-out plan package A1): submit_job, get_job,
// delete_job (package B, `rayjobs.go`); list_profiles has moved to
// profiles.go (package F). Each
// stub runs the SAME authorization the real handler will, then answers
// ErrNotImplemented, so r03's TestPermissionMatrix already pins the
// per-role outcome: 501 is "not 401/403", so an `allow` row passes, while
// `deny` and `scoped` rows need the genuine check. Replace a stub with its
// handler in the package that implements the behaviour; keep the
// authorization helpers below, they ARE the rule.

// authorizeJobInProject is the write-side authorization for a job in
// project (#5 rule: jobs are "code" — Developer/Admin write, Operator
// read, project-scoped). It composes the two rules cluster routes already
// apply separately: a caller holding project-scoped assignments is
// narrowed to those projects (readScope's pinned edge case — the scoped
// binding defines where they operate, even though a Developer's global
// role grants Write on jobs everywhere), and within scope the ordinary
// AuthorizeScoped check applies.
func (s *Server) authorizeJobInProject(ctx context.Context, identity *auth.Identity, action auth.PermissionType, project string) error {
	_, narrowed := readScope(ctx, s.Store, identity)
	if len(narrowed) > 0 && !containsString(narrowed, project) {
		emitAuthzDenial(ctx, s.Store, identity, action, auth.TargetJob)
		return ErrForbidden
	}
	return AuthorizeScoped(ctx, s.Store, identity, action, auth.TargetJob, project)
}

// authorizeJobByID is the authorization step for a job addressed by id
// before its row is loaded (get_job/delete_job). It grants when the
// caller's global roles permit action on jobs OR any effective assignment
// does (on some project — which one is settled against the row's project
// once it exists, exactly as scopeForRead does for clusters); otherwise
// it is the audited 403. The returned flag reports whether the caller is
// project-narrowed (readScope): a narrowed caller must never learn that an
// out-of-scope id exists, so ids outside their projects read as 404.
func (s *Server) authorizeJobByID(ctx context.Context, identity *auth.Identity, action auth.PermissionType) (narrowed bool, err error) {
	if identity == nil {
		return false, nil
	}
	assignments, projects := readScope(ctx, s.Store, identity)
	if identity.Permits(action, auth.TargetJob) {
		return len(projects) > 0, nil
	}
	for _, a := range assignments {
		if a.Role.Grants(action, auth.TargetJob) {
			return len(projects) > 0, nil
		}
	}
	emitAuthzDenial(ctx, s.Store, identity, action, auth.TargetJob)
	return false, ErrForbidden
}

// SubmitJob (package B): Write on job in the spec's project.
func (s *Server) SubmitJob(ctx context.Context, req SubmitJobRequestObject) (SubmitJobResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := s.authorizeJobInProject(ctx, identity, auth.Write, req.Body.Spec.Project); err != nil {
		return nil, err
	}
	return nil, ErrNotImplemented
}

// GetJob (package B): Read on job. There is no job row yet, so a
// project-narrowed caller gets the 404 an out-of-scope id will always get;
// everyone else authorized gets the 501.
func (s *Server) GetJob(ctx context.Context, _ GetJobRequestObject) (GetJobResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	narrowed, err := s.authorizeJobByID(ctx, identity, auth.Read)
	if err != nil {
		return nil, err
	}
	if narrowed {
		return nil, notFound("no such job")
	}
	return nil, ErrNotImplemented
}

// DeleteJob (package B): Write on job, same visibility rule as GetJob.
func (s *Server) DeleteJob(ctx context.Context, _ DeleteJobRequestObject) (DeleteJobResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	narrowed, err := s.authorizeJobByID(ctx, identity, auth.Write)
	if err != nil {
		return nil, err
	}
	if narrowed {
		return nil, notFound("no such job")
	}
	return nil, ErrNotImplemented
}
