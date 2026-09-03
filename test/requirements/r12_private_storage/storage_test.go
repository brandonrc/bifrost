// Requirement 12 — private storage (S3 and the like) from the cluster, with
// credentials that reach pods through a secret reference and never through
// the spec or an API response.
//
// The shape (plan ruling D7, the Rust predecessor's pod-shaping rule): an administrator
// catalogs Kubernetes Secrets as named `storage` entries in the policy
// (`PUT /settings/policy`, section-replace), a spec lists names, and the
// provisioner projects them as `envFrom.secretRef` (env) or a read-only
// Secret volume (file). Bifrost never reads a Secret's data — the API
// carries names, the pods get references, the kubelet resolves them.
package r12_private_storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// setStorageCatalog PUTs the storage section as admin and restores the
// previous catalog when the test ends (the policy is platform state; a
// cluster target's own entries must survive). Returns the PUT response
// body so callers can scan it.
func setStorageCatalog(t *testing.T, tgt req.Target, entries []client.StorageEntry) []byte {
	t.Helper()
	ctx := context.Background()
	admin := tgt.As("admin").API()
	before, err := admin.GetPolicyWithResponse(ctx)
	if err != nil || before.JSON200 == nil {
		t.Fatalf("get_policy: err=%v status=%v body=%s", err, before.StatusCode(), before.Body)
	}
	r, err := admin.UpdatePolicyWithResponse(ctx, client.UpdatePolicyJSONRequestBody{Storage: &entries})
	if err != nil || r.StatusCode() != http.StatusOK {
		t.Fatalf("update_policy storage: err=%v status=%v body=%s", err, r.StatusCode(), r.Body)
	}
	t.Cleanup(func() {
		restore := []client.StorageEntry{}
		if before.JSON200.Storage != nil {
			restore = *before.JSON200.Storage
		}
		_, _ = admin.UpdatePolicyWithResponse(context.Background(), client.UpdatePolicyJSONRequestBody{Storage: &restore})
	})
	return r.Body
}

func envEntry(name, secret string, projects ...string) client.StorageEntry {
	return client.StorageEntry{Name: name, SecretName: secret, Mode: client.Env, Projects: &projects}
}

func fileEntry(name, secret, mount string, projects ...string) client.StorageEntry {
	return client.StorageEntry{Name: name, SecretName: secret, Mode: client.File, MountPath: &mount, Projects: &projects}
}

// createWithStorage posts the canonical cluster body with storage names
// and returns (status, body).
func createWithStorage(t *testing.T, tgt req.Target, principal, id, project string, storage ...string) (int, []byte) {
	t.Helper()
	body := fixture.ClusterBody(id, project, nil)
	body.Spec.Storage = &storage
	resp, err := tgt.As(principal).API().CreateClusterWithResponse(context.Background(), body)
	if err != nil {
		t.Fatalf("create %s as %s: %v", id, principal, err)
	}
	if resp.StatusCode() == http.StatusCreated {
		t.Cleanup(func() { fixture.Delete(t, tgt, "admin", id) })
	}
	return resp.StatusCode(), resp.Body
}

func TestUnknownStorageRefIsRefused(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 12, "a storage name nothing catalogued is a 400 at create, never a cluster that silently runs without its credentials")
	id := req.Name("stor")
	st, body := createWithStorage(t, tgt, "admin", id, "team-a", req.Name("nosuchstorage"))
	if st != http.StatusBadRequest {
		t.Fatalf("create with an unknown storage entry = %d %s, want 400", st, body)
	}
	if got, _ := fixture.Get(t, tgt, "admin", id); got != http.StatusNotFound {
		t.Fatalf("a refused create must persist nothing; get = %d", got)
	}
}

func TestStorageRefOutsideProjectIsRefused(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 12, "a storage entry scoped to another project is a 400 for this one; the same entry is accepted by a project it is open to")
	name := req.Name("s3-b")
	setStorageCatalog(t, tgt, []client.StorageEntry{envEntry(name, req.Name("s3-b-creds"), "team-b")})

	id := req.Name("stora")
	st, body := createWithStorage(t, tgt, "dev-a", id, "team-a", name)
	if st != http.StatusBadRequest {
		t.Fatalf("dev-a referencing team-b's storage = %d %s, want 400", st, body)
	}
	if !strings.Contains(string(body), "not available to project") {
		t.Errorf("refusal must name the reason, got %s", body)
	}
	if got, _ := fixture.Get(t, tgt, "admin", id); got != http.StatusNotFound {
		t.Fatalf("a refused create must persist nothing; get = %d", got)
	}
	// The same name is accepted by the project it is open to.
	idB := req.Name("storb")
	if st, body := createWithStorage(t, tgt, "dev-b", idB, "team-b", name); st != http.StatusCreated {
		t.Fatalf("dev-b referencing its own storage = %d %s, want 201", st, body)
	}
}

func TestSecretValuesNeverAppearInResponses(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 12, "the policy view carries catalog names and Secret names only, and a cluster view never carries the resolution")
	ctx := context.Background()
	name := req.Name("s3-a")
	secret := req.Name("s3-a-creds")
	put := setStorageCatalog(t, tgt, []client.StorageEntry{envEntry(name, secret, "team-a")})

	var view struct {
		Storage []map[string]any `json:"storage"`
	}
	if err := json.Unmarshal(put, &view); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"name": true, "secret_name": true, "mode": true, "mount_path": true, "projects": true}
	found := false
	for _, e := range view.Storage {
		if e["name"] == name {
			found = true
		}
		for k := range e {
			if !allowed[k] {
				t.Errorf("policy storage entry carries %q; only names and delivery instructions may be on the wire (%v)", k, e)
			}
		}
	}
	if !found {
		t.Fatalf("PUT response does not list %s: %s", name, put)
	}

	id := req.Name("storv")
	st, created := createWithStorage(t, tgt, "dev-a", id, "team-a", name)
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", st, created)
	}
	got, err := tgt.As("dev-a").API().GetClusterWithResponse(ctx, id)
	if err != nil || got.StatusCode() != http.StatusOK {
		t.Fatalf("get: err=%v status=%v", err, got.StatusCode())
	}
	list, err := tgt.As("dev-a").API().ListClustersWithResponse(ctx)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list: err=%v status=%v", err, list.StatusCode())
	}
	for what, body := range map[string][]byte{"create": created, "get": got.Body, "list": list.Body} {
		if strings.Contains(string(body), "storage_resolved") || strings.Contains(string(body), secret) {
			t.Errorf("%s response carries the storage resolution: %s", what, body)
		}
	}
}

// secretValue is a random token no fixture could contain: its absence from
// every response is what proves the credentials never crossed Bifrost.
func secretValue(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "bifrost-r12-" + hex.EncodeToString(b[:])
}

// createSecret creates a run-labelled Secret in the workload namespace and
// deletes it when the test ends (postflight sweeps leftovers).
func createSecret(t *testing.T, tgt req.Target, name string, data map[string]string) {
	t.Helper()
	k, _ := tgt.K8s()
	ctx := context.Background()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tgt.Namespace(), Labels: map[string]string{req.RunLabel: req.RunID()}},
		StringData: data,
	}
	if err := k.Create(ctx, sec); err != nil {
		t.Fatalf("create secret %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k.Delete(context.Background(), sec) })
}

// headPod returns the cluster's head pod once it exists.
func headPod(t *testing.T, tgt req.Target, id string) corev1.Pod {
	t.Helper()
	k, _ := tgt.K8s()
	var head corev1.Pod
	req.Eventually(t, tgt, func() (bool, string) {
		var pods corev1.PodList
		if err := k.List(context.Background(), &pods, ctrlclient.InNamespace(tgt.Namespace()),
			ctrlclient.MatchingLabels{"ray.io/cluster": id, "ray.io/node-type": "head"}); err != nil {
			return false, err.Error()
		}
		if len(pods.Items) != 1 {
			return false, fmt.Sprintf("%d head pods", len(pods.Items))
		}
		head = pods.Items[0]
		return true, "head pod present"
	})
	return head
}

// scanForValue fails if value appears in any collected body or in the
// audit trail.
func scanForValue(t *testing.T, tgt req.Target, value string, bodies map[string][]byte) {
	t.Helper()
	audit, err := tgt.As("admin").API().ListAuditEventsWithResponse(context.Background(), nil)
	if err != nil || audit.StatusCode() != http.StatusOK {
		t.Fatalf("list_audit_events: err=%v status=%v", err, audit.StatusCode())
	}
	bodies["list_audit_events"] = audit.Body
	for what, body := range bodies {
		if strings.Contains(string(body), value) {
			t.Errorf("%s response carries the secret VALUE: credentials crossed the API", what)
		}
	}
}

func TestStorageRefReachesPodsAsSecretRefNeverAsValue(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 12, "an env-mode storage entry reaches the head pod as envFrom.secretRef naming the Secret; its value appears in no API response and no audit row")
	req.NeedK8s(t, tgt)
	ctx := context.Background()
	value := secretValue(t)
	secret := req.Name("s3-creds")
	createSecret(t, tgt, secret, map[string]string{"AWS_SECRET_ACCESS_KEY": value})
	name := req.Name("s3-a")
	bodies := map[string][]byte{"update_policy": setStorageCatalog(t, tgt, []client.StorageEntry{envEntry(name, secret, "team-a")})}

	id := req.Name("store")
	st, created := createWithStorage(t, tgt, "dev-a", id, "team-a", name)
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", st, created)
	}
	bodies["create_cluster"] = created
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	got, err := tgt.As("dev-a").API().GetClusterWithResponse(ctx, id)
	if err != nil || got.StatusCode() != http.StatusOK {
		t.Fatalf("get: err=%v status=%v", err, got.StatusCode())
	}
	bodies["get_cluster"] = got.Body
	if pol, err := tgt.As("admin").API().GetPolicyWithResponse(ctx); err == nil {
		bodies["get_policy"] = pol.Body
	}

	head := headPod(t, tgt, id)
	c := head.Spec.Containers[0]
	if len(c.EnvFrom) == 0 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != secret {
		t.Fatalf("head pod envFrom = %+v, want [0].secretRef.name == %s", c.EnvFrom, secret)
	}
	for _, e := range c.Env {
		if e.Value == value {
			t.Fatalf("head pod carries the secret value inline in env %s; it must arrive by reference", e.Name)
		}
	}
	raw, _ := json.Marshal(head)
	if strings.Contains(string(raw), value) {
		t.Fatal("the pod manifest carries the secret value; it must only name the Secret")
	}
	scanForValue(t, tgt, value, bodies)
}

func TestFileModeMountsAtPath(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 12, "a file-mode storage entry mounts the Secret read-only at the catalogued path on the head pod, by reference")
	req.NeedK8s(t, tgt)
	value := secretValue(t)
	secret := req.Name("gcs-key")
	createSecret(t, tgt, secret, map[string]string{"key.json": value})
	name := req.Name("gcs")
	mount := "/opt/bifrost-r12"
	bodies := map[string][]byte{"update_policy": setStorageCatalog(t, tgt, []client.StorageEntry{fileEntry(name, secret, mount, "team-a")})}

	id := req.Name("storf")
	st, created := createWithStorage(t, tgt, "dev-a", id, "team-a", name)
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", st, created)
	}
	bodies["create_cluster"] = created
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")

	head := headPod(t, tgt, id)
	var volume string
	for _, v := range head.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secret {
			volume = v.Name
		}
	}
	if volume == "" {
		t.Fatalf("head pod has no Secret volume for %s: %+v", secret, head.Spec.Volumes)
	}
	mounted := false
	for _, m := range head.Spec.Containers[0].VolumeMounts {
		if m.Name == volume {
			mounted = true
			if m.MountPath != mount || !m.ReadOnly {
				t.Errorf("mount = %+v, want read-only at %s", m, mount)
			}
		}
	}
	if !mounted {
		t.Fatalf("head container does not mount volume %s: %+v", volume, head.Spec.Containers[0].VolumeMounts)
	}
	scanForValue(t, tgt, value, bodies)
}
