package api

import (
	"context"
	_ "embed"
	"net/http"

	"github.com/brandonrc/bifrost/internal/auth"
)

// SpecPath is where the vendored OpenAPI contract is served. mobula-api's
// lib.rs mounts its live-generated spec at exactly this path
// (`SwaggerUi::new("/docs").url("/api/v1/openapi.json", ...)`), and the
// frozen bifrost-api contract doesn't document a path for itself (a
// spec's own serving location is necessarily out-of-band), so this
// carries the legacy path forward rather than inventing a new one — the
// auth middleware's public allowlist matches it exactly.
const SpecPath = "/api/v1/openapi.json"

//go:embed openapi.json
var specJSON []byte

// version is the control-plane semver reported by GET /api/v1/version.
// It tracks the vendored contract's info.version for now; Wave 1 T15
// (the bifrost CLI/binary) may override it at link time the way mobula
// used env!("CARGO_PKG_VERSION").
var version = "0.0.1"

// Server implements StrictServerInterface. Only Healthz and Version are
// live; every other operation returns ErrNotImplemented (501) until
// Wave 1 T11/T12 port the real handlers behind this same generated
// interface — the interface compiles complete from day one (ADR-0002)
// and burns down operation by operation from here.
type Server struct{}

// NewServer constructs the (currently stateless) skeleton server.
func NewServer() *Server { return &Server{} }

var _ StrictServerInterface = (*Server)(nil)

// Healthz is the liveness/readiness probe. Always 200 "ok", ported
// verbatim from mobula-api's lib.rs healthz().
func (s *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200TextResponse("ok"), nil
}

// Version reports control-plane identity + semver. name is always
// "bifrost" — the contract's VersionInfo.name doc says so, and it is the
// rebranded product identity (Global Constraints: "user-visible strings
// say Bifrost").
func (s *Server) Version(_ context.Context, _ VersionRequestObject) (VersionResponseObject, error) {
	return Version200JSONResponse{Name: "bifrost", Version: version}, nil
}

// specHandler serves the vendored contract verbatim at SpecPath.
func specHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(specJSON)
	}
}

// HandlerOptions configures NewHandler's auth wiring. Both Validator and
// Local may be nil (dev mode, matching mobula-api's `validator = None`
// build_router() path) or either/both may be set — mirroring
// mobula-api's `resolve_identity` dispatch (OIDC for JWT-shaped bearers,
// local for opaque `mob_*` PATs).
type HandlerOptions struct {
	Validator *auth.Validator
	Local     *auth.LocalAuthenticator
	// AllowUnauthenticated permits binding a non-loopback address with
	// no authentication configured (mobula-api's --dev-allow-unauthenticated,
	// ServeOptions.allow_unauthenticated). It does NOT disable
	// deny-by-default when a validator or local authenticator IS
	// configured — it only lifts the fail-closed non-loopback guard
	// when neither is.
	AllowUnauthenticated bool
	// StrictMiddlewares run inside the typed strict-server layer, ahead
	// of any generated handler. Empty by default.
	StrictMiddlewares []StrictMiddlewareFunc
}

// NewHandler builds the full Bifrost API http.Handler: the generated
// routes plus SpecPath, wrapped by the deny-by-default auth middleware
// and — when neither a validator nor local auth is configured and
// AllowUnauthenticated is false — the outermost fail-closed
// non-loopback guard (mobula-api lib.rs build_app_full_svc_inner's layer
// order: auth outermost, routes innermost; the loopback guard, when
// installed, wraps everything).
func NewHandler(server StrictServerInterface, opts HandlerOptions) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+SpecPath, specHandler())

	strict := NewStrictHandlerWithOptions(server, opts.StrictMiddlewares, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			WriteError(w, r, &HTTPError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()})
		},
		ResponseErrorHandlerFunc: WriteError,
	})
	h := HandlerWithOptions(strict, StdHTTPServerOptions{BaseRouter: mux})

	h = RequireAuth(AuthState{Validator: opts.Validator, Local: opts.Local})(h)

	// Fail-closed (mobula-api #45): when no authentication is
	// configured at all and it hasn't been explicitly overridden,
	// install the outermost per-request loopback guard so a caller who
	// hands this Handler straight to http.Serve on a non-loopback
	// listener still gets refused, regardless of what the (not-yet-built,
	// T15) CLI's own bind-time check does.
	if opts.Validator == nil && opts.Local == nil && !opts.AllowUnauthenticated {
		h = RefuseNonLoopback(h)
	}

	return h
}

// ListAssignments is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListAssignments(_ context.Context, _ ListAssignmentsRequestObject) (ListAssignmentsResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeleteAssignment is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeleteAssignment(_ context.Context, _ DeleteAssignmentRequestObject) (DeleteAssignmentResponseObject, error) {
	return nil, ErrNotImplemented
}

// UpsertAssignment is not yet implemented (Wave 1 T11/T12).
func (s *Server) UpsertAssignment(_ context.Context, _ UpsertAssignmentRequestObject) (UpsertAssignmentResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListRoles is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListRoles(_ context.Context, _ ListRolesRequestObject) (ListRolesResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListAuditEvents is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListAuditEvents(_ context.Context, _ ListAuditEventsRequestObject) (ListAuditEventsResponseObject, error) {
	return nil, ErrNotImplemented
}

// VerifyAuditTrail is not yet implemented (Wave 1 T11/T12).
func (s *Server) VerifyAuditTrail(_ context.Context, _ VerifyAuditTrailRequestObject) (VerifyAuditTrailResponseObject, error) {
	return nil, ErrNotImplemented
}

// Login is not yet implemented (Wave 1 T11/T12).
func (s *Server) Login(_ context.Context, _ LoginRequestObject) (LoginResponseObject, error) {
	return nil, ErrNotImplemented
}

// Logout is not yet implemented (Wave 1 T11/T12).
func (s *Server) Logout(_ context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
	return nil, ErrNotImplemented
}

// Providers is not yet implemented (Wave 1 T11/T12).
func (s *Server) Providers(_ context.Context, _ ProvidersRequestObject) (ProvidersResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListTokens is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListTokens(_ context.Context, _ ListTokensRequestObject) (ListTokensResponseObject, error) {
	return nil, ErrNotImplemented
}

// CreateToken is not yet implemented (Wave 1 T11/T12).
func (s *Server) CreateToken(_ context.Context, _ CreateTokenRequestObject) (CreateTokenResponseObject, error) {
	return nil, ErrNotImplemented
}

// RevokeToken is not yet implemented (Wave 1 T11/T12).
func (s *Server) RevokeToken(_ context.Context, _ RevokeTokenRequestObject) (RevokeTokenResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListUsers is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListUsers(_ context.Context, _ ListUsersRequestObject) (ListUsersResponseObject, error) {
	return nil, ErrNotImplemented
}

// CreateUser is not yet implemented (Wave 1 T11/T12).
func (s *Server) CreateUser(_ context.Context, _ CreateUserRequestObject) (CreateUserResponseObject, error) {
	return nil, ErrNotImplemented
}

// UpdateUser is not yet implemented (Wave 1 T11/T12).
func (s *Server) UpdateUser(_ context.Context, _ UpdateUserRequestObject) (UpdateUserResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListClusters is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListClusters(_ context.Context, _ ListClustersRequestObject) (ListClustersResponseObject, error) {
	return nil, ErrNotImplemented
}

// CreateCluster is not yet implemented (Wave 1 T11/T12).
func (s *Server) CreateCluster(_ context.Context, _ CreateClusterRequestObject) (CreateClusterResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeleteCluster is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeleteCluster(_ context.Context, _ DeleteClusterRequestObject) (DeleteClusterResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetCluster is not yet implemented (Wave 1 T11/T12).
func (s *Server) GetCluster(_ context.Context, _ GetClusterRequestObject) (GetClusterResponseObject, error) {
	return nil, ErrNotImplemented
}

// ClusterEvents is not yet implemented (Wave 1 T11/T12).
func (s *Server) ClusterEvents(_ context.Context, _ ClusterEventsRequestObject) (ClusterEventsResponseObject, error) {
	return nil, ErrNotImplemented
}

// ClusterJobs is not yet implemented (Wave 1 T11/T12).
func (s *Server) ClusterJobs(_ context.Context, _ ClusterJobsRequestObject) (ClusterJobsResponseObject, error) {
	return nil, ErrNotImplemented
}

// ClusterLogs is not yet implemented (Wave 1 T11/T12).
func (s *Server) ClusterLogs(_ context.Context, _ ClusterLogsRequestObject) (ClusterLogsResponseObject, error) {
	return nil, ErrNotImplemented
}

// ClusterMetrics is not yet implemented (Wave 1 T11/T12).
func (s *Server) ClusterMetrics(_ context.Context, _ ClusterMetricsRequestObject) (ClusterMetricsResponseObject, error) {
	return nil, ErrNotImplemented
}

// ClusterNodes is not yet implemented (Wave 1 T11/T12).
func (s *Server) ClusterNodes(_ context.Context, _ ClusterNodesRequestObject) (ClusterNodesResponseObject, error) {
	return nil, ErrNotImplemented
}

// ResumeCluster is not yet implemented (Wave 1 T11/T12).
func (s *Server) ResumeCluster(_ context.Context, _ ResumeClusterRequestObject) (ResumeClusterResponseObject, error) {
	return nil, ErrNotImplemented
}

// SuspendCluster is not yet implemented (Wave 1 T11/T12).
func (s *Server) SuspendCluster(_ context.Context, _ SuspendClusterRequestObject) (SuspendClusterResponseObject, error) {
	return nil, ErrNotImplemented
}

// Identity is not yet implemented (Wave 1 T11/T12).
func (s *Server) Identity(_ context.Context, _ IdentityRequestObject) (IdentityResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListJobs is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListJobs(_ context.Context, _ ListJobsRequestObject) (ListJobsResponseObject, error) {
	return nil, ErrNotImplemented
}

// Metrics is not yet implemented (Wave 1 T11/T12).
func (s *Server) Metrics(_ context.Context, _ MetricsRequestObject) (MetricsResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListPools is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListPools(_ context.Context, _ ListPoolsRequestObject) (ListPoolsResponseObject, error) {
	return nil, ErrNotImplemented
}

// CreatePool is not yet implemented (Wave 1 T11/T12).
func (s *Server) CreatePool(_ context.Context, _ CreatePoolRequestObject) (CreatePoolResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeletePool is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeletePool(_ context.Context, _ DeletePoolRequestObject) (DeletePoolResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetPool is not yet implemented (Wave 1 T11/T12).
func (s *Server) GetPool(_ context.Context, _ GetPoolRequestObject) (GetPoolResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListAllocations is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListAllocations(_ context.Context, _ ListAllocationsRequestObject) (ListAllocationsResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeleteAllocation is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeleteAllocation(_ context.Context, _ DeleteAllocationRequestObject) (DeleteAllocationResponseObject, error) {
	return nil, ErrNotImplemented
}

// PutAllocation is not yet implemented (Wave 1 T11/T12).
func (s *Server) PutAllocation(_ context.Context, _ PutAllocationRequestObject) (PutAllocationResponseObject, error) {
	return nil, ErrNotImplemented
}

// PoolUsage is not yet implemented (Wave 1 T11/T12).
func (s *Server) PoolUsage(_ context.Context, _ PoolUsageRequestObject) (PoolUsageResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListRegistry is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListRegistry(_ context.Context, _ ListRegistryRequestObject) (ListRegistryResponseObject, error) {
	return nil, ErrNotImplemented
}

// ListServices is not yet implemented (Wave 1 T11/T12).
func (s *Server) ListServices(_ context.Context, _ ListServicesRequestObject) (ListServicesResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeployService is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeployService(_ context.Context, _ DeployServiceRequestObject) (DeployServiceResponseObject, error) {
	return nil, ErrNotImplemented
}

// DeleteService is not yet implemented (Wave 1 T11/T12).
func (s *Server) DeleteService(_ context.Context, _ DeleteServiceRequestObject) (DeleteServiceResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetService is not yet implemented (Wave 1 T11/T12).
func (s *Server) GetService(_ context.Context, _ GetServiceRequestObject) (GetServiceResponseObject, error) {
	return nil, ErrNotImplemented
}

// GetPolicy is not yet implemented (Wave 1 T11/T12).
func (s *Server) GetPolicy(_ context.Context, _ GetPolicyRequestObject) (GetPolicyResponseObject, error) {
	return nil, ErrNotImplemented
}

// UpdatePolicy is not yet implemented (Wave 1 T11/T12).
func (s *Server) UpdatePolicy(_ context.Context, _ UpdatePolicyRequestObject) (UpdatePolicyResponseObject, error) {
	return nil, ErrNotImplemented
}

// UsageReport is not yet implemented (Wave 1 T11/T12).
func (s *Server) UsageReport(_ context.Context, _ UsageReportRequestObject) (UsageReportResponseObject, error) {
	return nil, ErrNotImplemented
}
