// Requirement 16 — the same user experience across Ray and Dask. The engine
// discriminator exists in the contract (engine: ray|dask); no Dask
// provisioner does, so a dask cluster never converges on real Kubernetes.
package r16_dask

import (
	"context"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestDaskClusterIsProvisioned(t *testing.T) {
	tgt := target.Get(t)
	req.NotYetBuilt(t, 16, "engine=dask provisions a DaskCluster and reaches running with the same list/stop UX", func(b *req.B) {
		k, ok := tgt.K8s()
		if !ok {
			b.Fatal("no Dask provisioner exists: internal/provision holds kuberay/kueue only, and inproc's fake converges any engine")
		}
		id := req.Name("dask")
		body := fixture.ClusterBody(id, "team-a", nil)
		engine := client.Engine("dask")
		body.Spec.Engine = &engine
		resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(context.Background(), body)
		if err != nil || resp.StatusCode() != http.StatusCreated {
			b.Fatalf("create dask cluster: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
		}
		req.Eventually(b, tgt, func() (bool, string) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubernetes.dask.org", Version: "v1", Kind: "DaskCluster"})
			if err := k.Get(context.Background(), ctrlclient.ObjectKey{Namespace: tgt.Namespace(), Name: id}, u); err != nil {
				return false, "no DaskCluster: " + err.Error()
			}
			return true, "DaskCluster exists"
		})
		fixture.WaitObserved(b, tgt, "dev-a", id, "running")
	})
}
