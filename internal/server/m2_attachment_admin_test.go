package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/storage"
)

// withStorage returns a copy of the fixture whose server uses a temp-dir fs backend.
func withStorage(t *testing.T, f *fixture) *fixture {
	t.Helper()
	fs, err := storage.NewFS(t.TempDir(), []byte("server-test-secret"), "")
	if err != nil {
		t.Fatal(err)
	}
	g := New(f.st, nil)
	g.Storage = fs
	c := *f
	c.srv = httptest.NewServer(g)
	t.Cleanup(c.srv.Close)
	return &c
}

func rawReq(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, b
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (f *fixture) newWorkflow(name string) string {
	f.t.Helper()
	w := f.wantOK(f.do(f.editor, "POST", "/v1/projects/"+f.project+"/workflows", map[string]any{"display_name": name}))
	return "/v1/" + str(w.body, "name")
}

// upload runs initUpload and the PUT; it returns the storage key.
func (f *fixture) upload(wfPath string, data []byte) string {
	f.t.Helper()
	ini := f.wantOK(f.do(f.editor, "POST", wfPath+"/attachments:initUpload", map[string]any{"filename": "evidence.txt", "size": len(data), "sha256": sha(data)}))
	code, b := rawReq(f.t, "PUT", str(ini.body, "upload_url"), data)
	if code != 200 {
		f.t.Fatalf("PUT: %d %s", code, b)
	}
	return str(ini.body, "storage_key")
}

func TestAttachmentFlowAndEvidence(t *testing.T) {
	f := withStorage(t, newFixture(t))
	wf := f.newWorkflow("att_wf")
	other := f.newWorkflow("att_other")
	data := []byte("row counts match: 42 = 42\n")

	// initUpload validation
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "big.bin", "size": 100<<20 + 1}), 400, "INVALID_ARGUMENT", "FILE_TOO_LARGE")
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "x", "size": 0}), 400, "INVALID_ARGUMENT", "INVALID_SIZE")
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "", "size": 5}), 400, "INVALID_ARGUMENT", "INVALID_FILENAME")
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "x", "size": 5, "sha256": "ZZ"}), 400, "INVALID_ARGUMENT", "INVALID_SHA256")
	f.wantErr(f.do(f.viewer, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "x", "size": 5}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	f.wantErr(f.do(f.editor, "POST", "/v1/projects/"+f.project+"/workflows/wf-999999/attachments:initUpload", map[string]any{"filename": "x", "size": 5}), 404, "NOT_FOUND", "NOT_FOUND")
	f.wantOK(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "ok.bin", "size": 100 << 20}))

	// registering before the upload
	ini := f.wantOK(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "evidence.txt", "size": len(data), "sha256": sha(data)}))
	key := str(ini.body, "storage_key")
	reg := map[string]any{"filename": "evidence.txt", "size": len(data), "sha256": sha(data), "storage_key": key}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", reg), 400, "FAILED_PRECONDITION", "UPLOAD_NOT_FOUND")

	// wrong signature on the upload URL is refused, the right one works
	if code, _ := rawReq(t, "PUT", strings.Replace(str(ini.body, "upload_url"), "sig=", "sig=00", 1), data); code != 403 {
		t.Fatalf("tampered upload url: %d", code)
	}
	if code, _ := rawReq(t, "PUT", str(ini.body, "upload_url"), []byte("short")); code == 200 {
		t.Fatal("wrong-size upload accepted")
	}
	if code, b := rawReq(t, "PUT", str(ini.body, "upload_url"), data); code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}

	// mismatches
	bad := map[string]any{"filename": "evidence.txt", "size": len(data) + 1, "sha256": sha(data), "storage_key": key}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", bad), 400, "FAILED_PRECONDITION", "SIZE_MISMATCH")
	bad = map[string]any{"filename": "evidence.txt", "size": len(data), "sha256": sha([]byte("other")), "storage_key": key}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", bad), 400, "FAILED_PRECONDITION", "SHA256_MISMATCH")
	// key of another workflow / traversal
	f.wantErr(f.do(f.editor, "POST", other+"/attachments", reg), 400, "INVALID_ARGUMENT", "INVALID_STORAGE_KEY")
	traversal := map[string]any{"filename": "x", "size": 1, "sha256": sha(data), "storage_key": key + "/../../x"}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", traversal), 400, "INVALID_ARGUMENT", "INVALID_STORAGE_KEY")

	// validate_only creates nothing
	vo := f.wantOK(f.do(f.editor, "POST", wf+"/attachments?validate_only=true", reg))
	if str(vo.body, "name") != "" {
		t.Fatalf("validate_only created: %s", vo.raw)
	}
	att := f.wantOK(f.do(f.editor, "POST", wf+"/attachments", reg))
	aname := str(att.body, "name")
	if !strings.Contains(aname, "/attachments/at-") || str(att.body, "creator") == "" || str(att.body, "sha256") != sha(data) {
		t.Fatal(att.raw)
	}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", reg), 409, "ALREADY_EXISTS", "STORAGE_KEY_REGISTERED")

	// download: time-limited URL returns the bytes
	dl := f.wantOK(f.do(f.viewer, "GET", "/v1/"+aname+":download", nil))
	code, b := rawReq(t, "GET", str(dl.body, "download_url"), nil)
	if code != 200 || !bytes.Equal(b, data) {
		t.Fatalf("download: %d %q", code, b)
	}
	f.wantErr(f.do(f.viewer, "GET", other+"/attachments/"+aname[strings.LastIndex(aname, "/")+1:]+":download", nil), 404, "NOT_FOUND", "NOT_FOUND")
	f.wantErr(f.do(f.viewer, "GET", wf+"/attachments/at-999999:download", nil), 404, "NOT_FOUND", "NOT_FOUND")

	// the registration is an audited event on the workflow
	evs := f.wantOK(f.do(f.viewer, "GET", wf+"/events", nil))
	found := false
	for _, e := range evs.body["events"].([]any) {
		if str(e.(map[string]any), "target") == aname {
			found = true
		}
	}
	if !found {
		t.Fatalf("attachment event missing: %s", evs.raw)
	}

	// evidence on advanceStage: validated and recorded on the event
	f.wantErr(f.do(f.editor, "POST", wf+":advanceStage", map[string]any{"stage": "development", "evidence_attachments": []string{"at-999999"}}), 400, "INVALID_ARGUMENT", "INVALID_EVIDENCE")
	f.wantErr(f.do(f.editor, "POST", wf+":advanceStage", map[string]any{"stage": "development", "evidence_attachments": []string{"projects/other/workflows/wf-000001/attachments/at-000001"}}), 400, "INVALID_ARGUMENT", "INVALID_EVIDENCE")
	okec := key2(t, f, other, []byte("elsewhere"))
	f.wantErr(f.do(f.editor, "POST", wf+":advanceStage", map[string]any{"stage": "development", "evidence_attachments": []string{okec}}), 400, "INVALID_ARGUMENT", "INVALID_EVIDENCE")
	f.wantOK(f.do(f.editor, "POST", wf+":advanceStage", map[string]any{"stage": "development", "evidence_attachments": []string{aname, aname[strings.LastIndex(aname, "/")+1:]}}))
	evs = f.wantOK(f.do(f.viewer, "GET", wf+"/events", nil))
	recorded := false
	for _, e := range evs.body["events"].([]any) {
		for _, c := range e.(map[string]any)["changes"].([]any) {
			c := c.(map[string]any)
			if c["key"] == "evidence.development" {
				l, _ := c["new_value"].([]any)
				recorded = len(l) == 1 && l[0] == aname
			}
		}
	}
	if !recorded {
		t.Fatalf("evidence not recorded: %s", evs.raw)
	}
}

// key2 uploads+registers an attachment on another workflow and returns its resource name.
func key2(t *testing.T, f *fixture, wfPath string, data []byte) string {
	t.Helper()
	key := f.upload(wfPath, data)
	a := f.wantOK(f.do(f.editor, "POST", wfPath+"/attachments", map[string]any{"filename": "e.txt", "size": len(data), "sha256": sha(data), "storage_key": key}))
	return str(a.body, "name")
}

// ---------------------------------------------------------------- admin

func TestAdminMembersAndTokens(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	acct := func(prefix string) string { return prefix + f.project }

	// only admins
	for _, tok := range []string{f.editor, f.viewer, f.cust} {
		f.wantErr(f.do(tok, "GET", base+"/members", nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
		// every member may read stages/reasons/rules (an editor must be able to find a valid reason key), never settings
		cfg := f.wantOK(f.do(tok, "GET", base+"/config", nil))
		if cfg.body["settings"] != nil || len(cfg.body["reasons"].([]any)) == 0 {
			t.Fatalf("non-admin config read must hide settings and include reasons: %v", cfg.body)
		}
		f.wantErr(f.do(tok, "PATCH", base+"/config?update_mask=settings", map[string]any{"settings": map[string]any{"a": "b"}}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
		f.wantErr(f.do(tok, "POST", "/v1/accounts/"+acct("ed")+"/tokens", map[string]any{}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	}

	ml := f.wantOK(f.do(f.admin, "GET", base+"/members?page_size=3", nil))
	if len(ml.body["members"].([]any)) != 3 || str(ml.body, "next_page_token") == "" {
		t.Fatal(ml.raw)
	}
	ml2 := f.wantOK(f.do(f.admin, "GET", base+"/members?page_size=3&page_token="+str(ml.body, "next_page_token"), nil))
	if len(ml2.body["members"].([]any)) != 1 {
		t.Fatal(ml2.raw)
	}

	// add, change, remove
	m := f.wantOK(f.do(f.admin, "PUT", base+"/members/newbie", map[string]any{"role": "EDITOR", "side": "CUSTOMER"}))
	if str(m.body, "name") != "projects/"+f.project+"/members/newbie" || str(m.body, "role") != "EDITOR" || str(m.body, "side") != "CUSTOMER" {
		t.Fatal(m.raw)
	}
	m = f.wantOK(f.do(f.admin, "PUT", base+"/members/newbie", map[string]any{"role": "VIEWER", "side": "DELIVERY"}))
	if str(m.body, "role") != "VIEWER" || str(m.body, "side") != "DELIVERY" {
		t.Fatal(m.raw)
	}
	f.wantErr(f.do(f.admin, "PUT", base+"/members/newbie", map[string]any{"role": "ROLE_UNSPECIFIED", "side": "DELIVERY"}), 400, "INVALID_ARGUMENT", "INVALID_ROLE")
	f.wantErr(f.do(f.admin, "PUT", base+"/members/newbie", map[string]any{"role": "EDITOR", "side": "SIDE_UNSPECIFIED"}), 400, "INVALID_ARGUMENT", "INVALID_SIDE")
	f.wantErr(f.do(f.admin, "PUT", base+"/members/bad%20name", map[string]any{"role": "EDITOR", "side": "DELIVERY"}), 400, "INVALID_ARGUMENT", "INVALID_ACCOUNT")
	if r := f.do(f.admin, "DELETE", base+"/members/newbie", nil); r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.raw)
	}
	f.wantErr(f.do(f.admin, "DELETE", base+"/members/newbie", nil), 404, "NOT_FOUND", "NOT_FOUND")

	// agents cannot be admin
	if _, err := f.st.CreateToken(context.Background(), f.project, "bot"+f.project, "delivery", "editor", "agent", 3600e9); err != nil {
		t.Fatal(err)
	}
	f.wantErr(f.do(f.admin, "PUT", base+"/members/"+acct("bot"), map[string]any{"role": "ADMIN", "side": "DELIVERY"}), 400, "FAILED_PRECONDITION", "AGENT_CANNOT_BE_ADMIN")
	// the last admin stays
	f.wantErr(f.do(f.admin, "PUT", base+"/members/"+acct("ad"), map[string]any{"role": "EDITOR", "side": "DELIVERY"}), 400, "FAILED_PRECONDITION", "LAST_ADMIN")
	f.wantErr(f.do(f.admin, "DELETE", base+"/members/"+acct("ad"), nil), 400, "FAILED_PRECONDITION", "LAST_ADMIN")

	// tokens
	tp := "/v1/accounts/" + acct("ed") + "/tokens"
	created := f.wantOK(f.do(f.admin, "POST", tp, map[string]any{}))
	secret := str(created.body, "secret")
	if !strings.HasPrefix(secret, "oetl_") || !strings.HasPrefix(str(created.body, "name"), "accounts/"+acct("ed")+"/tokens/") {
		t.Fatal(created.raw)
	}
	f.wantOK(f.do(secret, "GET", "/v1/me", nil))
	lst := f.wantOK(f.do(f.admin, "GET", tp, nil))
	if strings.Contains(lst.raw, secret) || strings.Contains(lst.raw, "secret") || len(lst.body["tokens"].([]any)) != 2 {
		t.Fatalf("list leaks or miscounts: %s", lst.raw)
	}
	f.wantErr(f.do(f.admin, "POST", tp, map[string]any{"expire_time": "2001-01-01T00:00:00Z"}), 400, "INVALID_ARGUMENT", "INVALID_EXPIRE_TIME")
	rv := f.wantOK(f.do(f.admin, "POST", "/v1/"+str(created.body, "name")+":revoke", nil))
	if str(rv.body, "revoke_time") == "" {
		t.Fatal(rv.raw)
	}
	f.wantOK(f.do(f.admin, "POST", "/v1/"+str(created.body, "name")+":revoke", nil)) // idempotent
	f.wantErr(f.do(secret, "GET", "/v1/me", nil), 401, "UNAUTHENTICATED", "UNAUTHENTICATED")
	f.wantErr(f.do(f.admin, "POST", tp+"/999999999:revoke", nil), 404, "NOT_FOUND", "NOT_FOUND")
	// an admin of project A has no power over accounts of an unrelated project
	f.wantErr(f.do(f.admin, "POST", "/v1/accounts/nobody-"+f.project+"/tokens", map[string]any{}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
}

func TestAdminConfig(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project + "/config"
	cfg := f.wantOK(f.do(f.admin, "GET", base, nil))
	if len(cfg.body["stages"].([]any)) < 6 || len(cfg.body["reasons"].([]any)) < 20 || len(cfg.body["rules"].([]any)) < 10 || str(cfg.body, "etag") == "" {
		t.Fatal(cfg.raw)
	}
	f.wantErr(f.do(f.admin, "PATCH", base, map[string]any{}), 400, "INVALID_ARGUMENT", "INVALID_UPDATE_MASK")
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=bogus", map[string]any{}), 400, "INVALID_ARGUMENT", "INVALID_UPDATE_MASK")

	// settings: merge, empty value deletes
	key := fmt.Sprintf("test.note-%s", f.project)
	s := f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"settings": map[string]string{key: "v1"}}))
	if s.body["settings"].(map[string]any)[key] != "v1" || str(s.body, "etag") == str(cfg.body, "etag") {
		t.Fatal(s.raw)
	}
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"etag": str(cfg.body, "etag"), "settings": map[string]string{key: "v2"}}), 409, "ABORTED", "ETAG_MISMATCH")
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"settings": map[string]string{"Bad Key": "v"}}), 400, "INVALID_ARGUMENT", "INVALID_SETTING")
	s = f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"settings": map[string]string{key: ""}}))
	if _, ok := s.body["settings"].(map[string]any)[key]; ok {
		t.Fatal(s.raw)
	}

	// reasons: upsert, never delete
	rk := "zz_test_" + strings.ReplaceAll(f.project, "-", "_")
	if len(rk) > 48 {
		rk = rk[:48]
	}
	rs := f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=reasons", map[string]any{"reasons": []map[string]any{{"reason": rk, "kind": "REJECT", "owner_side": "CUSTOMER", "name_en": "Test"}}}))
	var got map[string]any
	for _, r := range rs.body["reasons"].([]any) {
		if r.(map[string]any)["reason"] == rk {
			got = r.(map[string]any)
		}
	}
	if got == nil || got["kind"] != "REJECT" || got["owner_side"] != "CUSTOMER" || got["active"] != true {
		t.Fatal(rs.raw)
	}
	f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=reasons", map[string]any{"reasons": []map[string]any{{"reason": rk, "kind": "REJECT", "active": false}}}))
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=reasons", map[string]any{"reasons": []map[string]any{{"reason": rk, "kind": "KIND_UNSPECIFIED"}}}), 400, "INVALID_ARGUMENT", "INVALID_REASON")
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=reasons", map[string]any{"reasons": []map[string]any{{"reason": "Bad-Key", "kind": "REJECT"}}}), 400, "INVALID_ARGUMENT", "INVALID_REASON")

	// stages
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=stages", map[string]any{"stages": []map[string]any{{"stage": "verify", "seq": 1}}}), 400, "INVALID_ARGUMENT", "INVALID_STAGE")
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=stages", map[string]any{"stages": []map[string]any{{"stage": "Bad", "seq": 9}}}), 400, "INVALID_ARGUMENT", "INVALID_STAGE")

	// rules: validation, then add + restore (rows that did not change keep their ids)
	orig := cfg.body["rules"].([]any)
	for name, rule := range map[string]map[string]any{
		"bad side":    {"k_pattern": "x.%", "side": "NOBODY"},
		"bad kind":    {"k_pattern": "x.%", "side": "ANY", "reason_kind": "BOGUS"},
		"empty":       {"k_pattern": "", "side": "ANY"},
		"dangling \\": {"k_pattern": `x\`, "side": "ANY"},
		"long value":  {"k_pattern": "x", "side": "ANY", "to_value": strings.Repeat("v", 65)},
	} {
		f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=rules", map[string]any{"rules": append(append([]any{}, orig...), rule)}), 400, "INVALID_ARGUMENT", "INVALID_RULE")
		_ = name
	}
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=rules", map[string]any{"rules": []any{}}), 400, "INVALID_ARGUMENT", "RULES_EMPTY")
	extra := map[string]any{"k_pattern": `zz_test.%`, "to_value": "x", "side": "ANY", "reason_kind": "DISCARD"}
	with := f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=rules", map[string]any{"rules": append(append([]any{}, orig...), extra)}))
	if len(with.body["rules"].([]any)) != len(orig)+1 {
		t.Fatal(with.raw)
	}
	back := f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=rules", map[string]any{"rules": orig}))
	if len(back.body["rules"].([]any)) != len(orig) {
		t.Fatal(back.raw)
	}
}
