package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

func u32(v uint32) *uint32 { return &v }

func smallProfile(projects ...string) core.Profile {
	return core.Profile{
		Name: "small", Image: "rayproject/ray:2.9.0", RayVersion: "2.9.0", HeadCpu: "1", HeadMemory: "2Gi",
		WorkerGroups: []core.WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", MinReplicas: 1, MaxReplicas: 2, Replicas: 1}},
		MaxWorkers:   u32(2),
		Projects:     projects,
	}
}

func TestListProfilesRequiresReadOnCluster(t *testing.T) {
	s := &Server{Store: newMemStore(t), PolicySeed: PolicyConfig{Profiles: []core.Profile{smallProfile()}}}
	for _, tc := range []struct {
		id   *auth.Identity
		want int
	}{
		{testIdentity("admin", auth.RoleAdmin), http.StatusOK},
		{testIdentity("op", auth.RoleOperator), http.StatusOK},
		{testIdentity("dev", auth.RoleDeveloper), http.StatusOK},
		{testIdentity("viewer", auth.RoleViewer), http.StatusOK},
		{testIdentity("auditor", auth.RoleAuditor), http.StatusForbidden},
	} {
		resp, err := s.ListProfiles(ctxWithIdentity(tc.id), ListProfilesRequestObject{})
		if tc.want == http.StatusOK {
			if err != nil {
				t.Errorf("list_profiles as %v: %v", tc.id.Roles, err)
				continue
			}
			if got := mustResponse[ListProfiles200JSONResponse](t, resp); len(got) != 1 || got[0].Name != "small" {
				t.Errorf("list_profiles as %v = %+v, want [small]", tc.id.Roles, got)
			}
			continue
		}
		mustHTTPError(t, err, tc.want)
	}
}

func TestListProfilesIsNarrowedToTheCallersProjects(t *testing.T) {
	store := newMemStore(t)
	ctx := context.Background()
	if err := store.UpsertRoleAssignment(ctx, "dev", "operator", "project:team-a"); err != nil {
		t.Fatal(err)
	}
	open := smallProfile()
	open.Name = "open"
	s := &Server{Store: store, PolicySeed: PolicyConfig{Profiles: []core.Profile{
		smallProfile("team-a"), func() core.Profile { p := smallProfile("team-b"); p.Name = "b-only"; return p }(), open,
	}}}
	resp, err := s.ListProfiles(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), ListProfilesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range mustResponse[ListProfiles200JSONResponse](t, resp) {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "small,open" {
		t.Errorf("scoped dev sees %v, want [small open] (team-a's and the unrestricted one)", names)
	}
	resp, err = s.ListProfiles(ctxWithIdentity(testIdentity("root", auth.RoleAdmin)), ListProfilesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustResponse[ListProfiles200JSONResponse](t, resp); len(got) != 3 {
		t.Errorf("admin sees %d profiles, want all 3", len(got))
	}
}

func TestExpandProfileFillsEmptyAndRefusesConflicts(t *testing.T) {
	p := smallProfile("team-a")
	ttl := uint64(60)
	p.TtlSeconds = &ttl
	name := "small"

	spec := core.ClusterSpec{Name: "c", Project: "team-a", Profile: &name}
	if err := expandProfile(&spec, &p); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if spec.Image != p.Image || spec.RayVersion != p.RayVersion || spec.HeadCpu != "1" || spec.HeadMemory != "2Gi" || len(spec.WorkerGroups) != 1 {
		t.Errorf("expanded spec = %+v, want the profile's shape", spec)
	}
	if spec.TtlSeconds == nil || *spec.TtlSeconds != 60 {
		t.Errorf("ttl = %v, want the profile default 60", spec.TtlSeconds)
	}

	// A request's own ttl is kept: the profile's is a default, not a fix.
	own := uint64(5)
	spec = core.ClusterSpec{Project: "team-a", TtlSeconds: &own}
	if err := expandProfile(&spec, &p); err != nil || *spec.TtlSeconds != 5 {
		t.Errorf("own ttl: err=%v ttl=%v, want kept at 5", err, spec.TtlSeconds)
	}

	// Same value as the profile is not a conflict; a different one is.
	spec = core.ClusterSpec{Project: "team-a", Image: p.Image}
	if err := expandProfile(&spec, &p); err != nil {
		t.Errorf("matching image: %v", err)
	}
	spec = core.ClusterSpec{Project: "team-a", Image: "other:1"}
	err := expandProfile(&spec, &p)
	mustHTTPError(t, err, 400)
	if !strings.Contains(err.Error(), "fixes image") {
		t.Errorf("conflict message = %q", err.Error())
	}
	spec = core.ClusterSpec{Project: "team-a", WorkerGroups: []core.WorkerGroup{{Name: "x", Cpu: "1", Memory: "1Gi", MaxReplicas: 1, Replicas: 1}}}
	mustHTTPError(t, expandProfile(&spec, &p), 400)

	// Not available to another project.
	spec = core.ClusterSpec{Project: "team-b"}
	mustHTTPError(t, expandProfile(&spec, &p), 400)

	// The profile's max_workers caps what the request brings when the
	// profile itself has no worker groups.
	headOnly := smallProfile()
	headOnly.WorkerGroups = nil
	spec = core.ClusterSpec{Project: "team-a", WorkerGroups: []core.WorkerGroup{{Name: "x", Cpu: "1", Memory: "1Gi", MaxReplicas: 3, Replicas: 0}}}
	mustHTTPError(t, expandProfile(&spec, &headOnly), 400)
}

func TestCreateClusterWithProfile(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store, PolicySeed: PolicyConfig{Profiles: []core.Profile{smallProfile("team-a")}}}
	ctx := ctxWithIdentity(testIdentity("op", auth.RoleOperator))
	name := "small"

	body := CreateCluster{Id: "c1", Spec: ClusterSpec{Name: "c1", Project: "team-a", Profile: &name, WorkerGroups: []WorkerGroup{}}}
	if _, err := s.CreateCluster(ctx, CreateClusterRequestObject{Body: &body}); err != nil {
		t.Fatalf("create with profile: %v", err)
	}
	stored, err := store.Get(context.Background(), "c1")
	if err != nil || stored == nil {
		t.Fatalf("cluster not persisted: %v", err)
	}
	if stored.Spec.RayVersion != "2.9.0" || len(stored.Spec.WorkerGroups) != 1 || stored.Spec.Profile == nil {
		t.Errorf("stored spec = %+v, want the profile's shape and the profile name kept", stored.Spec)
	}

	missing := "huge"
	body = CreateCluster{Id: "c2", Spec: ClusterSpec{Name: "c2", Project: "team-a", Profile: &missing}}
	mustHTTPError(t, mustErr(s.CreateCluster(ctx, CreateClusterRequestObject{Body: &body})), 400)

	body = CreateCluster{Id: "c3", Spec: ClusterSpec{Name: "c3", Project: "team-b", Profile: &name}}
	mustHTTPError(t, mustErr(s.CreateCluster(ctx, CreateClusterRequestObject{Body: &body})), 400)
	if c, _ := store.Get(context.Background(), "c3"); c != nil {
		t.Error("a refused create must not be persisted")
	}
}

func mustErr(_ any, err error) error { return err }

func TestAdmissionForMergesStarAndProject(t *testing.T) {
	s := &Server{Store: newMemStore(t), PolicySeed: PolicyConfig{Admission: map[string]core.AdmissionRule{
		"*":      {AllowedImages: []string{"rayproject/"}, MaxWorkers: 4},
		"team-b": {AllowedImages: []string{"registry.example/"}},
		"team-c": {MaxWorkers: 1},
	}}}
	ctx := context.Background()
	cases := map[string]Admission{
		"team-a": {AllowedImagePrefixes: []string{"rayproject/"}, MaxWorkers: 4},
		"team-b": {AllowedImagePrefixes: []string{"registry.example/"}, MaxWorkers: 4},
		"team-c": {AllowedImagePrefixes: []string{"rayproject/"}, MaxWorkers: 1},
	}
	for project, want := range cases {
		got, err := s.admissionFor(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got.AllowedImagePrefixes, ",") != strings.Join(want.AllowedImagePrefixes, ",") || got.MaxWorkers != want.MaxWorkers {
			t.Errorf("admissionFor(%s) = %+v, want %+v", project, got, want)
		}
	}
	// No policy at all: unrestricted.
	empty := &Server{Store: newMemStore(t)}
	if got, err := empty.admissionFor(ctx, "x"); err != nil || len(got.AllowedImagePrefixes) != 0 || got.MaxWorkers != 0 {
		t.Errorf("admissionFor with no policy = %+v, %v", got, err)
	}
}

func TestSeedRulesTurnsFlagsIntoTheStarRule(t *testing.T) {
	if (Admission{}).SeedRules() != nil {
		t.Error("unset flags must seed nothing (an empty seed never materializes a row)")
	}
	rules := Admission{AllowedImagePrefixes: []string{"rayproject/"}, MaxWorkers: 2}.SeedRules()
	if r := rules["*"]; len(rules) != 1 || r.MaxWorkers != 2 || len(r.AllowedImages) != 1 {
		t.Errorf("SeedRules = %+v", rules)
	}
}

func TestUpdatePolicyProfilesAndAdmissionSections(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	ctx := ctxWithIdentity(admin())
	max := int32(2)
	projects := []string{"team-a"}
	good := ProfileSpec{Name: "small", Image: "rayproject/ray:2.9.0", RayVersion: "2.9.0", HeadCpu: "1", HeadMemory: "2Gi",
		WorkerGroups: []WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", MinReplicas: 0, MaxReplicas: 2, Replicas: 1}},
		MaxWorkers:   &max, Projects: &projects}
	images := []string{"registry.example/"}
	adm := map[string]AdmissionRule{"team-b": {AllowedImages: &images}}
	profiles := []ProfileSpec{good}
	resp, err := s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &profiles, Admission: &adm}})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	pv := mustResponse[UpdatePolicy200JSONResponse](t, resp)
	if pv.Profiles == nil || len(*pv.Profiles) != 1 || pv.Admission == nil || (*pv.Admission)["team-b"].AllowedImages == nil {
		t.Errorf("view after put = %+v", pv)
	}

	// An unrelated edit leaves both sections untouched.
	quotas := map[string]map[string]float64{"team-a": {"cpu": 9}}
	resp, err = s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Quotas: &quotas}})
	if err != nil {
		t.Fatal(err)
	}
	if pv = mustResponse[UpdatePolicy200JSONResponse](t, resp); len(*pv.Profiles) != 1 || len(*pv.Admission) != 1 {
		t.Errorf("quota edit disturbed profiles/admission: %+v", pv)
	}

	// Invalid catalogs are refused as a unit with a 400 naming the fault.
	for name, bad := range map[string][]ProfileSpec{
		"duplicate": {good, good},
		"quantity":  {func() ProfileSpec { p := good; p.Name = "q"; p.HeadCpu = "lots"; return p }()},
		"replicas": {func() ProfileSpec {
			p := good
			p.Name = "r"
			p.WorkerGroups = []WorkerGroup{{Name: "w", Cpu: "1", Memory: "1Gi", MinReplicas: 3, MaxReplicas: 2, Replicas: 3}}
			return p
		}()},
		"over cap":      {func() ProfileSpec { p := good; p.Name = "c"; one := int32(1); p.MaxWorkers = &one; return p }()},
		"empty project": {func() ProfileSpec { p := good; p.Name = "e"; pr := []string{""}; p.Projects = &pr; return p }()},
		"empty image":   {func() ProfileSpec { p := good; p.Name = "i"; p.Image = ""; return p }()},
		"empty name":    {func() ProfileSpec { p := good; p.Name = ""; return p }()},
	} {
		bad := bad
		_, err := s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &bad}})
		if err == nil {
			t.Errorf("%s: accepted, want 400", name)
			continue
		}
		mustHTTPError(t, err, 400)
	}
	neg := int32(-1)
	badAdm := map[string]AdmissionRule{"team-b": {MaxWorkers: &neg}}
	mustHTTPError(t, mustErr(s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Admission: &badAdm}})), 400)

	// Clearing: [] and {} empty the sections.
	none := []ProfileSpec{}
	noAdm := map[string]AdmissionRule{}
	resp, err = s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &none, Admission: &noAdm}})
	if err != nil {
		t.Fatal(err)
	}
	if pv = mustResponse[UpdatePolicy200JSONResponse](t, resp); len(*pv.Profiles) != 0 || len(*pv.Admission) != 0 {
		t.Errorf("clear left %+v", pv)
	}
}

func TestLoadProfilesValidatesTheSeedFile(t *testing.T) {
	got, err := LoadProfiles([]byte(`[{"name":"s","image":"rayproject/ray:2.9.0","ray_version":"2.9.0","head_cpu":"1","head_memory":"2Gi","worker_groups":[]}]`))
	if err != nil || len(got) != 1 || got[0].Name != "s" {
		t.Fatalf("LoadProfiles = %+v, %v", got, err)
	}
	if _, err := LoadProfiles([]byte(`[{"name":"s","image":"x","ray_version":"1","head_cpu":"lots","head_memory":"2Gi","worker_groups":[]}]`)); err == nil {
		t.Error("bad quantity accepted")
	}
	if _, err := LoadProfiles([]byte(`{`)); err == nil {
		t.Error("malformed JSON accepted")
	}
}
