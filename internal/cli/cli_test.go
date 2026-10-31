package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

type failKR struct{}

func (failKR) Get(string, string) (string, error) { return "", errors.New("no keyring") }
func (failKR) Set(string, string, string) error   { return errors.New("no keyring") }

type reqRec struct {
	Method, Path, RawQuery, Auth string
	Body                         string
}

type fake struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []reqRec
	// handler may return false to fall back to defaults
	h func(w http.ResponseWriter, r *http.Request, body string) bool
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, reqRec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(b)})
		f.mu.Unlock()
		if f.h != nil && f.h(w, r, string(b)) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/me" {
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED","message":"bad token"}}`)
				return
			}
			io.WriteString(w, `{"account":"accounts/alice","kind":"HUMAN"}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) last() reqRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fake) find(method, suffix string) *reqRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reqs {
		if f.reqs[i].Method == method && strings.HasSuffix(f.reqs[i].Path, suffix) {
			return &f.reqs[i]
		}
	}
	return nil
}

type env struct {
	home string
	vars map[string]string
}

func newEnv(t *testing.T, f *fake) *env {
	return &env{home: t.TempDir(), vars: map[string]string{
		"OPENETL_BASE_URL": f.srv.URL, "OPENETL_PROJECT": "acme-dw", "OPENETL_TOKEN": "good-token",
	}}
}

func (e *env) run(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	a := &App{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Version: "test",
		Keyring: failKR{},
		Getenv: func(k string) string {
			if k == "HOME" {
				return e.home
			}
			return e.vars[k]
		},
	}
	code := a.Run(args)
	return code, out.String(), errb.String()
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not json: %q: %v", s, err)
	}
	return m
}

func TestVersionAndViews(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	code, out, _ := e.run("", "--version")
	if code != 0 || strings.TrimSpace(out) != "openetl test" {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = e.run("", "views")
	m := mustJSON(t, out)
	if code != 0 || len(m["views"].([]any)) != 7 {
		t.Fatalf("views: %d %s", code, out)
	}
	if len(f.reqs) != 0 {
		t.Fatal("views must be offline")
	}
}

func TestAuthLoginStatusLogout(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	delete(e.vars, "OPENETL_TOKEN")

	code, _, _ := e.run("", "auth", "status")
	if code != 1 {
		t.Fatalf("status without token: %d", code)
	}
	// bad token is rejected and not saved
	code, _, errs := e.run("wrong\n", "auth", "login", "--token-stdin")
	if code != 1 || mustJSON(t, errs)["error"].(map[string]any)["status"] != "UNAUTHENTICATED" {
		t.Fatalf("bad login: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config/openetl/credentials.enc")); err == nil {
		t.Fatal("saved a rejected token")
	}
	code, out, errs := e.run("good-token\n", "auth", "login", "--token-stdin", "--project", "acme-dw")
	if code != 0 || strings.Contains(out+errs, "good-token") {
		t.Fatalf("login: %d %s %s", code, out, errs)
	}
	st, err := os.Stat(filepath.Join(e.home, ".config/openetl/credentials.enc"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", st, err)
	}
	raw, _ := os.ReadFile(filepath.Join(e.home, ".config/openetl/credentials.enc"))
	if bytes.Contains(raw, []byte("good-token")) {
		t.Fatal("plaintext token on disk")
	}

	// later invocation: no env token, no project/base env; everything from the store
	e2 := &env{home: e.home, vars: map[string]string{}}
	code, out, _ = e2.run("", "auth", "status")
	m := mustJSON(t, out)
	if code != 0 || m["authenticated"] != true || m["token_source"] != "store" || m["account"] != "accounts/alice" || m["project"] != "acme-dw" {
		t.Fatalf("status: %d %s", code, out)
	}
	if strings.Contains(out, "good-token") {
		t.Fatal("token printed")
	}
	// saved token is actually used as bearer
	if code, _, _ = e2.run("", "workflow", "get", "WF-000123"); code != 0 {
		t.Fatal("get via stored creds failed")
	}
	if got := f.last(); got.Auth != "Bearer good-token" || got.Path != "/v1/projects/acme-dw/workflows/wf-000123" {
		t.Fatalf("%+v", got)
	}

	code, out, _ = e2.run("", "auth", "logout")
	if code != 0 || mustJSON(t, out)["logged_out"] != true {
		t.Fatal(out)
	}
	if code, _, _ = e2.run("", "auth", "status"); code != 1 {
		t.Fatalf("status after logout: %d", code)
	}
}

func TestAuthLoginWithRealKeyringMock(t *testing.T) {
	keyring.MockInit()
	f := newFake(t)
	e := newEnv(t, f)
	delete(e.vars, "OPENETL_TOKEN")
	var out, errb bytes.Buffer
	a := &App{Stdin: strings.NewReader("good-token"), Stdout: &out, Stderr: &errb, Getenv: func(k string) string {
		if k == "HOME" {
			return e.home
		}
		return e.vars[k]
	}}
	if code := a.Run([]string{"auth", "login"}); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config/openetl/.encryption_key")); err == nil {
		t.Fatal("key file written although keyring available")
	}
}

func TestWorkflowCreateAndIDForms(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, body string) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/workflows") {
			io.WriteString(w, `{"name":"projects/acme-dw/workflows/wf-000001","display_name":"daily_loader"}`)
			return true
		}
		return false
	}
	code, out, errs := e.run("", "workflow", "create", "--name", "daily_loader", "--label", "domain=dwh", "--assignee", "dev_owner=alice", "--request-id", "7d1f3c52-2f0e-4c53-9d1a-6a9c0f3a1b11")
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if strings.Contains(out, " ") {
		t.Fatalf("not compact: %q", out)
	}
	r := f.last()
	if r.Path != "/v1/projects/acme-dw/workflows" || !strings.Contains(r.RawQuery, "request_id=7d1f3c52") {
		t.Fatalf("%+v", r)
	}
	b := mustJSON(t, r.Body)
	if b["display_name"] != "daily_loader" || b["labels"].(map[string]any)["domain"] != "dwh" ||
		b["assignees"].(map[string]any)["dev_owner"] != "accounts/alice" {
		t.Fatalf("body %s", r.Body)
	}
	// id forms
	for _, in := range []string{"WF-000123", "wf-000123", "projects/acme-dw/workflows/wf-000123"} {
		if code, _, errs := e.run("", "workflow", "get", in); code != 0 || f.last().Path != "/v1/projects/acme-dw/workflows/wf-000123" {
			t.Fatalf("%s: %d %s %+v", in, code, errs, f.last())
		}
	}
	// missing --name is a usage error
	code, _, errs = e.run("", "workflow", "create")
	if code != 2 || mustJSON(t, errs)["error"].(map[string]any)["status"] != "USAGE" {
		t.Fatalf("%d %s", code, errs)
	}
	// update builds a mask
	if code, _, errs = e.run("", "workflow", "update", "WF-000123", "--label", "priority=b1", "--etag", "ev-1"); code != 0 {
		t.Fatal(errs)
	}
	r = f.last()
	if r.Method != "PATCH" || r.RawQuery != "update_mask=labels" || mustJSON(t, r.Body)["etag"] != "ev-1" {
		t.Fatalf("%+v", r)
	}
}

func TestWorkflowListPagination(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if !strings.HasSuffix(r.URL.Path, "/workflows") || r.Method != "GET" {
			return false
		}
		switch r.URL.Query().Get("page_token") {
		case "":
			io.WriteString(w, `{"workflows":[{"display_name":"a"}],"next_page_token":"p2"}`)
		case "p2":
			io.WriteString(w, `{"workflows":[{"display_name":"b"}],"next_page_token":"p3"}`)
		default:
			io.WriteString(w, `{"workflows":[{"display_name":"c"}]}`)
		}
		return true
	}
	// single page
	code, out, _ := e.run("", "workflow", "list", "--page-size", "1", "--filter", "health = BLOCKED")
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, `"p2"`) {
		t.Fatalf("%d %q", code, out)
	}
	if q := f.last().RawQuery; !strings.Contains(q, "page_size=1") || !strings.Contains(q, "filter=health+%3D+BLOCKED") {
		t.Fatal(q)
	}
	// all pages as NDJSON
	code, out, _ = e.run("", "workflow", "list", "--page-count", "10")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("%d %q", code, out)
	}
	// capped at N pages
	_, out, _ = e.run("", "workflow", "list", "--page-count", "2")
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 2 {
		t.Fatalf("%d lines", n)
	}
	// --output writes a file
	file := filepath.Join(t.TempDir(), "wf.ndjson")
	code, out, _ = e.run("", "workflow", "list", "--page-count", "5", "--output", file)
	data, _ := os.ReadFile(file)
	if code != 0 || strings.Count(string(data), "\n") != 3 || mustJSON(t, out)["output"] != file {
		t.Fatalf("%d %q %q", code, out, data)
	}
}

func TestStageAdvanceDryRunEtag(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	code, _, errs := e.run("", "stage", "advance", "WF-000123", "--stage", "development", "--note", "Code merged", "--etag", "ev-8101", "--dry-run")
	if code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	if r.Method != "POST" || r.Path != "/v1/projects/acme-dw/workflows/wf-000123:advanceStage" {
		t.Fatalf("%+v", r)
	}
	b := mustJSON(t, r.Body)
	if b["stage"] != "development" || b["etag"] != "ev-8101" || b["validate_only"] != true || b["note"] != "Code merged" {
		t.Fatalf("%s", r.Body)
	}
	if id, _ := b["request_id"].(string); len(id) != 36 {
		t.Fatalf("request_id not generated: %s", r.Body)
	}
	// without --dry-run no validate_only
	e.run("", "stage", "advance", "wf-1", "--stage", "verify")
	if _, ok := mustJSON(t, f.last().Body)["validate_only"]; ok {
		t.Fatal("validate_only leaked")
	}
	// regress needs a reason; usage error otherwise
	if code, _, _ = e.run("", "stage", "regress", "WF-000123", "--stage", "accept"); code != 2 {
		t.Fatalf("regress without reason: %d", code)
	}
	if code, _, errs = e.run("", "stage", "regress", "WF-000123", "--stage", "accept", "--stage", "verify", "--reason", "rej_row_count"); code != 0 {
		t.Fatal(errs)
	}
	b = mustJSON(t, f.last().Body)
	if got := b["stages"].([]any); len(got) != 2 || b["reason"] != "rej_row_count" {
		t.Fatalf("%v", b)
	}
	// scope
	if code, _, errs = e.run("", "scope", "set", "WF-000200", "--to", "out", "--reason", "merged", "--into", "WF-000123"); code != 0 {
		t.Fatal(errs)
	}
	b = mustJSON(t, f.last().Body)
	if b["scope"] != "OUT_OF_SCOPE" || b["merged_into"] != "projects/acme-dw/workflows/wf-000123" {
		t.Fatalf("%v", b)
	}
}

func TestErrorMapping(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	status := 409
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.URL.Path == "/v1/me" {
			return false
		}
		w.WriteHeader(status)
		switch status {
		case 409:
			io.WriteString(w, `{"error":{"code":409,"status":"ABORTED","message":"changed","details":[{"@type":"ErrorInfo","reason":"ETAG_MISMATCH","domain":"openetl","metadata":{"current_etag":"ev-8102"}}]}}`)
		case 400:
			io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad sql"}}`)
		case 502:
			io.WriteString(w, `<html>bad server</html>`)
		}
		return true
	}
	code, out, errs := e.run("", "stage", "advance", "WF-1", "--stage", "development")
	m := mustJSON(t, errs)["error"].(map[string]any)
	if code != 1 || out != "" || m["status"] != "ABORTED" || m["code"].(float64) != 409 ||
		m["details"].([]any)[0].(map[string]any)["reason"] != "ETAG_MISMATCH" {
		t.Fatalf("%d %q %s", code, out, errs)
	}
	status = 400
	if code, _, errs = e.run("", "query", "SELECT 1"); code != 2 || mustJSON(t, errs)["error"].(map[string]any)["status"] != "INVALID_ARGUMENT" {
		t.Fatalf("%d %s", code, errs)
	}
	status = 502
	code, _, errs = e.run("", "workflow", "get", "wf-1")
	if code != 1 || mustJSON(t, errs)["error"].(map[string]any)["status"] != "INTERNAL" {
		t.Fatalf("%d %s", code, errs)
	}
	// usage errors
	for _, args := range [][]string{{"nope"}, {"workflow", "bogus"}, {"workflow", "get"}, {"stage", "advance", "wf-1", "--bogus"}} {
		if code, _, errs = e.run("", args...); code != 2 || !strings.Contains(errs, `"status":"USAGE"`) {
			t.Fatalf("%v: %d %s", args, code, errs)
		}
	}
	// missing project
	delete(e.vars, "OPENETL_PROJECT")
	if code, _, _ = e.run("", "workflow", "list"); code != 2 {
		t.Fatalf("no project: %d", code)
	}
	// missing token
	e.vars["OPENETL_PROJECT"] = "acme-dw"
	delete(e.vars, "OPENETL_TOKEN")
	code, _, errs = e.run("", "workflow", "list")
	if code != 1 || !strings.Contains(errs, "UNAUTHENTICATED") {
		t.Fatalf("%d %s", code, errs)
	}
}

// query has no --as-of (the server never supported it) and documents the real page-size maximum.
func TestQueryFlags(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	if code, _, _ := e.run("", "query", "SELECT 1", "--as-of", "2026-01-01T00:00:00Z"); code != 2 {
		t.Errorf("--as-of must be an unknown flag (exit 2), got %d", code)
	}
	_, out, errs := e.run("", "query", "--help")
	h := out + errs
	if !strings.Contains(h, "max 1000") || strings.Contains(h, "10000") || strings.Contains(h, "as-of") {
		t.Errorf("query --help:\n%s", h)
	}
}

func TestQueryPaginates(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	n := 0
	f.h = func(w http.ResponseWriter, r *http.Request, body string) bool {
		if !strings.HasSuffix(r.URL.Path, ":query") {
			return false
		}
		n++
		if n == 1 {
			io.WriteString(w, `{"columns":[{"name":"x"}],"rows":[[1]],"next_page_token":"t2"}`)
		} else {
			if !strings.Contains(body, `"page_token":"t2"`) {
				t.Errorf("second page body %s", body)
			}
			io.WriteString(w, `{"rows":[[2]]}`)
		}
		return true
	}
	code, out, errs := e.run("", "query", "SELECT x FROM workflows", "--page-count", "5")
	if code != 0 || len(strings.Split(strings.TrimSpace(out), "\n")) != 2 {
		t.Fatalf("%d %q %s", code, out, errs)
	}
	if r := f.find("POST", ":query"); r == nil || r.Path != "/v1/projects/acme-dw:query" || !strings.Contains(r.Body, `"sql":"SELECT x FROM workflows"`) {
		t.Fatalf("%+v", r)
	}
}

func TestCommentAddWithAttach(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	var uploaded []byte
	var uploadAuth, uploadHdr string
	f.h = func(w http.ResponseWriter, r *http.Request, body string) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/attachments:initUpload"):
			io.WriteString(w, `{"storage_key":"sk/1","upload_url":"`+f.srv.URL+`/blob/1","upload_headers":{"X-Test":"yes"}}`)
		case r.Method == "PUT" && r.URL.Path == "/blob/1":
			uploaded = []byte(body)
			uploadAuth, uploadHdr = r.Header.Get("Authorization"), r.Header.Get("X-Test")
		case strings.HasSuffix(r.URL.Path, "/attachments"):
			io.WriteString(w, `{"name":"projects/acme-dw/workflows/wf-000123/attachments/at-000009"}`)
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "POST":
			io.WriteString(w, `{"name":"projects/acme-dw/workflows/wf-000123/comments/cm-000031"}`)
		default:
			return false
		}
		return true
	}
	file := filepath.Join(t.TempDir(), "screenshot.png")
	os.WriteFile(file, []byte("PNGDATA"), 0o644)
	code, out, errs := e.run("", "comment", "add", "WF-000123", "--kind", "issue", "--body", "目标库权限未开通", "--attach", file)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	if !strings.Contains(out, "cm-000031") || string(uploaded) != "PNGDATA" || uploadHdr != "yes" {
		t.Fatalf("out=%s uploaded=%q hdr=%q", out, uploaded, uploadHdr)
	}
	_ = uploadAuth
	init := mustJSON(t, f.find("POST", "/attachments:initUpload").Body)
	if init["filename"] != "screenshot.png" || init["size"].(float64) != 7 || len(init["sha256"].(string)) != 64 {
		t.Fatalf("%v", init)
	}
	reg := mustJSON(t, f.find("POST", "/wf-000123/attachments").Body)
	if reg["storage_key"] != "sk/1" || reg["sha256"] != init["sha256"] {
		t.Fatalf("%v", reg)
	}
	cm := mustJSON(t, f.last().Body)
	if cm["kind"] != "ISSUE" || cm["attachments"].([]any)[0] != "projects/acme-dw/workflows/wf-000123/attachments/at-000009" {
		t.Fatalf("%v", cm)
	}
	// same request id => same derived attachment request id (idempotent retry)
	rq1 := f.find("POST", "/wf-000123/attachments").RawQuery
	e.run("", "comment", "add", "WF-000123", "--body", "x", "--attach", file, "--request-id", "7d1f3c52-2f0e-4c53-9d1a-6a9c0f3a1b11")
	e.run("", "comment", "add", "WF-000123", "--body", "x", "--attach", file, "--request-id", "7d1f3c52-2f0e-4c53-9d1a-6a9c0f3a1b11")
	var rids []string
	for _, r := range f.reqs {
		if strings.HasSuffix(r.Path, "/wf-000123/attachments") && strings.Contains(r.RawQuery, "request_id=") && r.RawQuery != rq1 {
			rids = append(rids, r.RawQuery)
		}
	}
	if len(rids) != 2 || rids[0] != rids[1] {
		t.Fatalf("attachment request ids not stable: %v", rids)
	}
	// bad kind / missing file are usage errors
	if code, _, _ = e.run("", "comment", "add", "wf-1", "--body", "x", "--kind", "oops"); code != 2 {
		t.Fatal("bad kind")
	}
	if code, _, _ = e.run("", "comment", "add", "wf-1", "--body", "x", "--attach", "/nonexistent"); code != 2 {
		t.Fatal("missing file")
	}
}

func TestCommentAttachNotImplemented(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if strings.HasSuffix(r.URL.Path, ":initUpload") {
			w.WriteHeader(501)
			io.WriteString(w, `{"error":{"code":501,"status":"UNIMPLEMENTED","message":"attachments are not available in M1"}}`)
			return true
		}
		return false
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	code, out, errs := e.run("", "comment", "add", "wf-1", "--body", "evidence", "--attach", file)
	m := mustJSON(t, errs)["error"].(map[string]any)
	if code != 1 || out != "" || m["status"] != "UNIMPLEMENTED" || m["code"].(float64) != 501 {
		t.Fatalf("%d %q %s", code, out, errs)
	}
	for _, r := range f.reqs {
		if strings.HasSuffix(r.Path, "/comments") {
			t.Fatal("comment created although attachment failed")
		}
	}
}

func TestCommentDryRunSkipsUpload(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	code, _, errs := e.run("", "comment", "add", "wf-1", "--body", "b", "--attach", file, "--dry-run")
	if code != 0 || !strings.Contains(errs, "warning") {
		t.Fatalf("%d %s", code, errs)
	}
	if len(f.reqs) != 1 || !strings.Contains(f.last().RawQuery, "validate_only=true") {
		t.Fatalf("%+v", f.reqs)
	}
}

func TestTaskAndCommentActions(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	if code, _, errs := e.run("", "task", "assign", "TK-004567", "--workflow", "WF-000123"); code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	if r.Path != "/v1/projects/acme-dw/tasks/tk-004567:assign" || mustJSON(t, r.Body)["membership"] != nil ||
		mustJSON(t, r.Body)["workflow"] != "projects/acme-dw/workflows/wf-000123" {
		t.Fatalf("%+v", r)
	}
	e.run("", "task", "list", "--workflow", "WF-000123")
	if q := f.last().RawQuery; !strings.HasPrefix(q, "filter=workflow+%3D+") {
		t.Fatal(q)
	}
	e.run("", "task", "create", "--name", "n", "--workflow", "WF-000123")
	if r = f.last(); r.Method != "POST" || mustJSON(t, r.Body)["workflow"] != "projects/acme-dw/workflows/wf-000123" {
		t.Fatalf("%+v", r)
	}
	if code, _, _ := e.run("", "task", "create", "--name", "n"); code != 2 {
		t.Fatalf("task create without --workflow must be a usage error, exit %d", code)
	}
	e.run("", "stage", "regress", "WF-000123", "--stage", "accept", "--stage", "verify", "--reason", "rej_row_count", "--evidence", "at-000009")
	if r = f.last(); !strings.HasSuffix(r.Path, ":regressStage") || fmt.Sprint(mustJSON(t, r.Body)["evidence_attachments"]) != "[at-000009]" {
		t.Fatalf("%+v", r)
	}
	e.run("", "scope", "set", "WF-000123", "--to", "in", "--evidence", "at-000009")
	if r = f.last(); !strings.HasSuffix(r.Path, ":setScope") || fmt.Sprint(mustJSON(t, r.Body)["evidence_attachments"]) != "[at-000009]" {
		t.Fatalf("%+v", r)
	}
	e.run("", "comment", "resolve", "WF-000123", "CM-000031", "--note", "fixed")
	if r = f.last(); r.Path != "/v1/projects/acme-dw/workflows/wf-000123/comments/cm-000031:resolve" {
		t.Fatalf("%+v", r)
	}
	e.run("", "comment", "delete", "WF-000123", "cm-000031")
	if r = f.last(); r.Method != "DELETE" {
		t.Fatalf("%+v", r)
	}
	e.run("", "workflow", "restore", "WF-000123")
	if r = f.last(); !strings.HasSuffix(r.Path, "wf-000123:undelete") {
		t.Fatalf("%+v", r)
	}
	e.run("", "workflow", "delete", "WF-000123", "--etag", "ev-9", "--dry-run")
	if r = f.last(); r.Method != "DELETE" || !strings.Contains(r.RawQuery, "etag=ev-9") || !strings.Contains(r.RawQuery, "validate_only=true") {
		t.Fatalf("%+v", r)
	}
}
