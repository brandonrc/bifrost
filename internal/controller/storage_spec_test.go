package controller

import (
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

// Requirement 12: the persisted storage resolution is part of the spec the
// generation tracks — a re-submitted spec whose names resolve to a
// different Secret must bump, one whose resolution is unchanged must not.
func TestSpecChangedTracksStorageResolution(t *testing.T) {
	base := func() core.ClusterSpec {
		return core.ClusterSpec{
			Name: "c", Project: "p", RayVersion: "2.9.0", Image: "img", HeadCpu: "1", HeadMemory: "1Gi",
			Storage:         []string{"s3"},
			StorageResolved: []core.ResolvedStorage{{Name: "s3", SecretName: "s3-creds", Mode: core.StorageModeEnv}},
		}
	}
	a, b := base(), base()
	if specChanged(&a, &b) {
		t.Fatal("identical specs must not differ")
	}
	b.StorageResolved[0].SecretName = "s3-creds-v2"
	if !specChanged(&a, &b) {
		t.Fatal("a re-resolved Secret name must bump the generation")
	}
	b = base()
	b.StorageResolved[0].Mode = core.StorageModeFile
	mount := "/opt/s3"
	b.StorageResolved[0].MountPath = &mount
	if !specChanged(&a, &b) {
		t.Fatal("a changed delivery mode must bump the generation")
	}
	empty, nilRes := base(), base()
	empty.StorageResolved, nilRes.StorageResolved = []core.ResolvedStorage{}, nil
	if specChanged(&empty, &nilRes) {
		t.Fatal("nil and empty resolutions are the same desired state")
	}

	sa := core.ServiceSpec{Name: "s", Storage: []string{"s3"}, StorageResolved: a.StorageResolved}
	sb := sa
	sb.StorageResolved = []core.ResolvedStorage{{Name: "s3", SecretName: "other", Mode: core.StorageModeEnv}}
	if !serviceSpecChanged(&sa, &sb) {
		t.Fatal("service specs track the resolution too")
	}
}
