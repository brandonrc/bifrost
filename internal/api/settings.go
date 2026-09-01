// Settings API (api-v1.md §5.16): the store-backed, API-editable governance
// policy — price sheet (cost estimates), per-project quota limits, and
// per-project time-windowed budgets. Ported from mobula-api's settings.rs.
//
// Precedence: the `--policy` boot-time seed is the DEFAULT; the store wins
// once a row exists (seeded lazily on first read, or written by PUT).
// Handlers read the effective policy per request via effectivePolicy, so
// edits apply without a restart. Both routes are Admin-only (governance is
// platform configuration, like pools); the permission target is
// auth.TargetCluster (same convention as the registry/access surfaces).
package api

import (
	"context"
	"math"
	"net/http"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
	"github.com/brandonrc/bifrost/internal/policy"
)

// PolicyConfig is the boot-time `--policy` seed (mobula-api's clusters.rs
// PolicyConfig) — NOT the effective policy. Handlers load the effective
// (store-backed) policy per request via effectivePolicy; this value is
// consulted only until the store holds a policy row.
//
// GPUDefaultSharing (#58) is different: it is boot-time config only, never
// seeded into the store — the per-pool gpu_sharing knob is the
// tenant-visible override, and this is just the platform-wide fallback when
// a pool spec leaves it unset.
type PolicyConfig struct {
	Prices  policy.PriceSheet
	Quotas  map[string]policy.ResourceMap
	Budgets map[string]policy.Budget
	// GPUDefaultSharing defaults to core.DefaultGpuSharing (whole-gpu) at
	// the zero value only when read through EffectiveGPUDefaultSharing —
	// a bare PolicyConfig{} leaves this "" (core.GpuSharing's zero value),
	// which is deliberately not core.DefaultGpuSharing so a caller that
	// forgets to set it fails the isValid() check rather than silently
	// picking a default with no seam to override it.
	GPUDefaultSharing core.GpuSharing
}

// EffectiveGPUDefaultSharing returns c.GPUDefaultSharing, or
// core.DefaultGpuSharing when unset — the platform-wide GPU-sharing
// fallback pools without an explicit gpu_sharing resolve to (#58).
func (c PolicyConfig) EffectiveGPUDefaultSharing() core.GpuSharing {
	if c.GPUDefaultSharing == "" {
		return core.DefaultGpuSharing
	}
	return c.GPUDefaultSharing
}

// seedFromConfig converts the in-flight PolicyConfig seed into a storable
// row, or nil when the seed is empty (no --policy given) — an empty seed
// never materializes a row, so source stays "none". Ported from settings.rs's
// seed_from_config.
func seedFromConfig(cfg *PolicyConfig) *controller.StoredPolicy {
	if cfg.Prices == nil && len(cfg.Quotas) == 0 && len(cfg.Budgets) == 0 {
		return nil
	}
	quotas := make(map[string]map[string]float64, len(cfg.Quotas))
	for k, v := range cfg.Quotas {
		quotas[k] = map[string]float64(v)
	}
	budgets := make(map[string]controller.StoredBudget, len(cfg.Budgets))
	for k, b := range cfg.Budgets {
		budgets[k] = controller.StoredBudget{WindowSecs: b.WindowSecs, Limits: b.Limits}
	}
	var prices map[string]float64
	if cfg.Prices != nil {
		prices = map[string]float64(cfg.Prices)
	}
	return &controller.StoredPolicy{
		Prices:       prices,
		Quotas:       quotas,
		Budgets:      budgets,
		FromFileSeed: true,
	}
}

// configFromStored converts a stored row back into the in-flight
// PolicyConfig shape the policy package consumes. gpuDefaultSharing is
// boot-time-only config, never part of the stored row (see PolicyConfig's
// doc comment) — callers that need it read it from the seed, not from here.
// Ported from settings.rs's config_from_stored.
func configFromStored(p *controller.StoredPolicy) PolicyConfig {
	quotas := make(map[string]policy.ResourceMap, len(p.Quotas))
	for k, v := range p.Quotas {
		quotas[k] = policy.ResourceMap(v)
	}
	budgets := make(map[string]policy.Budget, len(p.Budgets))
	for k, b := range p.Budgets {
		budgets[k] = policy.Budget{WindowSecs: b.WindowSecs, Limits: b.Limits}
	}
	var prices policy.PriceSheet
	if p.Prices != nil {
		prices = policy.PriceSheet(p.Prices)
	}
	return PolicyConfig{Prices: prices, Quotas: quotas, Budgets: budgets}
}

// effectivePolicy is the effective governance policy: the store row when
// one exists (seeded or edited), else the --policy boot seed, which is then
// persisted insert-if-absent so it becomes the row. nil = no policy
// configured at all (no row and an empty seed). Ported from settings.rs's
// effective_policy.
func effectivePolicy(ctx context.Context, store controller.Store, seed *PolicyConfig) (*controller.StoredPolicy, error) {
	if p, err := store.GetPolicy(ctx); err != nil {
		return nil, err
	} else if p != nil {
		return p, nil
	}
	seeded := seedFromConfig(seed)
	if seeded == nil {
		return nil, nil
	}
	// Insert-if-absent: a concurrent PUT that landed first is not
	// clobbered; a concurrent seeder wrote the same values. When the
	// insert loses the race, read back the row that actually won so this
	// request never answers with a stale seed.
	inserted, err := store.SeedPolicy(ctx, seeded)
	if err != nil {
		return nil, err
	}
	if inserted {
		return seeded, nil
	}
	return store.GetPolicy(ctx)
}

// policyView converts a stored policy + its provenance into the wire
// PolicyView response.
func policyView(p *controller.StoredPolicy, source string) PolicyView {
	budgets := make(map[string]BudgetView, len(p.Budgets))
	for k, b := range p.Budgets {
		budgets[k] = BudgetView{WindowSecs: int64(b.WindowSecs), AdditionalProperties: b.Limits}
	}
	quotas := p.Quotas
	if quotas == nil {
		quotas = map[string]map[string]float64{}
	}
	var prices *map[string]float64
	if p.Prices != nil {
		prices = &p.Prices
	}
	return PolicyView{
		Prices:   prices,
		Quotas:   quotas,
		Budgets:  budgets,
		Source:   source,
		Editable: true,
	}
}

// validateAmounts requires every value to be a non-negative finite number
// (JSON can't carry NaN/inf, but negative values can arrive; the check is
// the contract). Ported from settings.rs's validate_amounts.
func validateAmounts(m map[string]float64, what string) error {
	for k, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return HTTPError{Status: http.StatusBadRequest, Code: "bad_request",
				Message: "invalid " + what + " for " + k + ": must be a non-negative finite number"}
		}
	}
	return nil
}

// GetPolicy returns the effective governance policy and its provenance.
// Admin-only (governance is platform configuration).
func (s *Server) GetPolicy(ctx context.Context, _ GetPolicyRequestObject) (GetPolicyResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	p, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if p == nil {
		return GetPolicy200JSONResponse(policyView(&controller.StoredPolicy{}, "none")), nil
	}
	source := "store"
	if p.FromFileSeed {
		source = "file"
	}
	return GetPolicy200JSONResponse(policyView(p, source)), nil
}

// UpdatePolicy replaces sections of the governance policy (section-replace
// semantics — see UpdatePolicy's generated doc comment). Admin-only; emits
// an update_policy audit event on success.
func (s *Server) UpdatePolicy(ctx context.Context, req UpdatePolicyRequestObject) (UpdatePolicyResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: "missing request body"}
	}
	body := req.Body

	// Validate the INCOMING sections only — existing stored/seeded values
	// were accepted by whatever wrote them and must not 400 an unrelated
	// edit.
	//
	// Wire-contract note: the Rust reference distinguishes an ABSENT
	// `prices` key (untouched) from an explicit `"prices": null` (clears
	// the sheet) via Option<Option<T>> (settings.rs's de_present_nullable).
	// The generated Go UpdatePolicy.Prices is a single-level *map, which
	// json.Unmarshal sets to nil for BOTH an absent key and an explicit
	// null — the generated type (frozen; not touched by this task) cannot
	// carry that distinction. This handler therefore treats a nil Prices
	// as "leave the price sheet untouched"; there is currently no wire
	// way to explicitly clear an already-set sheet back to unconfigured
	// (a client can still zero it out with `"prices": {}`). Flagged as a
	// contract-fidelity gap, not silently shipped.
	if body.Prices != nil {
		if err := validateAmounts(*body.Prices, "price"); err != nil {
			return nil, err
		}
	}
	if body.Quotas != nil {
		for project, m := range *body.Quotas {
			if err := validateAmounts(m, "quota for project \""+project+"\""); err != nil {
				return nil, err
			}
		}
	}
	if body.Budgets != nil {
		for project, b := range *body.Budgets {
			if b.WindowSecs <= 0 {
				return nil, HTTPError{Status: http.StatusBadRequest, Code: "bad_request",
					Message: "invalid budget for project \"" + project + "\": window_secs must be > 0"}
			}
			if err := validateAmounts(b.AdditionalProperties, "budget for project \""+project+"\""); err != nil {
				return nil, err
			}
		}
	}

	next, err := effectivePolicy(ctx, s.Store, &s.PolicySeed)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	if next == nil {
		next = &controller.StoredPolicy{}
	}
	if body.Prices != nil {
		next.Prices = *body.Prices
	}
	if body.Quotas != nil {
		next.Quotas = *body.Quotas
	}
	if body.Budgets != nil {
		budgets := make(map[string]controller.StoredBudget, len(*body.Budgets))
		for k, b := range *body.Budgets {
			budgets[k] = controller.StoredBudget{WindowSecs: uint64(b.WindowSecs), Limits: b.AdditionalProperties}
		}
		next.Budgets = budgets
	}
	next.FromFileSeed = false

	if err := s.Store.SetPolicy(ctx, next); err != nil {
		return nil, wrapStoreErr(err)
	}
	action := "update_policy"
	status := uint16(http.StatusOK)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts:       controller.NowUnix(),
		Subject:  identitySubject(identity),
		Decision: core.AuditDecisionAllow,
		Action:   &action,
		Status:   &status,
	})
	return UpdatePolicy200JSONResponse(policyView(next, "store")), nil
}
