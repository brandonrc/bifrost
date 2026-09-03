package controller

import "github.com/brandonrc/bifrost/internal/core"

// Registrar is the reconciler's view of the gateway routing table
// (requirement 5): when the lifecycle controller brings a cluster up it
// registers the cluster's gateway hostname, and when it tears the cluster
// down it deregisters it, so ephemeral RayJob clusters are reachable
// through the authenticated gateway for exactly as long as they exist.
//
// *core.ClusterRegistry satisfies it (core.ClusterRegistry.Register /
// Deregister wrap Upsert / Remove). The controller depends on this
// interface rather than the registry type so tests can observe
// registrations without a real registry, and so the registry's
// static-entry validation stays the registry's concern.
type Registrar interface {
	// Register adds or replaces the dynamic entry for endpoint.Id. Errors
	// when the entry would shadow a static hostname or collide with
	// another dynamic hostname (see core.ClusterRegistry.Upsert).
	Register(endpoint core.ClusterEndpoint) error
	// Deregister removes the dynamic entry for id; a no-op for an unknown
	// or static id.
	Deregister(id core.ClusterId)
}

var _ Registrar = (*core.ClusterRegistry)(nil)
