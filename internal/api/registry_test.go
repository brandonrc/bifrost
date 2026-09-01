package api

import (
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/core"
)

func TestListRegistry_AdminOnly(t *testing.T) {
	token := "shh"
	registry := &core.ClusterRegistry{Clusters: []core.ClusterEndpoint{
		{Id: "c1", Hostname: "c1.example.com", ApiBaseUrl: "https://c1.internal", AuthToken: &token},
		{Id: "c2", Hostname: "c2.example.com", ApiBaseUrl: "https://c2.internal"},
	}}
	s := &Server{Registry: registry}

	// Operator: has Write/Delete/Read on Cluster but not Admin -> denied.
	if _, err := s.ListRegistry(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), ListRegistryRequestObject{}); err == nil {
		t.Fatal("expected operator to be denied — registry is Admin-only")
	} else {
		mustHTTPError(t, err, 403)
	}

	resp, err := s.ListRegistry(ctxWithIdentity(testIdentity("root", auth.RoleAdmin)), ListRegistryRequestObject{})
	if err != nil {
		t.Fatalf("admin should be permitted: %v", err)
	}
	entries := mustResponse[ListRegistry200JSONResponse](t, resp)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	byID := map[string]RegistryEntryView{}
	for _, e := range entries {
		byID[e.Id] = e
	}
	if !byID["c1"].TokenSet {
		t.Error("c1 has a configured AuthToken: token_set should be true")
	}
	if byID["c2"].TokenSet {
		t.Error("c2 has no AuthToken: token_set should be false")
	}
	// The token itself must never appear anywhere in the response.
	for _, e := range entries {
		if e.Hostname == "" || e.ApiBaseUrl == "" {
			t.Errorf("entry missing routing fields: %+v", e)
		}
	}
}

func TestListRegistry_EmptyRegistryIsEmptyArrayNotNull(t *testing.T) {
	s := &Server{}
	resp, err := s.ListRegistry(ctxWithIdentity(testIdentity("root", auth.RoleAdmin)), ListRegistryRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := mustResponse[ListRegistry200JSONResponse](t, resp)
	if entries == nil || len(entries) != 0 {
		t.Fatalf("entries = %#v, want a non-nil empty slice", entries)
	}
}

func TestListRegistry_DevModePermitsUnauthenticated(t *testing.T) {
	s := &Server{Registry: &core.ClusterRegistry{}}
	if _, err := s.ListRegistry(ctxWithIdentity(nil), ListRegistryRequestObject{}); err != nil {
		t.Fatalf("dev mode (no identity) should be permitted: %v", err)
	}
}
