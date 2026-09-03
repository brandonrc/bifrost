package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

func envEntry(name, secret string, projects ...string) StorageEntry {
	e := StorageEntry{Name: name, SecretName: secret, Mode: Env}
	if projects != nil {
		e.Projects = &projects
	}
	return e
}

func fileEntry(name, secret, mount string) StorageEntry {
	return StorageEntry{Name: name, SecretName: secret, Mode: File, MountPath: &mount}
}

func putStorage(t *testing.T, s *Server, entries []StorageEntry) (PolicyView, error) {
	t.Helper()
	resp, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Storage: &entries}})
	if err != nil {
		return PolicyView{}, err
	}
	return PolicyView(mustResponse[UpdatePolicy200JSONResponse](t, resp)), nil
}

func TestUpdatePolicyStorageSectionReplaceAndValidation(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	ctx := ctxWithIdentity(admin())

	pv, err := putStorage(t, s, []StorageEntry{envEntry("s3-a", "s3-a-creds", "team-a"), fileEntry("gcs", "gcs-key", "/opt/gcs")})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if pv.Storage == nil || len(*pv.Storage) != 2 {
		t.Fatalf("view after put = %+v", pv.Storage)
	}

	// An unrelated edit leaves the catalog untouched; [] clears it.
	quotas := map[string]map[string]float64{"team-a": {"cpu": 9}}
	resp, err := s.UpdatePolicy(ctx, UpdatePolicyRequestObject{Body: &UpdatePolicy{Quotas: &quotas}})
	if err != nil {
		t.Fatal(err)
	}
	if pv := mustResponse[UpdatePolicy200JSONResponse](t, resp); len(*pv.Storage) != 2 {
		t.Errorf("quota edit disturbed storage: %+v", pv.Storage)
	}
	if pv, err = putStorage(t, s, []StorageEntry{}); err != nil || len(*pv.Storage) != 0 {
		t.Errorf("clear: err=%v storage=%+v", err, pv.Storage)
	}

	// Invalid catalogs are refused as a unit with a 400 naming the fault.
	for name, bad := range map[string][]StorageEntry{
		"duplicate name":       {envEntry("a", "x"), envEntry("a", "y")},
		"empty name":           {envEntry("", "x")},
		"uppercase name":       {envEntry("S3", "x")},
		"dotted name":          {envEntry("s3.a", "x")},
		"bad secret name":      {envEntry("a", "Not_A_Secret")},
		"empty secret name":    {envEntry("a", "")},
		"file without mount":   {{Name: "a", SecretName: "x", Mode: File}},
		"env with mount":       {fileEntryMode("a", "x", "/opt/a", Env)},
		"relative mount":       {fileEntry("a", "x", "opt/a")},
		"root mount":           {fileEntry("a", "x", "/")},
		"tmp mount":            {fileEntry("a", "x", "/tmp")},
		"under tmp":            {fileEntry("a", "x", "/tmp/creds")},
		"ray home":             {fileEntry("a", "x", "/home/ray/")},
		"under ray home":       {fileEntry("a", "x", "/home/ray/.aws")},
		"duplicate mount path": {fileEntry("a", "x", "/opt/creds"), fileEntry("b", "y", "/opt/creds/")},
		"unknown mode":         {{Name: "a", SecretName: "x", Mode: "sidecar"}},
		"empty project":        {envEntry("a", "x", "")},
	} {
		if _, err := putStorage(t, s, bad); err == nil {
			t.Errorf("%s: accepted, want 400", name)
		} else {
			mustHTTPError(t, err, 400)
		}
	}
	// Paths that merely share a prefix string with a reserved path are fine.
	if _, err := putStorage(t, s, []StorageEntry{fileEntry("a", "x", "/tmpfs"), fileEntry("b", "y", "/home/rayuser")}); err != nil {
		t.Errorf("/tmpfs and /home/rayuser are not reserved: %v", err)
	}
}

func fileEntryMode(name, secret, mount string, mode StorageEntryMode) StorageEntry {
	e := fileEntry(name, secret, mount)
	e.Mode = mode
	return e
}

func TestPolicyViewStorageCarriesNamesOnly(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := putStorage(t, s, []StorageEntry{envEntry("s3-a", "s3-a-creds", "team-a")}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.GetPolicy(ctxWithIdentity(admin()), GetPolicyRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(mustResponse[GetPolicy200JSONResponse](t, resp))
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Storage []map[string]any `json:"storage"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Storage) != 1 {
		t.Fatalf("storage = %v", view.Storage)
	}
	allowed := map[string]bool{"name": true, "secret_name": true, "mode": true, "mount_path": true, "projects": true}
	for k := range view.Storage[0] {
		if !allowed[k] {
			t.Errorf("PolicyView.storage carries %q; only names and delivery instructions may be on the wire", k)
		}
	}
}

func TestResolveStorageChecksCatalogAndProject(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := putStorage(t, s, []StorageEntry{
		envEntry("s3-a", "s3-a-creds", "team-a"),
		fileEntry("shared", "shared-key", "/opt/shared"),
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := s.resolveStorage(ctx, "team-a", []string{"shared", "s3-a"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 2 || got[0].Name != "shared" || got[0].Mode != core.StorageModeFile || got[0].MountPath == nil || *got[0].MountPath != "/opt/shared" ||
		got[1].SecretName != "s3-a-creds" || got[1].Mode != core.StorageModeEnv || got[1].MountPath != nil {
		t.Errorf("resolved = %+v", got)
	}
	if got, err := s.resolveStorage(ctx, "team-b", nil); err != nil || got != nil {
		t.Errorf("no names must resolve to nil: %v %v", got, err)
	}
	for name, c := range map[string]struct {
		project string
		names   []string
		want    string
	}{
		"unknown":       {"team-a", []string{"nope"}, `no such storage "nope"`},
		"other project": {"team-b", []string{"s3-a"}, `not available to project "team-b"`},
		"duplicate":     {"team-a", []string{"shared", "shared"}, "listed twice"},
	} {
		_, err := s.resolveStorage(ctx, c.project, c.names)
		if err == nil {
			t.Errorf("%s: resolved, want 400", name)
			continue
		}
		mustHTTPError(t, err, 400)
		if !strings.Contains(httpMessage(err), c.want) {
			t.Errorf("%s: message %q, want it to contain %q", name, httpMessage(err), c.want)
		}
	}
	// "shared" is open to every project.
	if _, err := s.resolveStorage(ctx, "team-b", []string{"shared"}); err != nil {
		t.Errorf("an entry with no projects is open to every project: %v", err)
	}
}

func TestCreateClusterResolvesStorageAndRefusesForeignRefs(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	if _, err := putStorage(t, s, []StorageEntry{envEntry("s3-a", "s3-a-creds", "team-a")}); err != nil {
		t.Fatal(err)
	}
	ctx := ctxWithIdentity(testIdentity("op", auth.RoleOperator))
	body := func(id, project string, storage ...string) CreateCluster {
		return CreateCluster{Id: id, Spec: ClusterSpec{Name: id, Project: project, RayVersion: "2.9.0", Image: "rayproject/ray:2.9.0",
			HeadCpu: "1", HeadMemory: "2Gi", WorkerGroups: []WorkerGroup{}, Storage: &storage}}
	}
	b := body("c1", "team-a", "s3-a")
	resp, err := s.CreateCluster(ctx, CreateClusterRequestObject{Body: &b})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := store.Get(context.Background(), "c1")
	if err != nil || stored == nil {
		t.Fatalf("not persisted: %v", err)
	}
	if len(stored.Spec.StorageResolved) != 1 || stored.Spec.StorageResolved[0].SecretName != "s3-a-creds" {
		t.Errorf("stored resolution = %+v", stored.Spec.StorageResolved)
	}
	// The response echoes the names only — never the Secret's name nor the
	// resolution.
	raw, _ := json.Marshal(resp)
	if strings.Contains(string(raw), "s3-a-creds") || strings.Contains(string(raw), "storage_resolved") {
		t.Errorf("create response leaks the resolution: %s", raw)
	}

	for name, b := range map[string]CreateCluster{
		"unknown": body("c2", "team-a", "nope"),
		"foreign": body("c3", "team-b", "s3-a"),
	} {
		b := b
		mustHTTPError(t, mustErr(s.CreateCluster(ctx, CreateClusterRequestObject{Body: &b})), 400)
		if c, _ := store.Get(context.Background(), core.ClusterId(b.Id)); c != nil {
			t.Errorf("%s: a refused create must not be persisted", name)
		}
	}
	events, _, err := store.ListAudit(context.Background(), core.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, e := range events {
		if e.Event.Reason != nil && *e.Event.Reason == "storage_rejected" {
			denied++
		}
	}
	if denied != 2 {
		t.Errorf("storage_rejected audit rows = %d, want 2", denied)
	}
}

func TestDeployServiceResolvesStorage(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	if _, err := putStorage(t, s, []StorageEntry{fileEntry("models", "model-bucket-key", "/opt/models")}); err != nil {
		t.Fatal(err)
	}
	id := admin()
	spec := minimalServiceSpec()
	spec.Storage = &[]string{"models"}
	if err := deployAs(t, s, id, "svc-a", spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	svc, err := store.GetService(context.Background(), "svc-a")
	if err != nil || svc == nil {
		t.Fatalf("service not persisted: %v", err)
	}
	if len(svc.Spec.StorageResolved) != 1 || svc.Spec.StorageResolved[0].MountPath == nil || *svc.Spec.StorageResolved[0].MountPath != "/opt/models" {
		t.Errorf("stored resolution = %+v", svc.Spec.StorageResolved)
	}
	spec.Storage = &[]string{"nope"}
	mustHTTPError(t, deployAs(t, s, id, "svc-b", spec), 400)
	if svc, _ := store.GetService(context.Background(), "svc-b"); svc != nil {
		t.Error("a refused deploy must not be persisted")
	}
}
