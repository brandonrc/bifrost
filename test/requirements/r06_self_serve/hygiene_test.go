package r06_self_serve

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/fixture"
	"github.com/brandonrc/bifrost/test/requirements/req"
	"github.com/brandonrc/bifrost/test/requirements/target"
)

// Idle cleanup: idleness is derived from the persisted gateway job history,
// so a cluster that never runs a gateway job is idle from birth and is
// reaped once idle_timeout_secs passes. (Interactive Ray Client sessions are
// invisible to this signal — SPEC.md documents the limitation; users of
// interactive sessions leave idle_timeout unset and rely on ttl.)
func TestIdleClusterIsReaped(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "an idle cluster is cleaned up once its idle timeout passes")
	idle := fixture.TTL(tgt) // seconds; same budget logic as the max-age reaper
	id := req.Name("idle")
	body := fixture.ClusterBody(id, "team-a", nil)
	body.Spec.IdleTimeoutSecs = &idle
	resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(context.Background(), body)
	if err != nil || resp.StatusCode() != http.StatusCreated {
		t.Fatalf("create: err=%v status=%v body=%s", err, resp.StatusCode(), resp.Body)
	}
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	fixture.WaitGone(t, tgt, "dev-a", id)
}

func TestInvalidSpecIsRefusedWith400(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "users choose approved options: a malformed spec is refused with 400 and nothing is created")
	ctx := context.Background()
	cases := []struct {
		name  string
		patch func(*client.CreateClusterJSONRequestBody)
	}{
		{"negative ttl", func(b *client.CreateClusterJSONRequestBody) { v := int64(-5); b.Spec.TtlSeconds = &v }},
		{"unparseable head cpu", func(b *client.CreateClusterJSONRequestBody) { b.Spec.HeadCpu = "lots" }},
		{"unparseable worker memory", func(b *client.CreateClusterJSONRequestBody) { b.Spec.WorkerGroups[0].Memory = "2 giga" }},
		{"min replicas above max", func(b *client.CreateClusterJSONRequestBody) {
			b.Spec.WorkerGroups[0].MinReplicas = 3
			b.Spec.WorkerGroups[0].MaxReplicas = 1
		}},
	}
	for i, c := range cases {
		id := req.Name(fmt.Sprintf("bad%d", i))
		body := fixture.ClusterBody(id, "team-a", nil)
		c.patch(&body)
		resp, err := tgt.As("dev-a").API().CreateClusterWithResponse(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("%s: create = %d %s, want 400", c.name, resp.StatusCode(), resp.Body)
			continue
		}
		if st, _ := fixture.Get(t, tgt, "admin", id); st != http.StatusNotFound {
			t.Errorf("%s: a refused create must persist nothing; get = %d", c.name, st)
		}
	}
}

// Delete leaves a tombstone the console can show (desired=terminated), and
// ?purge=true removes the row once the cluster is observed gone. Purging a
// live cluster is refused.
func TestDeletedClusterTombstoneAndPurge(t *testing.T) {
	tgt := target.Get(t)
	req.Covers(t, 6, "a stopped cluster is visible as terminated until purged; a live cluster cannot be purged")
	ctx := context.Background()
	id := req.Name("tomb")
	fixture.MustCreate(t, tgt, "dev-a", id, "team-a")
	fixture.WaitObserved(t, tgt, "dev-a", id, "running")
	purge := true
	if r, err := tgt.As("dev-a").API().DeleteClusterWithResponse(ctx, id, &client.DeleteClusterParams{Purge: &purge}); err != nil || r.StatusCode() != http.StatusConflict {
		t.Fatalf("purge of a live cluster: err=%v status=%v body=%s, want 409", err, r.StatusCode(), r.Body)
	}
	if st := fixture.Delete(t, tgt, "dev-a", id); st/100 != 2 {
		t.Fatalf("delete = %d", st)
	}
	fixture.WaitGone(t, tgt, "dev-a", id)
	req.Eventually(t, tgt, func() (bool, string) {
		st, v := fixture.Get(t, tgt, "dev-a", id)
		if st == http.StatusNotFound {
			return true, "already reaped"
		}
		d, o := fixture.State(v)
		if d != "terminated" {
			return false, "desired=" + d
		}
		r, err := tgt.As("dev-a").API().DeleteClusterWithResponse(ctx, id, &client.DeleteClusterParams{Purge: &purge})
		if err != nil {
			return false, err.Error()
		}
		return r.StatusCode()/100 == 2, fmt.Sprintf("purge=%d observed=%s", r.StatusCode(), o)
	})
	if st, _ := fixture.Get(t, tgt, "dev-a", id); st != http.StatusNotFound {
		t.Fatalf("after purge get = %d, want 404", st)
	}
}
