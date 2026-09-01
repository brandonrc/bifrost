// Persisted audit trail (api-v1.md §5.9).
//
// Every audit-emitting site goes through EmitAudit (authz.go), which
// dual-writes: the event is logged (slog) and, when a store is
// configured, appended to the store. The store write is awaited (events
// are small) but a failure logs a warning and NEVER fails the request
// being audited.
//
// Tamper-evidence and access (#59, api-v1.md §5.9): the store hash-chains
// every appended row (sha256 of prev-hash || canonical row);
// GET /api/v1/audit/verify replays it. Reads (list, CSV export, verify)
// themselves append audit_read rows. Both endpoints need Read on
// Target::Audit — Admin's catch-all or Role::Auditor, nothing else. Ported
// from mobula-api's audit.rs.
package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// csvField writes value into out per RFC 4180 quoting: a field containing
// ",", '"', CR or LF is wrapped in double quotes, with inner quotes
// doubled. Ported from audit.rs's csv_field.
func csvField(out *strings.Builder, value string) {
	if strings.ContainsAny(value, ",\"\n\r") {
		out.WriteByte('"')
		for _, c := range value {
			if c == '"' {
				out.WriteByte('"')
			}
			out.WriteRune(c)
		}
		out.WriteByte('"')
	} else {
		out.WriteString(value)
	}
}

// auditEventToWire converts core.AuditEvent to the generated wire
// AuditEvent. Field-by-field: several fields differ in width
// (core.AuditEvent.Status is *uint16 vs the wire's *int32,
// core.AuditEvent.Ts is uint64 vs the wire's int64, ...) and
// core.AuditEvent.GrantedRoles is a plain []string vs the wire's *[]string
// — Go's blind struct conversion requires identical field types, so this
// is manual.
func auditEventToWire(e *core.AuditEvent) AuditEvent {
	var status *int32
	if e.Status != nil {
		v := int32(*e.Status)
		status = &v
	}
	var latency *int64
	if e.LatencyMs != nil {
		v := int64(*e.LatencyMs)
		latency = &v
	}
	var required *AuditRequired
	if e.Required != nil {
		required = &AuditRequired{Action: e.Required.Action, Target: e.Required.Target}
	}
	granted := e.GrantedRoles
	if granted == nil {
		granted = []string{}
	}
	return AuditEvent{
		Action: e.Action, Cluster: e.Cluster, Decision: AuditDecision(e.Decision.AsStr()),
		GrantedRoles: &granted, LatencyMs: latency, Method: e.Method, Path: e.Path,
		Reason: e.Reason, Required: required, Status: status, Subject: e.Subject, Ts: int64(e.Ts),
	}
}

func optStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// renderAuditCSV renders one audit page as CSV. granted_roles joins with
// ";" (comma is the delimiter); absent optional fields are empty cells.
// Ported from audit.rs's render_csv.
func renderAuditCSV(rows []controller.AuditRow) string {
	var out strings.Builder
	out.WriteString("seq,ts,subject,decision,reason,action,cluster,method,path,status," +
		"latency_ms,required_action,required_target,granted_roles\n")
	for _, row := range rows {
		e := row.Event
		reqAction, reqTarget := "", ""
		if e.Required != nil {
			reqAction, reqTarget = e.Required.Action, e.Required.Target
		}
		status, latency := "", ""
		if e.Status != nil {
			status = strconv.FormatUint(uint64(*e.Status), 10)
		}
		if e.LatencyMs != nil {
			latency = strconv.FormatUint(*e.LatencyMs, 10)
		}
		cells := []string{
			strconv.FormatUint(row.Seq, 10),
			strconv.FormatUint(e.Ts, 10),
			optStr(e.Subject),
			e.Decision.AsStr(),
			optStr(e.Reason),
			optStr(e.Action),
			optStr(e.Cluster),
			optStr(e.Method),
			optStr(e.Path),
			status,
			latency,
			reqAction,
			reqTarget,
			strings.Join(e.GrantedRoles, ";"),
		}
		for i, cell := range cells {
			if i > 0 {
				out.WriteByte(',')
			}
			csvField(&out, cell)
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// emitAuditRead appends the audit_read event for a successful audit-surface
// read (#59, SOC 2 CC7.2): reading the trail itself appends a row —
// deliberate recursion, so audit access is itself auditable. path carries
// the request's query string — an exception to the usual no-query-string
// convention, because the filter params ARE the payload worth auditing.
// Ported from audit.rs's emit_audit_read.
func (s *Server) emitAuditRead(ctx context.Context, identity *auth.Identity, path string) {
	action := "audit_read"
	status := uint16(http.StatusOK)
	EmitAudit(ctx, s.Store, &core.AuditEvent{
		Ts: controller.NowUnix(), Subject: identitySubject(identity),
		Decision: core.AuditDecisionAllow, Action: &action, Path: &path, Status: &status,
	})
}

// auditListCSVResponse renders a ListAuditEvents page as text/csv with a
// Content-Disposition attachment header, instead of the generated 200's
// JSON envelope — a custom ResponseObject implementation (see
// cluster_obs.go's clusterJobsUpstreamResponse for the same pattern) is the
// only way to change content-type for one branch without touching the
// generated file.
type auditListCSVResponse string

func (r auditListCSVResponse) VisitListAuditEventsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(r))
	return err
}

// ListAuditEvents lists the persisted audit trail, newest first.
// Admin/Auditor-only (Read on Target::Audit). ?format=csv exports the page
// as text/csv instead.
func (s *Server) ListAuditEvents(ctx context.Context, req ListAuditEventsRequestObject) (ListAuditEventsResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetAudit); err != nil {
		return nil, err
	}
	q := req.Params
	if q.From != nil && q.To != nil && *q.From > *q.To {
		return nil, badRequest("from must not be after to")
	}
	csv := false
	if q.Format != nil {
		switch *q.Format {
		case "csv":
			csv = true
		default:
			return nil, badRequest(fmt.Sprintf("unknown format %q", *q.Format))
		}
	}

	var limit *uint32
	if q.Limit != nil {
		v := uint32(*q.Limit)
		limit = &v
	}
	var cursor *uint64
	if q.Cursor != nil {
		v := uint64(*q.Cursor)
		cursor = &v
	}
	var from, to *uint64
	if q.From != nil {
		v := uint64(*q.From)
		from = &v
	}
	if q.To != nil {
		v := uint64(*q.To)
		to = &v
	}
	var minStatus *uint16
	if q.MinStatus != nil {
		v := uint16(*q.MinStatus)
		minStatus = &v
	}
	var decision *core.AuditDecision
	if q.Decision != nil {
		v := core.AuditDecision(*q.Decision)
		decision = &v
	}
	filter := core.AuditFilter{
		Limit: limit, Cursor: cursor, From: from, To: to,
		Subject: q.Subject, Cluster: q.Cluster, Method: q.Method, PathPrefix: q.PathPrefix,
		MinStatus: minStatus, Decision: decision, Reason: q.Reason,
	}

	rows, nextCursor, err := s.Store.ListAudit(ctx, filter)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	// Successful reads are themselves audited (#59) — CSV exports
	// included; the export is distinguishable by format=csv in the
	// recorded path's query string.
	s.emitAuditRead(ctx, identity, requestPath(req))
	if csv {
		return auditListCSVResponse(renderAuditCSV(rows)), nil
	}
	items := make([]AuditEvent, len(rows))
	for i, r := range rows {
		items[i] = auditEventToWire(&r.Event)
	}
	var nc *int64
	if nextCursor != nil {
		v := int64(*nextCursor)
		nc = &v
	}
	return ListAuditEvents200JSONResponse(AuditListResponse{Items: items, NextCursor: nc}), nil
}

// requestPath rebuilds "path?query" from the parsed ListAuditEventsParams —
// the strict-server layer hands handlers a parsed Params struct, not the
// raw *http.Request, so the query string is reassembled from the fields
// that were actually present rather than read off r.URL. Only non-empty
// params contribute, matching what the client actually sent closely enough
// for the audit trail's purpose (a human-auditable record of "what was
// queried", not a byte-exact echo).
func requestPath(req ListAuditEventsRequestObject) string {
	q := req.Params
	var parts []string
	add := func(k, v string) { parts = append(parts, k+"="+v) }
	if q.Limit != nil {
		add("limit", strconv.FormatInt(int64(*q.Limit), 10))
	}
	if q.Cursor != nil {
		add("cursor", strconv.FormatInt(*q.Cursor, 10))
	}
	if q.From != nil {
		add("from", strconv.FormatInt(*q.From, 10))
	}
	if q.To != nil {
		add("to", strconv.FormatInt(*q.To, 10))
	}
	if q.Subject != nil {
		add("subject", *q.Subject)
	}
	if q.Cluster != nil {
		add("cluster", *q.Cluster)
	}
	if q.Method != nil {
		add("method", *q.Method)
	}
	if q.PathPrefix != nil {
		add("path_prefix", *q.PathPrefix)
	}
	if q.MinStatus != nil {
		add("min_status", strconv.FormatInt(int64(*q.MinStatus), 10))
	}
	if q.Decision != nil {
		add("decision", string(*q.Decision))
	}
	if q.Reason != nil {
		add("reason", *q.Reason)
	}
	if q.Format != nil {
		add("format", *q.Format)
	}
	if len(parts) == 0 {
		return "/api/v1/audit"
	}
	return "/api/v1/audit?" + strings.Join(parts, "&")
}

// verifyDefaultLimit/verifyMaxLimit: "all" in practice for any realistic
// trail, bounded so a huge table can't OOM the process (#59).
const (
	verifyDefaultLimit uint32 = 100_000
	verifyMaxLimit     uint32 = 1_000_000
)

// VerifyAuditTrail replays the audit hash chain over a window.
// Admin/Auditor-only (Read on Target::Audit).
func (s *Server) VerifyAuditTrail(ctx context.Context, req VerifyAuditTrailRequestObject) (VerifyAuditTrailResponseObject, error) {
	identity, _ := IdentityFromContext(ctx)
	if err := Authorize(ctx, s.Store, identity, auth.Read, auth.TargetAudit); err != nil {
		return nil, err
	}
	limit := verifyDefaultLimit
	if req.Params.Limit != nil {
		limit = uint32(*req.Params.Limit)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > verifyMaxLimit {
		limit = verifyMaxLimit
	}
	var fromSeq *uint64
	if req.Params.FromSeq != nil {
		v := uint64(*req.Params.FromSeq)
		fromSeq = &v
	}
	window, err := s.Store.AuditChain(ctx, fromSeq, limit)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	v := controller.VerifyAuditChain(window.Head, window.Rows)
	// Verify reads the trail, so it too leaves an audit_read row (#59) —
	// appended after the replay, so the event itself never perturbs the
	// window it just checked.
	path := "/api/v1/audit/verify"
	if req.Params.FromSeq != nil || req.Params.Limit != nil {
		var parts []string
		if req.Params.FromSeq != nil {
			parts = append(parts, "from_seq="+strconv.FormatInt(*req.Params.FromSeq, 10))
		}
		if req.Params.Limit != nil {
			parts = append(parts, "limit="+strconv.FormatInt(int64(*req.Params.Limit), 10))
		}
		path += "?" + strings.Join(parts, "&")
	}
	s.emitAuditRead(ctx, identity, path)
	var firstBroken *int64
	if v.FirstBrokenSeq != nil {
		fb := int64(*v.FirstBrokenSeq)
		firstBroken = &fb
	}
	return VerifyAuditTrail200JSONResponse(AuditVerifyResponse{
		Ok: v.OK(), EventsChecked: int64(v.EventsChecked), FirstBrokenSeq: firstBroken,
	}), nil
}
