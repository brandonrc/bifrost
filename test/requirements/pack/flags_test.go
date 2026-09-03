package pack

import (
	"testing"

	"github.com/brandonrc/bifrost/test/requirements/req"
)

// The gateway values render the two serve flags the dynamic registry needs
// (requirement 5: a job's cluster is reached at <name>.<domain>), and only
// when set — an unset domain must render neither flag.
func TestGatewayDomainRendersAsFlag(t *testing.T) {
	req.Covers(t, 5, "the chart passes --gateway-domain and --gateway-external-base to serve when configured, and neither when not")
	out := Render(t, "image.tag=sha-test", "gateway.domain=ray.example", "gateway.externalBase=https://")
	mustContain(t, out, "--gateway-domain=ray.example", "gateway.domain must reach serve")
	mustContain(t, out, "--gateway-external-base=https://", "gateway.externalBase must reach serve")
	plain := Render(t, "image.tag=sha-test")
	mustNotContain(t, plain, "--gateway-domain", "an unset domain must not render the flag")
	mustNotContain(t, plain, "--gateway-external-base", "an unset external base must not render the flag")
	mustNotContain(t, plain, "--services-per-project", "the default cap of one renders no flag")
	two := Render(t, "image.tag=sha-test", "services.perProject=2")
	mustContain(t, two, "--services-per-project=2", "services.perProject must reach serve when it is not the default")
}

// The control plane's Role must cover the object kinds the build-out added:
// RayJobs (requirement 5) and a metadata-only Secret get (requirement 12).
// It must not grant any other Secrets verb — the point of #12 is that
// credentials never transit Bifrost.
func TestRBACGrantsRayJobsAndSecretMetadataOnly(t *testing.T) {
	req.Covers(t, 12, "the chart grants secrets get only (existence check) and RayJob lifecycle, nothing more on Secrets")
	req.Covers(t, 5, "the chart's Role covers rayjobs so ephemeral jobs can be provisioned")
	out := Render(t, "image.tag=sha-test")
	mustContain(t, out, `resources: ["rayclusters", "rayservices", "rayjobs"]`, "rayjobs must be in the ray.io rule")
	mustContain(t, out, `resources: ["secrets"]`, "a secrets rule must exist for the metadata-only existence check")
	for _, verb := range []string{`"list"`, `"watch"`, `"create"`, `"update"`, `"patch"`, `"delete"`} {
		if idx := indexAfter(out, `resources: ["secrets"]`); idx >= 0 {
			rule := ruleAfter(out, idx)
			if containsVerb(rule, verb) {
				t.Fatalf("secrets rule grants %s; only get is allowed: %s", verb, rule)
			}
		}
	}
}
