package api

import (
	"fmt"
	"strings"

	"github.com/brandonrc/bifrost/internal/core"
)

// Admission is what a platform administrator lets self-serve users request
// (requirement 7: profiles, images, CPU/memory/GPU and max worker counts).
// CPU/memory/GPU are governed by quotas and budgets in PolicyConfig; this
// covers the two the policy could not express: which images may run, and
// how many workers one cluster may ask for.
type Admission struct {
	// AllowedImagePrefixes: a requested image must start with one of these
	// (e.g. "rayproject/", "registry.example.com/ml/"). Empty = any image.
	AllowedImagePrefixes []string
	// MaxWorkers caps the sum of max_replicas across a cluster's worker
	// groups. 0 = no cap.
	MaxWorkers int
}

// admissionError is the reason a spec is refused, in audit-row form
// (reason) and caller form (message).
type admissionError struct {
	reason, message string
}

// Check returns nil when spec is admissible, else the refusal.
func (a Admission) Check(spec *core.ClusterSpec) *admissionError {
	if len(a.AllowedImagePrefixes) > 0 {
		ok := false
		for _, p := range a.AllowedImagePrefixes {
			if p != "" && strings.HasPrefix(spec.Image, p) {
				ok = true
				break
			}
		}
		if !ok {
			return &admissionError{
				reason:  "image_not_allowed",
				message: fmt.Sprintf("image %q is not in the administrator's allowlist (prefixes: %s)", spec.Image, strings.Join(a.AllowedImagePrefixes, ", ")),
			}
		}
	}
	if a.MaxWorkers > 0 {
		total := 0
		for _, g := range spec.WorkerGroups {
			total += int(g.MaxReplicas)
		}
		if total > a.MaxWorkers {
			return &admissionError{
				reason:  "max_workers_exceeded",
				message: fmt.Sprintf("worker groups ask for up to %d workers; the administrator's cap is %d", total, a.MaxWorkers),
			}
		}
	}
	return nil
}

// ParseImagePrefixes splits a comma-separated flag value into prefixes,
// dropping blanks.
func ParseImagePrefixes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
