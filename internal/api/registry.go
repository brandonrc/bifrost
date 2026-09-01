// Gateway registry read API: exposes the effective routing table the job
// gateway uses (ADR-0002). This is the credential-routing table, so it is
// Admin-only even for reads — and the static Ray tokens are never
// serialized, only their presence (token_set). Ported from mobula-api's
// registry.rs.
package api

import (
	"context"

	"github.com/brandonrc/bifrost/internal/auth"
)

// ListRegistry lists the gateway's static routing table. Admin-only:
// Admin on any target is granted only to auth.RoleAdmin, and api-v1.md
// §2.2 classifies registry surfaces as Admin; auth.TargetCluster because
// registry entries describe cluster routing.
//
// No store is consulted for this authorization check (registry.rs's
// list_registry passes `None` for the store argument) — mirrored here by
// passing a nil Store, so a denial stays trace-only, exactly like the
// upstream store-less registry router.
func (s *Server) ListRegistry(ctx context.Context, _ ListRegistryRequestObject) (ListRegistryResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, nil, identity, auth.Admin, auth.TargetCluster); err != nil {
		return nil, err
	}
	var entries []RegistryEntryView
	if s.Registry != nil {
		entries = make([]RegistryEntryView, 0, len(s.Registry.Clusters))
		for _, c := range s.Registry.Clusters {
			entries = append(entries, RegistryEntryView{
				Id:         c.Id.String(),
				Hostname:   c.Hostname,
				ApiBaseUrl: c.ApiBaseUrl,
				TokenSet:   c.AuthToken != nil,
			})
		}
	}
	if entries == nil {
		entries = []RegistryEntryView{}
	}
	return ListRegistry200JSONResponse(entries), nil
}
