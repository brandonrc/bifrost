package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonrc/bifrost/internal/auth"
	"github.com/brandonrc/bifrost/internal/controller"
	"github.com/brandonrc/bifrost/internal/core"
)

// --- CSV rendering: ported 1:1 from audit.rs's #[cfg(test)] module
// (csv_has_header_and_one_row_per_event, csv_quotes_commas_quotes_and_newlines,
// permission_target_role_strings_are_snake_case — the last of those lives
// in authz_test.go/rbac_test.go already via PermissionStr/TargetStr/RoleStr,
// so it isn't re-ported here). ---

func auditTestEvent(subject *string, roles []string) core.AuditEvent {
	reason := "insufficient_permission"
	path := "/api/v1/audit"
	status := uint16(403)
	return core.AuditEvent{
		Ts: 1_755_280_000, Subject: subject, Decision: core.AuditDecisionDeny,
		Reason: &reason, Path: &path, Status: &status,
		Required:     &core.AuditRequired{Action: "admin", Target: "cluster"},
		GrantedRoles: roles,
	}
}

func strPtr(s string) *string { return &s }

func TestRenderAuditCSV_HasHeaderAndOneRowPerEvent(t *testing.T) {
	csv := renderAuditCSV([]controller.AuditRow{
		{Seq: 7, Event: auditTestEvent(strPtr("alice"), []string{"viewer"})},
		{Seq: 3, Event: auditTestEvent(nil, nil)},
	})
	lines := strings.Split(strings.TrimRight(csv, "\n"), "\n")
	wantHeader := "seq,ts,subject,decision,reason,action,cluster,method,path,status,latency_ms,required_action,required_target,granted_roles"
	if lines[0] != wantHeader {
		t.Errorf("header = %q, want %q", lines[0], wantHeader)
	}
	wantRow1 := "7,1755280000,alice,deny,insufficient_permission,,,,/api/v1/audit,403,,admin,cluster,viewer"
	if lines[1] != wantRow1 {
		t.Errorf("row 1 = %q, want %q", lines[1], wantRow1)
	}
	wantRow2 := "3,1755280000,,deny,insufficient_permission,,,,/api/v1/audit,403,,admin,cluster,"
	if lines[2] != wantRow2 {
		t.Errorf("row 2 = %q, want %q", lines[2], wantRow2)
	}
}

func TestRenderAuditCSV_QuotesCommasQuotesAndNewlines(t *testing.T) {
	e := auditTestEvent(strPtr("a,b\"c\nd"), nil)
	plain := "/plain"
	e.Path = &plain
	csv := renderAuditCSV([]controller.AuditRow{{Seq: 1, Event: e}})
	if !strings.Contains(csv, "\"a,b\"\"c\nd\"") {
		t.Errorf("subject not RFC 4180-quoted: %s", csv)
	}
	if !strings.HasSuffix(csv, "/plain,403,,admin,cluster,\n") {
		t.Errorf("unexpected suffix: %s", csv)
	}
}

// --- Handler-level branch coverage ---

func auditor() *auth.Identity { return testIdentity("checker", auth.RoleAuditor) }

func TestListAuditEvents_AdminOrAuditorOnly(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	for _, id := range []*auth.Identity{testIdentity("v", auth.RoleViewer), testIdentity("op", auth.RoleOperator)} {
		_, err := s.ListAuditEvents(ctxWithIdentity(id), ListAuditEventsRequestObject{})
		if err == nil {
			t.Fatalf("%s: expected denial", id.Subject)
		}
		mustHTTPError(t, err, 403)
	}
	if _, err := s.ListAuditEvents(ctxWithIdentity(admin()), ListAuditEventsRequestObject{}); err != nil {
		t.Errorf("admin should be allowed: %v", err)
	}
	if _, err := s.ListAuditEvents(ctxWithIdentity(auditor()), ListAuditEventsRequestObject{}); err != nil {
		t.Errorf("auditor should be allowed: %v", err)
	}
}

func TestListAuditEvents_JSONThenCSVFormat(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	// The admin authz check itself and the pool/cluster fixtures below
	// each emit an audit row we can then list back.
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("audit-pool")}}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	resp, err := s.ListAuditEvents(ctxWithIdentity(admin()), ListAuditEventsRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	list := mustResponse[ListAuditEvents200JSONResponse](t, resp)
	if len(list.Items) == 0 {
		t.Fatal("expected at least the seeded create_pool event")
	}

	csvFormat := "csv"
	resp2, err := s.ListAuditEvents(ctxWithIdentity(admin()), ListAuditEventsRequestObject{Params: ListAuditEventsParams{Format: &csvFormat}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	csv := mustResponse[auditListCSVResponse](t, resp2)
	if !strings.HasPrefix(string(csv), "seq,ts,subject,decision,") {
		t.Errorf("csv export missing header: %s", csv)
	}
}

func TestListAuditEvents_UnknownFormatRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	bogus := "xml"
	_, err := s.ListAuditEvents(ctxWithIdentity(admin()), ListAuditEventsRequestObject{Params: ListAuditEventsParams{Format: &bogus}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestListAuditEvents_FromAfterToRejected(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	from, to := int64(1000), int64(500)
	_, err := s.ListAuditEvents(ctxWithIdentity(admin()), ListAuditEventsRequestObject{Params: ListAuditEventsParams{From: &from, To: &to}})
	if err == nil {
		t.Fatal("expected 400")
	}
	mustHTTPError(t, err, 400)
}

func TestVerifyAuditTrail_CleanChainVerifies(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("verify-pool")}}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	resp, err := s.VerifyAuditTrail(ctxWithIdentity(admin()), VerifyAuditTrailRequestObject{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v := mustResponse[VerifyAuditTrail200JSONResponse](t, resp)
	if !v.Ok {
		t.Errorf("chain should verify clean: %+v", v)
	}
	if v.EventsChecked < 1 {
		t.Errorf("events_checked = %d, want >= 1", v.EventsChecked)
	}
}

// TestListAuditEvents_CSVOverHTTP is an end-to-end round trip through a
// real http.Handler (rather than calling the handler method directly, as
// every other test in this file does) so the CSV export's custom
// auditListCSVResponse.VisitListAuditEventsResponse actually runs and
// writes the response — the property under test (content-type, the
// attachment header, the rendered body) only exists once a real
// ResponseWriter is involved.
func TestListAuditEvents_CSVOverHTTP(t *testing.T) {
	store := controller.NewMemoryStore()
	s := &Server{Store: store}
	if _, err := s.CreatePool(ctxWithIdentity(admin()), CreatePoolRequestObject{Body: &CreatePool{Spec: minimalPoolSpec("http-csv-pool")}}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	srv := httptest.NewServer(NewHandler(s, HandlerOptions{AllowUnauthenticated: true}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/audit?format=csv")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q, want text/csv", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="audit.csv"` {
		t.Errorf("content-disposition = %q", cd)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.HasPrefix(string(body), "seq,ts,subject,decision,") {
		t.Errorf("body = %s", body)
	}
}

func TestVerifyAuditTrail_AdminOrAuditorOnly(t *testing.T) {
	s := &Server{Store: newMemStore(t)}
	_, err := s.VerifyAuditTrail(ctxWithIdentity(testIdentity("dev", auth.RoleDeveloper)), VerifyAuditTrailRequestObject{})
	if err == nil {
		t.Fatal("expected denial")
	}
	mustHTTPError(t, err, 403)
}
