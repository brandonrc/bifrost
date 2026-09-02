// Requirement 12 — private storage (S3 and the like) from the cluster, with
// credentials that reach pods through a secret reference and never through
// the spec or an API response.
package r12_private_storage

import (
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/contract"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestClusterSpecCanReferenceStorageCredentials(t *testing.T) {
	_ = target.Get(t)
	doc := contract.Load(t)
	req.NotYetBuilt(t, 12, "the cluster spec can name a storage credential secret that reaches the pods without ever appearing in a response", func(b *req.B) {
		spec := doc.Components.Schemas["ClusterSpec"]
		if spec == nil || spec.Value == nil {
			b.Fatal("ClusterSpec schema missing")
		}
		for name := range spec.Value.Properties {
			l := strings.ToLower(name)
			if strings.Contains(l, "storage") || strings.Contains(l, "secret") || strings.Contains(l, "s3") || strings.Contains(l, "mount") {
				return
			}
		}
		b.Fatal("ClusterSpec has no storage/secret/mount property: private storage cannot be requested through the API")
	})
}
