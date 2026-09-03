// Profile catalog (requirement 7, plan rulings D4/D7): administrator-
// defined named cluster shapes users pick by name. The catalog rides the
// policy row (settings.go: PUT /settings/policy `profiles` section
// validates it as a unit); this file is the read side (list_profiles)
// and the expansion a create/submit runs when a spec names a profile.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
)

// ListProfiles lists the profiles the caller may use: Read on cluster
// (the same rule as the other catalog reads), narrowed to the caller's
// projects by readScope — a project-scoped caller sees the profiles open
// to every project plus those naming one of theirs; admins and global
// roles see the whole catalog.
func (s *Server) ListProfiles(ctx context.Context, _ ListProfilesRequestObject) (ListProfilesResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetCluster); err != nil {
		return nil, err
	}
	p, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	out := make([]ProfileSpec, 0)
	if p == nil {
		return ListProfiles200JSONResponse(out), nil
	}
	_, projects := readScope(ctx, s.Store, identity)
	for i := range p.Profiles {
		prof := &p.Profiles[i]
		if len(projects) > 0 && !profileOpenToAny(prof, projects) {
			continue
		}
		out = append(out, profileToWire(prof))
	}
	return ListProfiles200JSONResponse(out), nil
}

// profileAvailableTo reports whether project may use p: every project when
// p.Projects is empty, else only the listed ones.
func profileAvailableTo(p *core.Profile, project string) bool {
	return len(p.Projects) == 0 || containsString(p.Projects, project)
}

func profileOpenToAny(p *core.Profile, projects []string) bool {
	if len(p.Projects) == 0 {
		return true
	}
	for _, proj := range projects {
		if containsString(p.Projects, proj) {
			return true
		}
	}
	return false
}

// resolveProfile looks up *spec.Profile in the effective catalog and
// expands it into spec (expandProfile). The entry point create_cluster —
// and package B's submit_job — call when a spec names a profile: 400 for
// an unknown name, a profile the spec's project may not use, or a
// conflicting field; a store failure comes back wrapped for the caller.
func (s *Server) resolveProfile(ctx context.Context, spec *core.ClusterSpec) error {
	if spec.Profile == nil {
		return nil
	}
	p, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return wrapStoreErr(err)
	}
	if p != nil {
		for i := range p.Profiles {
			if p.Profiles[i].Name == *spec.Profile {
				return expandProfile(spec, &p.Profiles[i])
			}
		}
	}
	return badRequest(fmt.Sprintf("no such profile %q", *spec.Profile))
}

// expandProfile fills spec's zero-valued shape fields from p and refuses
// a non-empty one that p also fixes (plan ruling D4): a profile is the
// administrator's shape, so a client either takes it whole or leaves the
// profile out. Image, ray_version, head_cpu, head_memory and
// worker_groups are fixed; ttl_seconds and idle_timeout_secs are the
// profile's defaults, which a request may override with its own value.
// p.MaxWorkers caps the expanded spec's total max_replicas; p.Projects
// gates which project may use it. Every refusal is a 400.
func expandProfile(spec *core.ClusterSpec, p *core.Profile) error {
	if !profileAvailableTo(p, spec.Project) {
		return badRequest(fmt.Sprintf("profile %q is not available to project %q", p.Name, spec.Project))
	}
	conflict := func(field string) error {
		return badRequest(fmt.Sprintf("profile %q fixes %s; leave it empty or omit the profile", p.Name, field))
	}
	fill := func(dst *string, src, field string) error {
		switch {
		case *dst == "":
			*dst = src
		case src != "" && *dst != src:
			return conflict(field)
		}
		return nil
	}
	if err := fill(&spec.Image, p.Image, "image"); err != nil {
		return err
	}
	if err := fill(&spec.RayVersion, p.RayVersion, "ray_version"); err != nil {
		return err
	}
	if err := fill(&spec.HeadCpu, p.HeadCpu, "head_cpu"); err != nil {
		return err
	}
	if err := fill(&spec.HeadMemory, p.HeadMemory, "head_memory"); err != nil {
		return err
	}
	switch {
	case len(spec.WorkerGroups) == 0:
		spec.WorkerGroups = append([]core.WorkerGroup(nil), p.WorkerGroups...)
	case len(p.WorkerGroups) > 0:
		return conflict("worker_groups")
	}
	if spec.TtlSeconds == nil && p.TtlSeconds != nil {
		v := *p.TtlSeconds
		spec.TtlSeconds = &v
	}
	if spec.IdleTimeoutSecs == nil && p.IdleTimeoutSecs != nil {
		v := *p.IdleTimeoutSecs
		spec.IdleTimeoutSecs = &v
	}
	if p.MaxWorkers != nil && *p.MaxWorkers > 0 {
		if total := totalMaxWorkers(spec); total > int(*p.MaxWorkers) {
			return badRequest(fmt.Sprintf("worker groups ask for up to %d workers; profile %q caps them at %d", total, p.Name, *p.MaxWorkers))
		}
	}
	return nil
}

// LoadProfiles parses a `--profiles` seed file — a JSON list of ProfileSpec
// objects, the same shape PUT /settings/policy takes — and validates it
// exactly as the API would, so a bad seed fails at boot with the message
// the API would have given rather than at the first create.
func LoadProfiles(data []byte) ([]core.Profile, error) {
	var specs []ProfileSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("parsing profile catalog: %w", err)
	}
	profiles, err := profilesFromWire(specs)
	if err != nil {
		return nil, errors.New(httpMessage(err))
	}
	return profiles, nil
}
