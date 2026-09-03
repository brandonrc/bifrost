package live

import (
	"context"
	"errors"
	"strings"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

// fakePatchClient is a minimal client.Client stub for applySSA's unit
// test: it captures the object and patch passed to Patch. Embeds the
// (nil) client.Client interface so only Patch needs a real
// implementation — applySSA calls nothing else on the client it's given.
type fakePatchClient struct {
	client.Client
	gotObj   client.Object
	gotPatch client.Patch
}

func (f *fakePatchClient) Patch(_ context.Context, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
	f.gotObj = obj
	f.gotPatch = patch
	return nil
}

// TestApplySSAZeroesStatusBeforePatching is the ledgered requirement,
// verified at the structural enforcement point (fix round 1, M1):
// applySSA is the ONLY call site in this package that invokes
// client.Patch with client.Apply, so a status-populated object handed to
// it must come back zeroed BEFORE the Patch call — proving the invariant
// holds unconditionally, including for object kinds (e.g. ResourceFlavor,
// applied from ApplyPool's flavor loop) that never call
// provision.ZeroStatus themselves.
func TestApplySSAZeroesStatusBeforePatching(t *testing.T) {
	rc := &rayv1.RayCluster{
		Status: rayv1.RayClusterStatus{
			State: "ready", //nolint:staticcheck // SA1019: populated on purpose to prove applySSA clears it
		},
	}
	fake := &fakePatchClient{}
	if err := applySSA(context.Background(), fake, rc, client.FieldOwner("bifrost")); err != nil {
		t.Fatalf("applySSA: %v", err)
	}
	if fake.gotPatch != client.Apply { //nolint:staticcheck // SA1019: client.Apply is the Patch type under test, not the newer typed c.Apply() API — see applySSA's doc comment
		t.Fatalf("patch = %v, want client.Apply", fake.gotPatch)
	}
	got, ok := fake.gotObj.(*rayv1.RayCluster)
	if !ok {
		t.Fatalf("gotObj type = %T, want *rayv1.RayCluster", fake.gotObj)
	}
	state := got.Status.State //nolint:staticcheck // SA1019: asserting the deprecated field was cleared
	if state != "" {
		t.Fatalf("Status.State = %q, want empty (ZeroStatus must run before Patch)", state)
	}
}

// fakeGetClient answers Get for Secrets by name: names in present exist,
// anything else is NotFound. It records the object type every Get asked
// for, so the test can prove the existence check never requests a full
// Secret (metadata only — the data must not reach Bifrost's process).
type fakeGetClient struct {
	client.Client
	present map[string]bool
	asked   []client.Object
}

func (f *fakeGetClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	f.asked = append(f.asked, obj)
	if f.present[key.Name] {
		return nil
	}
	return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
}

func TestEnsureSecretsExistIsMetadataOnlyAndFailsFast(t *testing.T) {
	fake := &fakeGetClient{present: map[string]bool{"s3-a-creds": true}}
	storage := []core.ResolvedStorage{
		{Name: "s3-a", SecretName: "s3-a-creds", Mode: core.StorageModeEnv},
		{Name: "gcs", SecretName: "gcs-key", Mode: core.StorageModeFile},
	}
	err := ensureSecretsExist(context.Background(), fake, "tenants", storage)
	if err == nil {
		t.Fatal("a missing Secret must fail the apply")
	}
	var perr provision.ProvisionError
	if !errors.As(err, &perr) || perr.Kind != provision.ProvisionErrBackend || !strings.Contains(perr.Message, `secret "gcs-key" not found`) {
		t.Fatalf("err = %v, want a backend ProvisionError naming gcs-key", err)
	}
	if len(fake.asked) != 2 {
		t.Fatalf("Get calls = %d, want 2", len(fake.asked))
	}
	for _, obj := range fake.asked {
		meta, ok := obj.(*metav1.PartialObjectMetadata)
		if !ok {
			t.Fatalf("Get asked for %T; only PartialObjectMetadata may be requested for a Secret", obj)
		}
		if gvk := meta.GroupVersionKind(); gvk.Kind != "Secret" || gvk.Version != "v1" {
			t.Fatalf("Get asked for %s, want core/v1 Secret metadata", gvk)
		}
	}
	if err := ensureSecretsExist(context.Background(), fake, "tenants", storage[:1]); err != nil {
		t.Fatalf("all present: %v", err)
	}
	if err := ensureSecretsExist(context.Background(), fake, "tenants", nil); err != nil {
		t.Fatalf("no storage: %v", err)
	}
}
