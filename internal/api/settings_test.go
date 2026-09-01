package api

import (
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/policy"
)

func admin() *auth.Identity { return testIdentity("root", auth.RoleAdmin) }

func TestGetPolicy_AdminOnly(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.GetPolicy(ctxWithIdentity(testIdentity("op", auth.RoleOperator)), GetPolicyRequestObject{}); err == nil {
		t.Fatal("expected non-admin to be denied")
	} else {
		mustHTTPError(t, err, 403)
	}
}

func TestGetPolicy_NoneThenFileSeedThenStore(t *testing.T) {
	s := &Server{Store: newMemStore(t)}

	// No seed, no store row -> source "none".
	resp, err := s.GetPolicy(ctxWithIdentity(admin()), GetPolicyRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pv := mustResponse[GetPolicy200JSONResponse](t, resp)
	if pv.Source != "none" {
		t.Errorf("source = %q, want none", pv.Source)
	}

	// A boot seed materializes as source "file" on first read (lazy seed).
	s.PolicySeed = PolicyConfig{Quotas: map[string]policy.ResourceMap{"demo": {"cpu": 5}}}
	resp, err = s.GetPolicy(ctxWithIdentity(admin()), GetPolicyRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pv = mustResponse[GetPolicy200JSONResponse](t, resp)
	if pv.Source != "file" {
		t.Errorf("source = %q, want file", pv.Source)
	}
	if pv.Quotas["demo"]["cpu"] != 5 {
		t.Errorf("quotas = %+v", pv.Quotas)
	}

	// After PUT, the store row wins (source "store"), even against the
	// unchanged boot seed.
	quotas := map[string]map[string]float64{"demo": {"cpu": 9}}
	if _, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Quotas: &quotas}}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	resp, err = s.GetPolicy(ctxWithIdentity(admin()), GetPolicyRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pv = mustResponse[GetPolicy200JSONResponse](t, resp)
	if pv.Source != "store" {
		t.Errorf("source = %q, want store", pv.Source)
	}
	if pv.Quotas["demo"]["cpu"] != 9 {
		t.Errorf("quotas after edit = %+v", pv.Quotas)
	}
}

func TestUpdatePolicy_SectionReplaceSemantics(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	prices := map[string]float64{"cpu": 0.04}
	quotas := map[string]map[string]float64{"demo": {"cpu": 5}}
	if _, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Prices: &prices, Quotas: &quotas}}); err != nil {
		t.Fatalf("seed update failed: %v", err)
	}

	// Updating only quotas leaves prices untouched (absent key = untouched).
	quotas2 := map[string]map[string]float64{"demo": {"cpu": 7}}
	resp, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Quotas: &quotas2}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pv := mustResponse[UpdatePolicy200JSONResponse](t, resp)
	if pv.Prices == nil || (*pv.Prices)["cpu"] != 0.04 {
		t.Errorf("prices should be untouched, got %+v", pv.Prices)
	}
	if pv.Quotas["demo"]["cpu"] != 7 {
		t.Errorf("quotas = %+v, want the just-applied edit", pv.Quotas)
	}

	// An explicit empty map clears all quotas.
	empty := map[string]map[string]float64{}
	resp, err = s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Quotas: &empty}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pv = mustResponse[UpdatePolicy200JSONResponse](t, resp)
	if len(pv.Quotas) != 0 {
		t.Errorf("quotas = %+v, want cleared", pv.Quotas)
	}
}

func TestUpdatePolicy_RejectsNegativeAndNonFiniteAmounts(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	for _, prices := range []map[string]float64{
		{"cpu": -1},
		{"cpu": nan()},
		{"cpu": inf()},
	} {
		p := prices
		_, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Prices: &p}})
		if err == nil {
			t.Fatalf("prices %+v should have been rejected", prices)
		}
		mustHTTPError(t, err, 400)
	}
}

func TestUpdatePolicy_RejectsZeroWindowBudget(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	budgets := map[string]BudgetView{"demo": {WindowSecs: 0, AdditionalProperties: map[string]float64{"cpu": 1}}}
	_, err := s.UpdatePolicy(ctxWithIdentity(admin()), UpdatePolicyRequestObject{Body: &UpdatePolicy{Budgets: &budgets}})
	if err == nil {
		t.Fatal("window_secs=0 should be rejected")
	}
	mustHTTPError(t, err, 400)
}

func nan() float64 { var z float64; return z / z }
func inf() float64 { var z float64; return 1 / z }
