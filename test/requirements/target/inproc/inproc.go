// Package inproc is the L2 target: the real control plane — app.New, real
// API server, real auth, real reconciler, real (memory) store — with only
// the Kubernetes edge faked. It is the ONE package under test/requirements
// allowed to import internal/ (guards_test.go enforces this).
package inproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/brandonrc/bifrost/internal/app"
	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/pkg/client"
	"github.com/brandonrc/bifrost/test/requirements/req"
)

type principal struct {
	role    core.LocalRole
	project string // "" = no project assignment
}

// Seeded principals, identical on every target (spec §1.1).
var principals = map[string]principal{
	"admin":    {role: core.LocalRoleAdmin},
	"operator": {role: core.LocalRoleOperator},
	"dev-a":    {role: core.LocalRoleDeveloper, project: "team-a"},
	"dev-b":    {role: core.LocalRoleDeveloper, project: "team-b"},
}

func password(name string) string { return "pw-" + name + "-0123456789" }

type target struct {
	srv       *httptest.Server
	store     controller.Store
	principal string
	tokens    *sync.Map // principal -> bearer token
	cancel    context.CancelFunc
}

// New builds the in-process target and starts its reconcile loop. Callers
// (target.Get) register Cleanup and srv.Close via t.Cleanup.
func New(t testing.TB) req.Target {
	t.Helper()
	store := controller.NewMemoryStore()
	ctx := context.Background()
	for name, p := range principals {
		hash, err := auth.HashPassword(password(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateLocalUser(ctx, name, nil, hash, p.role); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	a, err := app.New(app.Config{
		Store:             store,
		Local:             auth.NewLocalAuthenticator(store, 86_400, 90),
		Provisioner:       newFakeProvisioner(),
		ReconcileInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	go a.RunLoops(loopCtx)
	srv := httptest.NewServer(a.Handler)

	tg := &target{srv: srv, store: store, principal: "admin", tokens: &sync.Map{}, cancel: cancel}
	// Project-scoped assignments are made through the API as admin, so the
	// seeding itself exercises the real access path.
	for name, p := range principals {
		if p.project == "" {
			continue
		}
		body := client.UpsertAssignmentJSONRequestBody{}
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"role":"developer","scope":"project:%s"}`, p.project)), &body)
		resp, err := tg.API().UpsertAssignmentWithResponse(ctx, name, body)
		if err != nil || resp.StatusCode()/100 != 2 {
			t.Fatalf("assign %s: err=%v status=%v body=%s", name, err, statusOf(resp), bodyOf(resp))
		}
	}
	t.Cleanup(func() {
		_ = tg.Cleanup(context.Background())
		cancel()
		srv.Close()
	})
	return tg
}

func statusOf(r *client.UpsertAssignmentHTTPResponse) any {
	if r == nil {
		return nil
	}
	return r.StatusCode()
}
func bodyOf(r *client.UpsertAssignmentHTTPResponse) string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

func (tg *target) Name() string      { return "inproc" }
func (tg *target) Namespace() string { return "inproc" }
func (tg *target) Clock() req.FakeClock {
	return nil // deferred to P1 (plan Global Constraints)
}
func (tg *target) K8s() (ctrlclient.Client, bool) { return nil, false }
func (tg *target) Has(capability string) bool     { return false }
func (tg *target) BaseURL() string                { return tg.srv.URL }
func (tg *target) Authorize(r *http.Request) {
	if tok := tg.token(context.Background()); tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
}

func (tg *target) As(p string) req.Target {
	if _, ok := principals[p]; !ok && p != "anon" {
		panic("inproc: unknown principal " + p)
	}
	cp := *tg
	cp.principal = p
	return &cp
}

func (tg *target) token(ctx context.Context) string {
	if tg.principal == "anon" {
		return ""
	}
	if v, ok := tg.tokens.Load(tg.principal); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	c, _ := client.NewClientWithResponses(tg.srv.URL)
	body := client.LoginJSONRequestBody{}
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"username":%q,"password":%q}`, tg.principal, password(tg.principal))), &body)
	resp, err := c.LoginWithResponse(ctx, body)
	if err != nil || resp.StatusCode() != http.StatusOK {
		panic(fmt.Sprintf("inproc: login %s failed: %v %s", tg.principal, err, bodyStr(resp)))
	}
	var m struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(resp.Body, &m)
	tg.tokens.Store(tg.principal, m.Token)
	return m.Token
}

func bodyStr(r *client.LoginHTTPResponse) string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

func (tg *target) API() *client.ClientWithResponses {
	tok := tg.token(context.Background())
	c, err := client.NewClientWithResponses(tg.srv.URL, client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}))
	if err != nil {
		panic(err)
	}
	return c
}

// Cleanup deletes every cluster whose id carries the run prefix, as admin.
func (tg *target) Cleanup(ctx context.Context) error {
	api := tg.As("admin").API()
	list, err := api.ListClustersWithResponse(ctx)
	if err != nil {
		return err
	}
	var items []struct {
		Id string `json:"id"`
	}
	_ = json.Unmarshal(list.Body, &items)
	prefix := req.RunID() + "-"
	for _, it := range items {
		if strings.HasPrefix(it.Id, prefix) {
			if _, err := api.DeleteClusterWithResponse(ctx, it.Id, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
