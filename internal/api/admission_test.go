package api

import (
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/core"
)

func TestAdmission_ImagePrefixesAndWorkerCap(t *testing.T) {
	spec := func(image string, maxReplicas ...uint32) *core.ClusterSpec {
		s := &core.ClusterSpec{Image: image}
		for i, m := range maxReplicas {
			s.WorkerGroups = append(s.WorkerGroups, core.WorkerGroup{Name: string(rune('a' + i)), MaxReplicas: m})
		}
		return s
	}
	var none Admission
	if err := none.Check(spec("anything/at:all", 99)); err != nil {
		t.Fatalf("zero-value admission must admit everything, got %v", err)
	}
	a := Admission{AllowedImagePrefixes: ParseImagePrefixes(" rayproject/, registry.example.com/ml/ ,"), MaxWorkers: 4}
	if err := a.Check(spec("rayproject/ray:2.56.0", 2, 2)); err != nil {
		t.Fatalf("allowed image within cap refused: %v", err)
	}
	if err := a.Check(spec("docker.io/library/nginx:1", 1)); err == nil || err.reason != "image_not_allowed" {
		t.Fatalf("disallowed image: %+v", err)
	}
	if err := a.Check(spec("rayproject/ray:2.56.0", 3, 2)); err == nil || err.reason != "max_workers_exceeded" || !strings.Contains(err.message, "5") {
		t.Fatalf("over cap: %+v", err)
	}
	if err := a.Check(spec("rayprojectX/ray", 1)); err == nil {
		t.Fatal("prefix match must be a real prefix: rayprojectX/ is not rayproject/")
	}
}
