package core

import (
	"encoding/json"
	"fmt"
)

// Audit event model (api-v1.md §5.9).
//
// Every security-relevant decision and mutation in the control plane emits
// one AuditEvent. Events dual-write: a tracing target (optionally exported
// to JSONL) and, when a store is configured, an audit_events table, read
// back by GET /api/v1/audit through AuditFilter.
//
// Wire conventions follow api-v1.md §2.1: snake_case serde defaults, unix
// seconds for Ts, and optional fields always present as null when the
// emitting site has no value — missing context is never invented (e.g.
// authn failures have no Subject; pool mutations have no Cluster).

// AuditDecision is whether Bifrost allowed or refused the thing the event
// describes. Serialized snake_case (allow / deny).
type AuditDecision string

const (
	AuditDecisionAllow AuditDecision = "allow"
	AuditDecisionDeny  AuditDecision = "deny"
)

// DefaultAuditDecision mirrors Rust's #[derive(Default)] on AuditDecision.
const DefaultAuditDecision = AuditDecisionAllow

// AsStr returns the wire value ("allow" | "deny").
func (d AuditDecision) AsStr() string {
	switch d {
	case AuditDecisionAllow:
		return "allow"
	case AuditDecisionDeny:
		return "deny"
	}
	return string(d)
}

// ParseAuditDecision parses a wire value into an AuditDecision.
func ParseAuditDecision(s string) (AuditDecision, bool) {
	switch s {
	case "allow":
		return AuditDecisionAllow, true
	case "deny":
		return AuditDecisionDeny, true
	}
	return "", false
}

// UnmarshalJSON rejects any value other than the known AuditDecision
// variants, mirroring serde's strict enum deserialization.
func (d *AuditDecision) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, ok := ParseAuditDecision(s)
	if !ok {
		return fmt.Errorf("core: invalid AuditDecision %q", s)
	}
	*d = v
	return nil
}

// AuditRequired is the (verb, target) permission an authorization decision
// was checked against — mirrors auth package types as lowercase strings
// ("write", "cluster") so core stays free of auth types. Present only on
// authz denials.
type AuditRequired struct {
	// Action is e.g. "read" | "write" | "delete" | "admin".
	Action string `json:"action"`
	// Target is e.g. "job" | "cluster" | "service" | "pool".
	Target string `json:"target"`
}

// AuditEvent is one audit-trail row (api-v1.md §5.9). Fields not
// applicable to the emitting site are nil (serialized null); GrantedRoles
// is empty.
//
// Decision policy: deny rows are emitted at the point of refusal (authn
// failures, authz denials, quota denials); gateway per-request rows are
// always allow — a request Bifrost refuses never reaches the gateway, so
// its deny row comes from the refuser.
type AuditEvent struct {
	// Ts is unix seconds.
	Ts uint64 `json:"ts"`
	// Subject is the authenticated subject; nil for authn failures (no
	// identity yet).
	Subject  *string       `json:"subject"`
	Decision AuditDecision `json:"decision"`
	// Reason is a machine-readable refusal reason (missing_token,
	// invalid_token, insufficient_permission, quota_exceeded); nil on
	// allows.
	Reason *string `json:"reason"`
	// Action is a control-plane mutation (create_cluster, delete_pool,
	// …); nil on gateway rows.
	Action *string `json:"action"`
	// Cluster is the cluster id the event concerns; nil when not
	// cluster-scoped.
	Cluster *string `json:"cluster"`
	// Method is the HTTP method, for gateway and authn/ext_authz rows.
	Method *string `json:"method"`
	// Path is the request path (no query string).
	Path *string `json:"path"`
	// Status is the HTTP status of the outcome, when one is known.
	Status *uint16 `json:"status"`
	// LatencyMs is the gateway upstream round-trip; nil elsewhere.
	LatencyMs *uint64 `json:"latency_ms"`
	// Required is the permission an authz denial was checked against.
	Required *AuditRequired `json:"required"`
	// GrantedRoles are roles the caller held (snake_case); authz denials
	// only, else [].
	GrantedRoles []string `json:"granted_roles"`
}

// auditEventAlias breaks the recursion MarshalJSON would otherwise cause
// by re-entering AuditEvent's own MarshalJSON.
type auditEventAlias AuditEvent

// MarshalJSON ensures GrantedRoles serializes as `[]`, never `null`,
// mirroring Vec<String>'s #[serde(default)] behavior in the Rust
// reference (Vec::default() is an empty vec, which serde always writes as
// `[]`).
func (e AuditEvent) MarshalJSON() ([]byte, error) {
	a := auditEventAlias(e)
	if a.GrantedRoles == nil {
		a.GrantedRoles = []string{}
	}
	return json.Marshal(a)
}

// AuditFilter is a filter for listing audit events, mirroring the
// GET /api/v1/audit query params. All present conditions are ANDed;
// From/To are inclusive unix seconds bounds on Ts.
//
// Pagination is deliberately dead simple: rows come back newest-first by
// their autoincrement seq; Cursor means "only rows with seq strictly
// before this value"; the store returns the next cursor of the oldest
// returned row when more rows exist beyond the page.
type AuditFilter struct {
	// Limit is the page size; AuditFilterDefaultLimit when absent,
	// clamped to AuditFilterMaxLimit.
	Limit *uint32
	// Cursor: only rows with seq < Cursor.
	Cursor     *uint64
	From       *uint64
	To         *uint64
	Subject    *string
	Cluster    *string
	Method     *string
	PathPrefix *string
	MinStatus  *uint16
	Decision   *AuditDecision
	Reason     *string
}

// AuditFilterDefaultLimit is the default page size when Limit is absent.
const AuditFilterDefaultLimit uint32 = 100

// AuditFilterMaxLimit is the maximum page size a store will honor.
const AuditFilterMaxLimit uint32 = 1000

// EffectiveLimit is the page size a store applies: the requested limit or
// the default, clamped into [1, AuditFilterMaxLimit].
func (f AuditFilter) EffectiveLimit() uint32 {
	limit := AuditFilterDefaultLimit
	if f.Limit != nil {
		limit = *f.Limit
	}
	if limit < 1 {
		return 1
	}
	if limit > AuditFilterMaxLimit {
		return AuditFilterMaxLimit
	}
	return limit
}

// Matches reports whether an event matches the filter's non-pagination
// conditions (everything except Limit/Cursor). Shared by the in-memory
// store so it stays behaviorally identical to the SQL WHERE clause in the
// SQL-backed implementations (conformance suite).
func (f AuditFilter) Matches(event *AuditEvent) bool {
	if f.From != nil && event.Ts < *f.From {
		return false
	}
	if f.To != nil && event.Ts > *f.To {
		return false
	}
	if f.Subject != nil && (event.Subject == nil || *event.Subject != *f.Subject) {
		return false
	}
	if f.Cluster != nil && (event.Cluster == nil || *event.Cluster != *f.Cluster) {
		return false
	}
	if f.Method != nil && (event.Method == nil || *event.Method != *f.Method) {
		return false
	}
	if f.PathPrefix != nil {
		if event.Path == nil || !hasPrefix(*event.Path, *f.PathPrefix) {
			return false
		}
	}
	if f.MinStatus != nil && (event.Status == nil || *event.Status < *f.MinStatus) {
		return false
	}
	if f.Decision != nil && event.Decision != *f.Decision {
		return false
	}
	if f.Reason != nil && (event.Reason == nil || *event.Reason != *f.Reason) {
		return false
	}
	return true
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
