package live

import (
	"context"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
