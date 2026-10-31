package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/store"
)

var seq atomic.Int64

type fixture struct {
	t       *testing.T
	srv     *httptest.Server
	st      *store.Store
	project string
	editor  string // delivery editor
	cust    string // customer editor
	viewer  string
	admin   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAllowGlobalConfig, "1") // the shared test database holds many projects
	f := &fixture{t: t, st: st, project: fmt.Sprintf("gw%d-%d", time.Now().UnixNano()%1e9, seq.Add(1))}
	mk := func(acct, side, role string) string {
		tok, err := st.CreateToken(ctx, f.project, acct+f.project, side, role, "human", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	f.editor, f.cust, f.viewer, f.admin = mk("ed", "delivery", "editor"), mk("cu", "customer", "editor"), mk("vi", "delivery", "viewer"), mk("ad", "delivery", "admin")
	f.srv = httptest.NewServer(New(st, nil))
	t.Cleanup(f.srv.Close)
	return f
}

type resp struct {
	code int
	body map[string]any
	raw  string
}

func (f *fixture) do(token, method, path string, body any) resp {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{code: r.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (r resp) errInfo() (status, reason string) {
	e, _ := r.body["error"].(map[string]any)
	if e == nil {
		return "", ""
	}
	status, _ = e["status"].(string)
	if d, _ := e["details"].([]any); len(d) > 0 {
		reason, _ = d[0].(map[string]any)["reason"].(string)
	}
	return
}

func (f *fixture) wantErr(r resp, code int, status, reason string) {
	f.t.Helper()
	s, rs := r.errInfo()
	if r.code != code || s != status || rs != reason {
		f.t.Fatalf("want %d %s/%s, got %d %s/%s: %s", code, status, reason, r.code, s, rs, r.raw)
	}
}

func (f *fixture) wantOK(r resp) resp {
	f.t.Helper()
	if r.code != 200 {
		f.t.Fatalf("want 200, got %d: %s", r.code, r.raw)
	}
	return r
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func TestHappyPath(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project

	// auth
	f.wantErr(f.do("", "GET", "/v1/me", nil), 401, "UNAUTHENTICATED", "UNAUTHENTICATED")
	f.wantErr(f.do("oetl_nope", "GET", "/v1/me", nil), 401, "UNAUTHENTICATED", "UNAUTHENTICATED")
	me := f.wantOK(f.do(f.editor, "GET", "/v1/me", nil))
	if !strings.HasPrefix(str(me.body, "account"), "accounts/ed") {
		t.Fatal(me.raw)
	}
	f.wantErr(f.do(f.editor, "GET", "/v1/projects/other-project/workflows", nil), 403, "PERMISSION_DENIED", "NOT_A_MEMBER")

	// create
	w := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{
		"display_name": "daily_loader", "labels": map[string]string{"Domain": "DWH", "priority": "B1"},
		"assignees": map[string]string{"dev_owner": "accounts/alice"}}))
	name := str(w.body, "name")
	if !strings.HasPrefix(name, "projects/"+f.project+"/workflows/wf-") || str(w.body, "scope") != "IN_SCOPE" || str(w.body, "health") != "NORMAL" ||
		str(w.body, "current_stage") != "development" || str(w.body, "etag") == "" {
		t.Fatal(w.raw)
	}
	if lb := w.body["labels"].(map[string]any); lb["domain"] != "dwh" || lb["priority"] != "b1" {
		t.Fatalf("labels not normalised: %v", lb)
	}
	if str(w.body, "creator") == "" || str(w.body, "create_time") == "" || len(w.body["stages"].([]any)) != 6 {
		t.Fatal(w.raw)
	}
	path := "/v1/" + name
	// case-insensitive id
	up := strings.Replace(path, "/wf-", "/WF-", 1)
	f.wantOK(f.do(f.viewer, "GET", up, nil))
	f.wantErr(f.do(f.viewer, "POST", base+"/workflows", map[string]any{"display_name": "x"}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")

	// advance with a stale etag, then the right one
	etag := str(w.body, "etag")
	a := f.wantOK(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "development", "etag": etag, "note": "Code merged"}))
	if str(a.body, "current_stage") != "deployment" || str(a.body, "etag") == etag {
		t.Fatal(a.raw)
	}
	if st := a.body["stages"].([]any)[0].(map[string]any); st["state"] != "PASSED" {
		t.Fatal(a.raw)
	}
	r := f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "deployment", "etag": etag})
	f.wantErr(r, 409, "ABORTED", "ETAG_MISMATCH")
	md := r.body["error"].(map[string]any)["details"].([]any)[0].(map[string]any)["metadata"].(map[string]any)
	if md["current_etag"] != str(a.body, "etag") || md["name"] != name {
		t.Fatalf("metadata %v", md)
	}
	// order, side rules
	f.wantErr(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "verify"}), 400, "FAILED_PRECONDITION", "STAGE_ORDER_VIOLATION")
	f.wantOK(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "deployment"}))
	f.wantOK(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "verify"}))
	f.wantErr(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "accept"}), 403, "PERMISSION_DENIED", "SIDE_NOT_ALLOWED")
	f.wantErr(f.do(f.viewer, "POST", path+":advanceStage", map[string]any{"stage": "accept"}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	f.wantOK(f.do(f.cust, "POST", path+":advanceStage", map[string]any{"stage": "accept"}))

	// regress
	f.wantErr(f.do(f.editor, "POST", path+":regressStage", map[string]any{"stages": []string{"verify"}}), 400, "FAILED_PRECONDITION", "REASON_REQUIRED")
	f.wantErr(f.do(f.editor, "POST", path+":regressStage", map[string]any{"stages": []string{"cutover"}, "reason": "rej_row_count"}), 400, "FAILED_PRECONDITION", "NOT_REGRESSIBLE")
	rg := f.wantOK(f.do(f.cust, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count", "note": "counts differ"}))
	if str(rg.body, "current_stage") != "accept" {
		t.Fatal(rg.raw)
	}
	// a regress may not leave a later stage PASSED, and there is no cascade
	f.wantOK(f.do(f.cust, "POST", path+":advanceStage", map[string]any{"stage": "accept"}))
	// an admin of the delivery side advances delivery stages like anyone (the customer's stages need evidence, see TestAdminOverride)
	f.wantOK(f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "dual_run"}))
	lr := f.do(f.cust, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count"})
	f.wantErr(lr, 400, "FAILED_PRECONDITION", "REGRESS_LATER_STAGES_PASSED")
	if md := lr.body["error"].(map[string]any)["details"].([]any)[0].(map[string]any)["metadata"].(map[string]any); md["stages"] != "dual_run" {
		t.Fatalf("metadata %v", md)
	}
	f.wantOK(f.do(f.admin, "POST", path+":regressStage", map[string]any{"stages": []string{"dual_run"}, "reason": "rej_row_count"}))
	f.wantOK(f.do(f.cust, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count"}))

	// INV-31 holds on every path: PATCH stages may not reopen a stage while a later one stays PASSED either
	f.wantOK(f.do(f.cust, "POST", path+":advanceStage", map[string]any{"stage": "accept"}))
	f.wantOK(f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "dual_run"}))
	reopen := func(stage string) resp {
		return f.do(f.editor, "PATCH", path+"?update_mask=stages", map[string]any{"reason": "rej_row_count",
			"workflow": map[string]any{"stages": []any{map[string]any{"stage": stage, "state": "OPEN"}}}})
	}
	pr := reopen("verify")
	f.wantErr(pr, 400, "FAILED_PRECONDITION", "REGRESS_LATER_STAGES_PASSED")
	if md := pr.body["error"].(map[string]any)["details"].([]any)[0].(map[string]any)["metadata"].(map[string]any); md["stages"] != "accept,dual_run" {
		t.Fatalf("metadata %v", md)
	}
	f.wantErr(reopen("development"), 400, "FAILED_PRECONDITION", "REGRESS_LATER_STAGES_PASSED")
	f.wantOK(f.do(f.admin, "POST", path+":regressStage", map[string]any{"stages": []string{"dual_run"}, "reason": "rej_row_count"}))
	f.wantOK(f.do(f.cust, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count"}))

	// comments -> health
	c := f.wantOK(f.do(f.editor, "POST", path+"/comments", map[string]any{"kind": "ISSUE", "body": "目标库权限未开通"}))
	cname := str(c.body, "name")
	if !strings.HasPrefix(cname, name+"/comments/cm-") || c.body["resolved"] != false || str(c.body, "side") != "DELIVERY" {
		t.Fatal(c.raw)
	}
	g := f.wantOK(f.do(f.viewer, "GET", path, nil))
	if str(g.body, "health") != "BLOCKED" {
		t.Fatal(g.raw)
	}
	f.wantErr(f.do(f.editor, "POST", path+"/comments", map[string]any{"kind": "BOGUS", "body": "x"}), 400, "INVALID_ARGUMENT", "INVALID_KIND")
	cl := f.wantOK(f.do(f.viewer, "GET", path+"/comments?filter=kind%3DISSUE", nil))
	if len(cl.body["comments"].([]any)) != 1 {
		t.Fatal(cl.raw)
	}
	rs := f.wantOK(f.do(f.editor, "POST", "/v1/"+cname+":resolve", nil))
	if rs.body["resolved"] != true || str(rs.body, "resolve_time") == "" {
		t.Fatal(rs.raw)
	}
	if g := f.wantOK(f.do(f.viewer, "GET", path, nil)); str(g.body, "health") != "NORMAL" {
		t.Fatal(g.raw)
	}
	f.wantOK(f.do(f.editor, "POST", "/v1/"+cname+":reopen", nil))
	f.wantOK(f.do(f.editor, "DELETE", "/v1/"+cname, nil))
	f.wantErr(f.do(f.editor, "GET", "/v1/"+cname, nil), 404, "NOT_FOUND", "NOT_FOUND")
	if g := f.wantOK(f.do(f.viewer, "GET", path, nil)); str(g.body, "health") != "NORMAL" {
		t.Fatal("deleted comment still blocks")
	}

	// query (viewers may read)
	q := f.wantOK(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT stage, health, workflows FROM stage_distribution"}))
	rows := q.body["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != "accept" || rows[0].([]any)[1] != "ok" || rows[0].([]any)[2] != float64(1) {
		t.Fatalf("query: %s", q.raw)
	}
	cols := q.body["columns"].([]any)
	if len(cols) != 3 || cols[0].(map[string]any)["name"] != "stage" {
		t.Fatal(q.raw)
	}
	q = f.wantOK(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT code, name, attrs FROM workflows"}))
	row := q.body["rows"].([]any)[0].([]any)
	if !strings.HasPrefix(row[0].(string), "WF-") || row[2].(map[string]any)["domain"] != "dwh" {
		t.Fatalf("query: %s", q.raw)
	}
	f.wantErr(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT * FROM entity"}), 400, "INVALID_ARGUMENT", "INVALID_QUERY")
	f.wantErr(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "DROP TABLE entity"}), 400, "INVALID_ARGUMENT", "INVALID_QUERY")
	f.wantErr(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT 1", "as_of": "2026-01-01T00:00:00Z"}), 400, "INVALID_ARGUMENT", "AS_OF_UNSUPPORTED")

	// events
	ev := f.wantOK(f.do(f.viewer, "GET", path+"/events?page_size=3", nil))
	evs := ev.body["events"].([]any)
	if len(evs) != 3 || str(ev.body, "next_page_token") == "" {
		t.Fatal(ev.raw)
	}
	first := evs[0].(map[string]any)
	if first["channel"] != "API" || first["side"] != "DELIVERY" || len(first["changes"].([]any)) == 0 {
		t.Fatal(ev.raw)
	}
	f.wantOK(f.do(f.viewer, "GET", "/v1/"+str(first, "name"), nil))
	ev2 := f.wantOK(f.do(f.viewer, "GET", path+"/events?page_size=100&page_token="+str(ev.body, "next_page_token"), nil))
	if len(ev2.body["events"].([]any)) < 5 {
		t.Fatal(ev2.raw)
	}
	asOf := f.wantOK(f.do(f.viewer, "GET", path+"/events?as_of="+str(first, "create_time"), nil))
	if n := len(asOf.body["events"].([]any)); n < 1 || n > 2 {
		t.Fatalf("as_of events: %d", n)
	}
	f.wantErr(f.do(f.viewer, "GET", path+"/events?as_of=yesterday", nil), 400, "INVALID_ARGUMENT", "INVALID_AS_OF")
}

func TestValidateOnlyAndIdempotencyHTTP(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	v := f.wantOK(f.do(f.editor, "POST", base+"/workflows?validate_only=true", map[string]any{"display_name": "ghost"}))
	if str(v.body, "display_name") != "ghost" || str(v.body, "name") != "" {
		t.Fatal(v.raw)
	}
	l := f.wantOK(f.do(f.editor, "GET", base+"/workflows", nil))
	if n := len(l.body["workflows"].([]any)); n != 0 {
		t.Fatalf("validate_only persisted %d workflows", n)
	}
	rid := "7d1f3c52-2f0e-4c53-9d1a-6a9c0f3a1b11"
	a := f.wantOK(f.do(f.editor, "POST", base+"/workflows?request_id="+rid, map[string]any{"display_name": "once"}))
	b := f.wantOK(f.do(f.editor, "POST", base+"/workflows?request_id="+rid, map[string]any{"display_name": "once"}))
	if str(a.body, "name") != str(b.body, "name") {
		t.Fatal("replay created a second workflow")
	}
	path := "/v1/" + str(a.body, "name")
	// validate_only advance reports rule failures and previews success
	f.wantErr(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "verify", "validate_only": true}), 400, "FAILED_PRECONDITION", "STAGE_ORDER_VIOLATION")
	p := f.wantOK(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "development", "validate_only": true}))
	if str(p.body, "current_stage") != "deployment" {
		t.Fatal(p.raw)
	}
	g := f.wantOK(f.do(f.editor, "GET", path, nil))
	if str(g.body, "current_stage") != "development" || str(g.body, "etag") != str(a.body, "etag") {
		t.Fatal("validate_only changed state")
	}
}

func TestUpdateScopeDeleteTasks(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	mkwf := func(n string) (string, string) {
		w := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{"display_name": n, "labels": map[string]string{"domain": "dwh"}}))
		return str(w.body, "name"), str(w.body, "etag")
	}
	n1, e1 := mkwf("one")
	n2, _ := mkwf("two")

	// patch with mask
	u := f.wantOK(f.do(f.editor, "PATCH", "/v1/"+n1+"?update_mask=display_name,labels", map[string]any{
		"etag": e1, "workflow": map[string]any{"display_name": "uno", "labels": map[string]string{"area": "x"}}}))
	if str(u.body, "display_name") != "uno" {
		t.Fatal(u.raw)
	}
	lb := u.body["labels"].(map[string]any)
	if lb["area"] != "x" || lb["domain"] != nil {
		t.Fatalf("labels replace failed: %v", lb)
	}
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+n1+"?update_mask=scope", map[string]any{"workflow": map[string]any{"display_name": "x"}}), 400, "INVALID_ARGUMENT", "INVALID_UPDATE_MASK")
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+n1, map[string]any{"workflow": map[string]any{"display_name": "x"}}), 400, "INVALID_ARGUMENT", "INVALID_UPDATE_MASK")
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+n1+"?update_mask=display_name", map[string]any{"etag": e1, "workflow": map[string]any{"display_name": "stale"}}), 409, "ABORTED", "ETAG_MISMATCH")

	// stage via patch needs the right reason kinds
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+n1+"?update_mask=stages", map[string]any{"workflow": map[string]any{"display_name": "x", "stages": []any{map[string]any{"stage": "verify", "state": "NOT_APPLICABLE"}}}}), 400, "FAILED_PRECONDITION", "REASON_REQUIRED")
	f.wantOK(f.do(f.editor, "PATCH", "/v1/"+n1+"?update_mask=stages", map[string]any{"reason": "na_manual_trigger", "note": "manual", "workflow": map[string]any{"display_name": "x", "stages": []any{map[string]any{"stage": "development", "state": "NOT_APPLICABLE"}}}}))

	// scope
	f.wantErr(f.do(f.editor, "POST", "/v1/"+n1+":setScope", map[string]any{"scope": "OUT_OF_SCOPE"}), 400, "FAILED_PRECONDITION", "REASON_REQUIRED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+n1+":setScope", map[string]any{"scope": "OUT_OF_SCOPE", "reason": "merged"}), 400, "INVALID_ARGUMENT", "MERGE_TARGET_REQUIRED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+n1+":setScope", map[string]any{"scope": "OUT_OF_SCOPE", "reason": "merged", "merged_into": n1}), 400, "FAILED_PRECONDITION", "MERGE_CYCLE")
	m := f.wantOK(f.do(f.editor, "POST", "/v1/"+n1+":setScope", map[string]any{"scope": "OUT_OF_SCOPE", "reason": "merged", "merged_into": n2}))
	if str(m.body, "scope") != "OUT_OF_SCOPE" || str(m.body, "merged_into") != n2 {
		t.Fatal(m.raw)
	}
	f.wantErr(f.do(f.editor, "POST", "/v1/"+n2+":setScope", map[string]any{"scope": "OUT_OF_SCOPE", "reason": "merged", "merged_into": n1}), 400, "FAILED_PRECONDITION", "MERGE_CYCLE")
	back := f.wantOK(f.do(f.editor, "POST", "/v1/"+n1+":setScope", map[string]any{"scope": "IN_SCOPE"}))
	if str(back.body, "scope") != "IN_SCOPE" || back.body["merged_into"] != nil {
		t.Fatal(back.raw)
	}
	l := f.wantOK(f.do(f.editor, "GET", base+"/workflows?filter=scope%3DOUT_OF_SCOPE", nil))
	if len(l.body["workflows"].([]any)) != 0 {
		t.Fatal(l.raw)
	}

	// delete / undelete / show_deleted
	d := f.wantOK(f.do(f.editor, "DELETE", "/v1/"+n2, nil))
	if str(d.body, "delete_time") == "" {
		t.Fatal(d.raw)
	}
	l = f.wantOK(f.do(f.editor, "GET", base+"/workflows", nil))
	if len(l.body["workflows"].([]any)) != 1 {
		t.Fatal(l.raw)
	}
	l = f.wantOK(f.do(f.editor, "GET", base+"/workflows?show_deleted=true&filter=display_name:two", nil))
	if len(l.body["workflows"].([]any)) != 1 {
		t.Fatal(l.raw)
	}
	un := f.wantOK(f.do(f.editor, "POST", "/v1/"+n2+":undelete", nil))
	if un.body["delete_time"] != nil {
		t.Fatal(un.raw)
	}

	// tasks
	f.wantErr(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "no_workflow"}), 400, "INVALID_ARGUMENT", "INVALID_WORKFLOW")
	f.wantErr(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "x", "workflow": "projects/" + f.project + "/workflows/wf-999999"}), 400, "INVALID_ARGUMENT", "WORKFLOW_NOT_FOUND")
	t1 := f.wantOK(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "load_orders", "labels": map[string]string{"kind": "etl"}, "workflow": n1}))
	tname := str(t1.body, "name")
	if !strings.HasPrefix(tname, "projects/"+f.project+"/tasks/tk-") || t1.body["workflow"] != n1 || t1.body["workflows"] != nil {
		t.Fatal(t1.raw)
	}
	as := f.wantOK(f.do(f.editor, "POST", "/v1/"+tname+":assign", map[string]any{"workflow": n2}))
	if as.body["workflow"] != n2 {
		t.Fatal(as.raw)
	}
	tl := f.wantOK(f.do(f.viewer, "GET", base+"/tasks?filter="+urlEnc(`workflow = "`+n2+`"`), nil))
	if len(tl.body["tasks"].([]any)) != 1 {
		t.Fatal(tl.raw)
	}
	tl = f.wantOK(f.do(f.viewer, "GET", base+"/tasks?filter="+urlEnc(`workflow = "`+n1+`"`), nil))
	if len(tl.body["tasks"].([]any)) != 0 {
		t.Fatal(tl.raw)
	}
	tu := f.wantOK(f.do(f.editor, "PATCH", "/v1/"+tname+"?update_mask=display_name", map[string]any{"task": map[string]any{"display_name": "load_orders_v2"}}))
	if str(tu.body, "display_name") != "load_orders_v2" {
		t.Fatal(tu.raw)
	}
	f.wantOK(f.do(f.editor, "DELETE", "/v1/"+tname, nil))
	f.wantErr(f.do(f.editor, "POST", "/v1/"+tname+":assign", map[string]any{"workflow": "projects/" + f.project + "/workflows/wf-999999"}), 400, "INVALID_ARGUMENT", "WORKFLOW_NOT_FOUND")
	tl = f.wantOK(f.do(f.viewer, "GET", base+"/tasks", nil))
	if len(tl.body["tasks"].([]any)) != 0 {
		t.Fatal("deleted task listed")
	}
	f.wantOK(f.do(f.editor, "POST", "/v1/"+tname+":undelete", nil))
}

func urlEnc(s string) string {
	r := strings.NewReplacer(" ", "%20", `"`, "%22", "=", "%3D", ":", "%3A", "/", "%2F")
	return r.Replace(s)
}

func TestNotImplementedAndErrors(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	for _, p := range []struct{ method, path string }{} {
		r := f.do(f.admin, p.method, p.path, map[string]any{})
		if r.code != 501 {
			t.Errorf("%s %s: want 501, got %d %s", p.method, p.path, r.code, r.raw)
			continue
		}
		f.wantErr(r, 501, "UNIMPLEMENTED", "UNIMPLEMENTED")
	}
	f.wantErr(f.do(f.editor, "GET", base+"/workflows/wf-999999", nil), 404, "NOT_FOUND", "NOT_FOUND")
	f.wantErr(f.do(f.editor, "GET", base+"/workflows/bogus", nil), 404, "NOT_FOUND", "NOT_FOUND")
	f.wantErr(f.do(f.editor, "GET", base+"/nothing", nil), 404, "NOT_FOUND", "NOT_FOUND")
	f.wantErr(f.do(f.editor, "GET", base+"/workflows?filter=bogus%3D1", nil), 400, "INVALID_ARGUMENT", "INVALID_FILTER")
	f.wantErr(f.do(f.editor, "GET", base+"/workflows?page_token=zzz", nil), 400, "INVALID_ARGUMENT", "INVALID_PAGE_TOKEN")
	f.wantErr(f.do(f.editor, "POST", base+"/workflows", map[string]any{"labels": map[string]string{"a": "b"}}), 400, "INVALID_ARGUMENT", "INVALID_DISPLAY_NAME")
	f.wantErr(f.do(f.editor, "POST", base+"/workflows?workflow_id=custom", map[string]any{"display_name": "x"}), 400, "INVALID_ARGUMENT", "CLIENT_ID_UNSUPPORTED")
	f.wantErr(f.do(f.editor, "PATCH", base+"/workflows/wf-000001:advanceStage", map[string]any{}), 400, "INVALID_ARGUMENT", "METHOD_NOT_ALLOWED")
	f.wantErr(f.do(f.editor, "POST", base+"/workflows/wf-000001:explode", map[string]any{}), 404, "NOT_FOUND", "NOT_FOUND")
	r := f.do(f.editor, "POST", base+"/workflows", nil)
	f.wantErr(r, 400, "INVALID_ARGUMENT", "INVALID_BODY")
	// event channel header is honoured, only cli|api
	req, _ := http.NewRequest("POST", f.srv.URL+base+"/workflows", strings.NewReader(`{"display_name":"via-cli"}`))
	req.Header.Set("Authorization", "Bearer "+f.editor)
	req.Header.Set("X-Openetl-Channel", "import")
	rr, _ := http.DefaultClient.Do(req)
	rr.Body.Close()
	var ch string
	f.st.DB.QueryRow(`SELECT ev.channel FROM event ev JOIN project p ON p.id = ev.project_id WHERE p.name = ? ORDER BY ev.id DESC LIMIT 1`, f.project).Scan(&ch)
	if ch != "api" {
		t.Fatalf("clients must not be able to claim the import channel, got %q", ch)
	}
}

func TestConcurrentEtagWriters(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	w := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{"display_name": "race"}))
	path, etag := "/v1/"+str(w.body, "name"), str(w.body, "etag")
	codes := make(chan int, 6)
	for i := 0; i < 6; i++ {
		go func(i int) {
			r := f.do(f.editor, "PATCH", path+"?update_mask=display_name", map[string]any{"etag": etag, "workflow": map[string]any{"display_name": fmt.Sprintf("n%d", i)}})
			codes <- r.code
		}(i)
	}
	ok, conflict := 0, 0
	for i := 0; i < 6; i++ {
		switch <-codes {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != 5 {
		t.Fatalf("exactly one writer may win an etag: ok=%d conflict=%d", ok, conflict)
	}
}

// A soft-deleted workflow or task is read-only except for its restore; a comment of a deleted workflow too.
func TestDeletedEntitiesRejectWrites(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	w := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{"display_name": "w"}))
	wn := str(w.body, "name")
	w2 := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{"display_name": "w2"}))
	c := f.wantOK(f.do(f.editor, "POST", "/v1/"+wn+"/comments", map[string]any{"kind": "ISSUE", "body": "b"}))
	tk := f.wantOK(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "t", "workflow": wn}))
	tn := str(tk.body, "name")
	w2n := str(w2.body, "name")

	// a task has one workflow: create needs it, :assign moves it
	f.wantErr(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "t2"}), 400, "INVALID_ARGUMENT", "INVALID_WORKFLOW")
	mv := f.wantOK(f.do(f.editor, "POST", "/v1/"+tn+":assign", map[string]any{"workflow": w2n}))
	if mv.body["workflow"] != w2n {
		t.Fatalf("moved task is in %v: %s", mv.body["workflow"], mv.raw)
	}
	f.wantOK(f.do(f.editor, "POST", "/v1/"+tn+":assign", map[string]any{"workflow": wn}))

	f.wantOK(f.do(f.editor, "DELETE", "/v1/"+wn, nil)) // takes the task along (INV-30)
	f.wantOK(f.do(f.editor, "DELETE", "/v1/"+tn, nil))
	pre := "FAILED_PRECONDITION"
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+wn+"?update_mask=display_name", map[string]any{"workflow": map[string]any{"display_name": "x"}}), 400, pre, "ENTITY_DELETED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+wn+":advanceStage", map[string]any{"stage": "development"}), 400, pre, "ENTITY_DELETED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+wn+":setScope", map[string]any{"scope": "OUT_OF_SCOPE", "reason": "obsolete"}), 400, pre, "ENTITY_DELETED")
	f.wantErr(f.do(f.editor, "PATCH", "/v1/"+tn+"?update_mask=display_name", map[string]any{"task": map[string]any{"display_name": "x"}}), 400, pre, "ENTITY_DELETED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+tn+":assign", map[string]any{"workflow": w2n}), 400, pre, "ENTITY_DELETED")
	f.wantErr(f.do(f.editor, "POST", "/v1/"+str(c.body, "name")+":resolve", nil), 400, pre, "ENTITY_DELETED")
	f.wantOK(f.do(f.editor, "GET", "/v1/"+wn, nil)) // reads stay
	f.wantErr(f.do(f.editor, "POST", "/v1/"+tn+":undelete", nil), 400, pre, "PARENT_DELETED")
	f.wantErr(f.do(f.editor, "POST", base+"/tasks", map[string]any{"display_name": "late", "workflow": wn}), 400, pre, "ENTITY_DELETED")
	f.wantOK(f.do(f.editor, "POST", "/v1/"+wn+":undelete", nil))
	f.wantOK(f.do(f.editor, "POST", "/v1/"+tn+":undelete", nil))
	f.wantOK(f.do(f.editor, "POST", "/v1/"+str(c.body, "name")+":resolve", nil))
	f.wantOK(f.do(f.editor, "PATCH", "/v1/"+wn+"?update_mask=display_name", map[string]any{"workflow": map[string]any{"display_name": "x"}}))
}
