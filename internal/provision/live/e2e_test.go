//go:build e2e

// Live KubeRay e2e — requires a Kubernetes cluster with the KubeRay
// operator installed. Built only with -tags=e2e; the kind-e2e workflow
// (.github/workflows/kind-e2e.yml) runs it against a kind cluster on
// workflow_dispatch / a weekly schedule, never on push/PR. Exercises the
// full Provisioner contract: ensure namespace posture -> apply -> observe
// until Running -> terminate. Ported (trimmed) from
// the predecessor's provision crate, tests/kuberay_e2e.rs's `provisions_observes_and_terminates`.
package live

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/provision"
)

func e2eNamespace() string {
	if ns := os.Getenv("BIFROST_E2E_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func tinyClusterSpec() *core.ClusterSpec {
	return &core.ClusterSpec{
		Name:       "e2e-demo",
		Project:    "e2e",
		RayVersion: "2.57.0",
		Image:      "rayproject/ray:2.57.0",
		// Ray's head reserves object-store/GCS memory; too little makes
		// the head pod crash-loop. 2560Mi is a safe floor on a kind node.
		HeadCpu:    "1",
		HeadMemory: "2560Mi",
		// One real worker so the e2e actually exercises the worker path,
		// not just the head.
		WorkerGroups: []core.WorkerGroup{{
			Name: "cpu", Cpu: "500m", Memory: "1Gi",
			MinReplicas: 1, MaxReplicas: 2, Replicas: 1,
		}},
	}
}

// assertNamespacePosture asserts EnsureNamespacePosture landed: the
// default-deny and tenant-allow policies exist, every Bifrost-managed
// policy is scoped to the tenant pod label (never namespace-wide), and the
// namespace carries the PSS enforce=baseline label.
func assertNamespacePosture(ctx context.Context, c *Client, ns string) error {
	var list networkingv1.NetworkPolicyList
	if err := c.c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return err
	}
	var haveDeny, haveAllow bool
	for i := range list.Items {
		p := &list.Items[i]
		switch p.Name {
		case provision.DefaultDenyPolicyName:
			haveDeny = true
		case provision.TenantAllowPolicyName:
			haveAllow = true
		}
		if p.Labels[provision.ManagedByLabel] != provision.FieldManager {
			continue
		}
		if len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.PodSelector.MatchExpressions) == 0 {
			return fmt.Errorf("policy %s must not select the whole namespace", p.Name)
		}
	}
	if !haveDeny {
		return fmt.Errorf("default-deny policy %s must exist", provision.DefaultDenyPolicyName)
	}
	if !haveAllow {
		return fmt.Errorf("tenant-allow policy %s must exist", provision.TenantAllowPolicyName)
	}

	var namespace corev1.Namespace
	if err := c.c.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		return err
	}
	if namespace.Labels["pod-security.kubernetes.io/enforce"] != "baseline" {
		return fmt.Errorf("namespace must enforce PSS baseline, labels: %v", namespace.Labels)
	}
	return nil
}

// assertPolicyGone asserts the named NetworkPolicy no longer exists.
func assertPolicyGone(ctx context.Context, c *Client, ns, name string) error {
	var np networkingv1.NetworkPolicy
	err := c.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &np)
	if err == nil {
		return fmt.Errorf("%s must be deleted", name)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func TestE2EProvisionsObservesAndTerminates(t *testing.T) {
	ns := e2eNamespace()
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatalf("get kubeconfig: %v", err)
	}
	c, err := NewClient(cfg, ns, false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx := context.Background()
	id := core.ClusterId("e2e-demo")

	// Namespace posture: idempotent, ensures the default-deny + tenant
	// allow + PSS labels land before anything else applies.
	if err := c.EnsureNamespacePosture(ctx); err != nil {
		t.Fatalf("ensure namespace posture: %v", err)
	}
	if err := c.EnsureNamespacePosture(ctx); err != nil {
		t.Fatalf("re-ensure namespace posture must be idempotent: %v", err)
	}
	if err := assertNamespacePosture(ctx, c, ns); err != nil {
		t.Fatal(err)
	}

	// Idempotent apply (generation 1).
	if _, err := c.Apply(ctx, id, tinyClusterSpec(), 1, "e2e/1", nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := c.Apply(ctx, id, tinyClusterSpec(), 1, "e2e/1", nil); err != nil {
		t.Fatalf("second apply must be idempotent: %v", err)
	}

	clusters, err := c.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, cl := range clusters {
		if cl.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("applied cluster %s must be listed, got %v", id, clusters)
	}

	allowName := provision.ClusterAllowPolicyName(string(id))

	// Poll observe until the head reports Running (image pulls are slow).
	deadline := time.Now().Add(6 * time.Minute)
	var lastState core.ClusterState
	for {
		obs, err := c.Observe(ctx, id)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if obs.ApiBaseUrl == nil || !strings.Contains(*obs.ApiBaseUrl, "e2e-demo-head-svc") {
			t.Fatalf("api base url = %v, want it to reference e2e-demo-head-svc", obs.ApiBaseUrl)
		}
		lastState = obs.State
		if obs.State == core.ClusterStateRunning {
			if obs.ObservedGeneration == nil || *obs.ObservedGeneration != 1 {
				t.Fatalf("observed generation = %v, want 1", obs.ObservedGeneration)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster never reached Running (last: %v)", lastState)
		}
		time.Sleep(10 * time.Second)
	}

	// Teardown is idempotent, and takes the per-cluster allow policy with
	// it.
	if err := c.Terminate(ctx, id); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := c.Terminate(ctx, id); err != nil {
		t.Fatalf("second terminate must be a no-op: %v", err)
	}
	if err := assertPolicyGone(ctx, c, ns, allowName); err != nil {
		t.Fatal(err)
	}
}
