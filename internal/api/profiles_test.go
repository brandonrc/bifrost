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
		WorkerGroups: []core.WorkerGroup{{Name: "w", Cpu: "1", Memory: "2Gi", MinReplicas: 1, MaxReplicas: 2, Replicas: 1}},
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
		// No global role, one project grant: the identity a Keycloak group
		// mapped to a project role produces, and the one the notebook panel
		// asks as. It was refused, and the panel had nothing to offer.
		{projectMember("pam", auth.RoleOperator, "team-a"), http.StatusOK},
		{testIdentity("nobody"), http.StatusForbidden},
		// Same gate as the cluster list, same answer for the auditor: reads
		// everything, mutates nothing. A catalog is not a secret.
		{testIdentity("auditor", auth.RoleAuditor), http.StatusOK},
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

func TestListProfilesForAProjectOnlyMemberIsTheirProjectsCatalog(t *testing.T) {
	// The self-service shape end to end: no global role, a project-scoped
	// operator grant, and a catalog with a profile for their project, one for
	// another, and one open to all. They see two, and never the third.
	forA, forB, forAll := smallProfile("team-a"), smallProfile("team-b"), smallProfile()
	forA.Name, forB.Name, forAll.Name = "for-a", "for-b", "for-all"
	s := &Server{Store: newMemStore(t), PolicySeed: PolicyConfig{Profiles: []core.Profile{forA, forB, forAll}}}

	resp, err := s.ListProfiles(ctxWithIdentity(projectMember("pam", auth.RoleOperator, "team-a")), ListProfilesRequestObject{})
	if err != nil {
		t.Fatalf("list_profiles as a project member: %v", err)
	}
	got := map[string]bool{}
	for _, p := range mustResponse[ListProfiles200JSONResponse](t, resp) {
		got[p.Name] = true
	}
	if !got["for-a"] || !got["for-all"] || got["for-b"] {
		t.Errorf("project member of team-a sees %v; want for-a and for-all, not for-b", got)
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
		WorkerGroups: []WorkerGroup{{Name: "w", Cpu: "1", Memory: "2Gi", MinReplicas: 0, MaxReplicas: 2, Replicas: 1}},
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

// A profile's storage is additive: what it names is attached ahead of the
// request's own names, a request cannot drop it, and naming the same entry
// twice (once each) is not a conflict. This is how a `checkmaite` profile
// gives every cluster started from the JupyterLab sidebar — which sends
// the profile name and an empty shape — the analytics volume without the
// notebook user knowing the catalog.
func TestExpandProfileAttachesTheProfilesStorage(t *testing.T) {
	p := smallProfile("team-a")
	p.Storage = []string{"analytics", "creds"}

	spec := core.ClusterSpec{Project: "team-a"}
	if err := expandProfile(&spec, &p); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got := strings.Join(spec.Storage, ","); got != "analytics,creds" {
		t.Errorf("storage = %q, want the profile's two entries", got)
	}

	spec = core.ClusterSpec{Project: "team-a", Storage: []string{"scratch", "analytics"}}
	if err := expandProfile(&spec, &p); err != nil {
		t.Fatalf("expand with own storage: %v", err)
	}
	if got := strings.Join(spec.Storage, ","); got != "analytics,creds,scratch" {
		t.Errorf("storage = %q, want profile entries first, the request's extra after, no duplicate", got)
	}

	// A profile without storage leaves the request's alone.
	none := smallProfile("team-a")
	spec = core.ClusterSpec{Project: "team-a", Storage: []string{"scratch"}}
	if err := expandProfile(&spec, &none); err != nil || strings.Join(spec.Storage, ",") != "scratch" {
		t.Errorf("no-storage profile: err=%v storage=%v", err, spec.Storage)
	}
}

// The catalog refuses a profile whose storage names nothing in the storage
// catalog — in the same request or the one already stored — so the fault
// is the administrator's 400 now, not a notebook user's 400 later. The
// view round-trips the field, and a cluster created from the profile
// resolves the storage against its project like a request's own.
func TestUpdatePolicyProfileStorageMustExistAndRoundTrips(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	ctx := ctxWithIdentity(admin())
	projects := []string{"team-a"}
	storage := []string{"analytics"}
	prof := ProfileSpec{Name: "checkmaite", Image: "checkmaite:1", RayVersion: "2.9.0", HeadCpu: "1", HeadMemory: "2Gi",
		WorkerGroups: []WorkerGroup{}, Projects: &projects, Storage: &storage}
	profiles := []ProfileSpec{prof}

	// No storage catalog yet: dangling.
	err := mustErr(s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &profiles}}))
	mustHTTPError(t, err, 400)
	if !strings.Contains(err.Error(), `no such storage "analytics"`) {
		t.Errorf("message = %q", err.Error())
	}

	// Same request carries the entry: fine, and the view shows it.
	entries := []StorageEntry{pvcEntry("analytics", "checkmaite-analytics", "/app/data/analytics")}
	resp, err := s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &profiles, Storage: &entries}})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	pv := mustResponse[UpdatePolicy200JSONResponse](t, resp)
	if pv.Profiles == nil || len(*pv.Profiles) != 1 || (*pv.Profiles)[0].Storage == nil || strings.Join(*(*pv.Profiles)[0].Storage, ",") != "analytics" {
		t.Errorf("view = %+v, want the profile's storage echoed", pv.Profiles)
	}

	// Removing the entry from the storage section while a stored profile
	// still names it is refused too.
	none := []StorageEntry{}
	mustHTTPError(t, mustErr(s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Storage: &none}})), 400)

	// Listed twice on one profile: refused.
	twice := []string{"analytics", "analytics"}
	dup := prof
	dup.Storage = &twice
	dupProfiles := []ProfileSpec{dup}
	mustHTTPError(t, mustErr(s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Profiles: &dupProfiles}})), 400)

	// A cluster from the profile, with an empty shape, carries the mount.
	name := "checkmaite"
	body := CreateCluster{Id: "c1", Spec: ClusterSpec{Name: "c1", Project: "team-a", Profile: &name, WorkerGroups: []WorkerGroup{}}}
	cresp, err := s.CreateCluster(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), CreateClusterRequestObject{Body: &body})
	if err != nil {
		t.Fatalf("create from profile: %v", err)
	}
	_ = cresp
	stored, err := s.Store.Get(ctx, core.ClusterId("c1"))
	if err != nil || stored == nil {
		t.Fatalf("stored: %v %v", stored, err)
	}
	if len(stored.Spec.StorageResolved) != 1 || stored.Spec.StorageResolved[0].Name != "analytics" || stored.Spec.StorageResolved[0].ClaimName != "checkmaite-analytics" {
		t.Errorf("resolved storage = %+v, want the analytics claim", stored.Spec.StorageResolved)
	}
}
