// Requirement 12 — private storage (S3 and the like) from the cluster, with
// credentials that reach pods through a secret reference and never through
// the spec or an API response.
package r12_private_storage

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/contract"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestClusterSpecCanReferenceStorageCredentials(t *testing.T) {
	tgt := target.Get(t)
	doc := contract.Load(t)
	req.NotYetBuilt(t, 12, "the cluster spec can name a storage credential secret that reaches the pods without ever appearing in a response", func(b *req.B) {
		spec := doc.Components.Schemas["ClusterSpec"]
		if spec == nil || spec.Value == nil {
			b.Fatal("ClusterSpec schema missing")
		}
		found := false
		for name := range spec.Value.Properties {
			l := strings.ToLower(name)
			if strings.Contains(l, "storage") || strings.Contains(l, "secret") || strings.Contains(l, "s3") || strings.Contains(l, "mount") {
				found = true
			}
		}
		if !found {
			b.Fatal("ClusterSpec has no storage/secret/mount property: private storage cannot be requested through the API")
		}
		// The contract carries `storage` (catalog names) since 0.2.0; the
		// server must resolve every name against the catalog at admission,
		// so a name nothing configured is a 400, never a cluster that
		// silently runs without its credentials.
		id := req.Name("stor")
		body := fixture.ClusterBody(id, "team-a", nil)
		body.Spec.Storage = &[]string{req.Name("nosuchstorage")}
		resp, err := tgt.As("admin").API().CreateClusterWithResponse(context.Background(), body)
		if err != nil {
			b.Fatal(err)
		}
		if resp.StatusCode() == http.StatusCreated {
			b.Cleanup(func() { fixture.Delete(t, tgt, "admin", id) })
		}
		if resp.StatusCode() != http.StatusBadRequest {
			b.Fatalf("create with an unknown storage entry = %d %s, want 400: storage names are not resolved against a catalog", resp.StatusCode(), resp.Body)
		}
	})
}
