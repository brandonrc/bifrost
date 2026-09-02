// Requirement 18 — NIST security baseline operation + audit evidence.
package r18_baseline

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

func TestAuditChainVerifiesAfterEvents(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 18, "after a mix of allowed and denied actions the audit hash chain replays clean")
	ctx := context.Background()
	id := req.Name("au")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	_, _ = fixture.Get(t, tgt, "dev-b", id)                            // denied
	_, _ = tgt.As("dev-b").API().GetPolicyWithResponse(ctx)            // denied
	_, _ = tgt.As("admin").API().ListClustersWithResponse(ctx)         // allowed
	_ = fixture.Delete(t, tgt, "dev-a", id)                            // allowed
	_, _ = tgt.As("anon").API().ListClustersWithResponse(ctx)          // 401
	_, _ = tgt.As("dev-a").API().ListAuditEventsWithResponse(ctx, nil) // denied

	ver, err := tgt.As("admin").API().VerifyAuditTrailWithResponse(ctx, nil)
	if err != nil || ver.StatusCode() != http.StatusOK {
		t.Fatalf("audit verify: err=%v status=%v", err, ver.StatusCode())
	}
	var res struct {
		Ok            bool   `json:"ok"`
		EventsChecked int64  `json:"events_checked"`
		FirstBroken   *int64 `json:"first_broken_seq"`
	}
	if err := json.Unmarshal(ver.Body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Ok || res.FirstBroken != nil {
		t.Fatalf("audit chain broken: %s", ver.Body)
	}
	if res.EventsChecked < 4 {
		t.Errorf("events_checked = %d, want at least the 4 audited actions above", res.EventsChecked)
	}
}

func TestDeniedRequestIsAudited(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 18, "a denied request leaves a deny row naming the caller")
	ctx := context.Background()
	subject := fixture.Subject(t, tgt, "dev-b")
	if r, err := tgt.As("dev-b").API().GetPolicyWithResponse(ctx); err != nil || r.StatusCode() != http.StatusForbidden {
		t.Fatalf("dev-b get_policy: err=%v status=%v, want 403", err, r.StatusCode())
	}
	list, err := tgt.As("admin").API().ListAuditEventsWithResponse(ctx, nil)
	if err != nil || list.StatusCode() != http.StatusOK {
		t.Fatalf("list audit: err=%v status=%v", err, list.StatusCode())
	}
	if !hasDeny(list.Body, subject) {
		t.Fatalf("no deny row for %q in the audit trail: %s", subject, truncate(list.Body))
	}
}

// hasDeny scans the AuditListResponse ({"items":[...]}) for a deny
// decision by subject; a bare array is tolerated for older shapes.
func hasDeny(body []byte, subject string) bool {
	var wrapped struct {
		Items []map[string]any `json:"items"`
	}
	events := wrapped.Items
	if err := json.Unmarshal(body, &wrapped); err != nil || wrapped.Items == nil {
		_ = json.Unmarshal(body, &events)
	} else {
		events = wrapped.Items
	}
	for _, e := range events {
		if e["decision"] == "deny" && e["subject"] == subject {
			return true
		}
	}
	return false
}

func truncate(b []byte) string {
	if len(b) > 600 {
		return string(b[:600]) + "…"
	}
	return string(b)
}

// The container posture the pack promises: non-root, read-only root
// filesystem, no privilege escalation. Read from the live pod spec.
func TestControlPlaneRunsNonRootReadOnly(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 18, "the control plane runs as non-root on a read-only root filesystem with no privilege escalation")
	req.NeedK8s(t, tgt)
	k, _ := tgt.K8s()
	ctx := context.Background()
	selector := os.Getenv("REQ_CONTROL_PLANE_SELECTOR")
	if selector == "" {
		selector = "app.kubernetes.io/name=bifrost-pack"
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := k.List(ctx, &pods, ctrlclient.InNamespace(tgt.Namespace()), ctrlclient.MatchingLabelsSelector{Selector: sel}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no control-plane pods match %q in %s", selector, tgt.Namespace())
	}
	for _, p := range pods.Items {
		nonRoot := p.Spec.SecurityContext != nil && p.Spec.SecurityContext.RunAsNonRoot != nil && *p.Spec.SecurityContext.RunAsNonRoot
		for _, c := range p.Spec.Containers {
			sc := c.SecurityContext
			if sc != nil && sc.RunAsNonRoot != nil && *sc.RunAsNonRoot {
				nonRoot = true
			}
			if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Errorf("pod %s container %s: readOnlyRootFilesystem is not true", p.Name, c.Name)
			}
			if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("pod %s container %s: allowPrivilegeEscalation is not false", p.Name, c.Name)
			}
		}
		if !nonRoot {
			t.Errorf("pod %s: runAsNonRoot is not set at pod or container level", p.Name)
		}
	}
}
