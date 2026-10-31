package gateway

import (
	"fmt"
	"strings"
	"testing"
)

func TestImportJobsHTTP(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	row := func(ref, name, hash string) map[string]any {
		return map[string]any{
			"source": map[string]any{"system": "legacy", "reference": ref, "row_hash": hash},
			"data":   map[string]any{"display_name": name, "labels": map[string]string{"Group": "G1"}, "stages": []map[string]string{{"stage": "development", "state": "OPEN"}}},
		}
	}
	body := map[string]any{"source": "legacy", "rows": []any{row("a", "A", "h1"), row("b", "B", "h2"), map[string]any{"source": map[string]any{"reference": "c"}, "data": map[string]any{}}}}

	f.wantErr(f.do(f.viewer, "POST", base+"/importJobs", body), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	f.wantErr(f.do(f.editor, "POST", base+"/importJobs", map[string]any{"source": "legacy", "rows": []any{}}), 400, "INVALID_ARGUMENT", "NO_ROWS")
	j := f.wantOK(f.do(f.editor, "POST", base+"/importJobs", body))
	name := str(j.body, "name")
	if !strings.HasPrefix(name, "projects/"+f.project+"/importJobs/") || str(j.body, "state") != "STAGED" {
		t.Fatal(j.raw)
	}
	sum := j.body["summary"].(map[string]any)
	if sum["NEW"] != float64(2) || sum["INVALID"] != float64(1) {
		t.Fatalf("summary %v", sum)
	}
	errs := j.body["row_errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["row_no"] != float64(3) || errs[0].(map[string]any)["reason"] != "INVALID_DISPLAY_NAME" {
		t.Fatalf("row_errors %v", errs)
	}
	path := "/v1/" + name

	// nothing is created by staging
	wl := f.wantOK(f.do(f.viewer, "GET", base+"/workflows", nil))
	if ws, _ := wl.body["workflows"].([]any); len(ws) != 0 {
		t.Fatal(wl.raw)
	}
	g := f.wantOK(f.do(f.viewer, "GET", path, nil))
	if str(g.body, "state") != "STAGED" || g.body["summary"].(map[string]any)["NEW"] != float64(2) {
		t.Fatal(g.raw)
	}
	f.wantErr(f.do(f.editor, "GET", base+"/importJobs/9999999", nil), 404, "NOT_FOUND", "NOT_FOUND")

	d := f.wantOK(f.do(f.editor, "GET", path+":diff?page_size=1&filter=action%3DNEW", nil))
	if rows := d.body["rows"].([]any); len(rows) != 1 || rows[0].(map[string]any)["action"] != "NEW" || str(d.body, "next_page_token") == "" {
		t.Fatal(d.raw)
	}
	f.wantErr(f.do(f.editor, "GET", path+":diff?filter=bogus", nil), 400, "INVALID_ARGUMENT", "INVALID_FILTER")
	f.wantErr(f.do(f.viewer, "GET", path+":diff", nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")

	// apply is admin only and returns an Operation
	f.wantErr(f.do(f.editor, "POST", path+":apply", nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	f.wantErr(f.do(f.editor, "POST", path+":rollback", nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	op := f.wantOK(f.do(f.admin, "POST", path+":apply", map[string]any{"note": "first load"}))
	resp := op.body["response"].(map[string]any)
	if op.body["done"] != true || op.body["name"] != name || resp["state"] != "APPLIED" || resp["summary"].(map[string]any)["CREATED"] != float64(2) {
		t.Fatal(op.raw)
	}
	wl = f.wantOK(f.do(f.viewer, "GET", base+"/workflows?filter=labels.group%3Dg1", nil))
	ws := wl.body["workflows"].([]any)
	if len(ws) != 2 {
		t.Fatal(wl.raw)
	}
	wfName := str(ws[0].(map[string]any), "name")
	ev := f.wantOK(f.do(f.viewer, "GET", "/v1/"+wfName+"/events", nil))
	e0 := ev.body["events"].([]any)[0].(map[string]any)
	if e0["channel"] != "IMPORT" || !strings.Contains(fmt.Sprint(e0["note"]), "import job") {
		t.Fatalf("event %v", e0)
	}
	// second apply: no-op, same answer
	op2 := f.wantOK(f.do(f.admin, "POST", path+":apply", nil))
	if op2.body["done"] != true {
		t.Fatal(op2.raw)
	}
	ev2 := f.wantOK(f.do(f.viewer, "GET", "/v1/"+wfName+"/events", nil))
	if len(ev2.body["events"].([]any)) != len(ev.body["events"].([]any)) {
		t.Fatal("second apply wrote events")
	}

	// a later person edit blocks the rollback, listing the workflow
	f.wantOK(f.do(f.editor, "PATCH", "/v1/"+wfName+"?update_mask=labels.priority", map[string]any{"workflow": map[string]any{"labels": map[string]string{"priority": "p1"}}}))
	r := f.do(f.admin, "POST", path+":rollback", nil)
	f.wantErr(r, 400, "FAILED_PRECONDITION", "ROLLBACK_BLOCKED")
	if !strings.Contains(r.raw, wfName) {
		t.Fatal(r.raw)
	}

	// a clean job rolls back
	j2 := f.wantOK(f.do(f.editor, "POST", base+"/importJobs", map[string]any{"source": "legacy2", "rows": []any{row("q", "Q", "hq")}}))
	p2 := "/v1/" + str(j2.body, "name")
	f.wantOK(f.do(f.admin, "POST", p2+":apply", nil))
	rb := f.wantOK(f.do(f.admin, "POST", p2+":rollback", nil))
	if rb.body["response"].(map[string]any)["state"] != "ROLLED_BACK" {
		t.Fatal(rb.raw)
	}
	wl = f.wantOK(f.do(f.viewer, "GET", base+"/workflows", nil))
	if len(wl.body["workflows"].([]any)) != 2 {
		t.Fatal(wl.raw)
	}
}
