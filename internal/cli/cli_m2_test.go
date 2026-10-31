package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func (e *env) runTTY(stdin string, tty bool, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	a := &App{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Version: "test",
		Keyring: failKR{}, IsTerminal: func() bool { return tty }, PollInterval: time.Millisecond,
		Getenv: func(k string) string {
			if k == "HOME" {
				return e.home
			}
			return e.vars[k]
		},
	}
	return a.Run(args), out.String(), errb.String()
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

func TestTaskDeleteRestore(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	if code, _, errs := e.run("", "task", "delete", "TK-000001", "--etag", "ev-3", "--dry-run"); code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	if r.Method != "DELETE" || r.Path != "/v1/projects/acme-dw/tasks/tk-000001" ||
		!strings.Contains(r.RawQuery, "etag=ev-3") || !strings.Contains(r.RawQuery, "validate_only=true") {
		t.Fatalf("%+v", r)
	}
	if code, _, _ := e.run("", "task", "delete", "TK-000001", "--note", "x"); code != 2 {
		t.Fatal("--note on delete must be a usage error")
	}
	if code, _, errs := e.run("", "task", "restore", "tk-000001", "--note", "oops"); code != 0 {
		t.Fatal(errs)
	}
	r = f.last()
	if r.Method != "POST" || !strings.HasSuffix(r.Path, "/tasks/tk-000001:undelete") || mustJSON(t, r.Body)["note"] != "oops" {
		t.Fatalf("%+v", r)
	}
}

func TestEvents(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if strings.HasSuffix(r.URL.Path, "/events") {
			if r.URL.Query().Get("page_token") == "" {
				reply(w, `{"events":[{"name":"e1"}],"next_page_token":"t2"}`)
			} else {
				reply(w, `{"events":[{"name":"e2"}]}`)
			}
			return true
		}
		return false
	}
	code, out, errs := e.run("", "events", "list", "WF-000123", "--as-of", "2026-09-30T00:00:00+08:00", "--filter", "actor = \"accounts/a\"", "--page-count", "3")
	if code != 0 {
		t.Fatal(errs)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 {
		t.Fatalf("want 2 NDJSON pages: %q", out)
	}
	r := *f.find("GET", "/events")
	q, _ := url.ParseQuery(r.RawQuery)
	if r.Path != "/v1/projects/acme-dw/workflows/wf-000123/events" || q.Get("as_of") != "2026-09-29T16:00:00Z" || q.Get("filter") == "" {
		t.Fatalf("%+v", r)
	}
	if code, _, _ := e.run("", "events", "list", "WF-000123", "--as-of", "yesterday"); code != 2 {
		t.Fatal("bad --as-of must exit 2")
	}
	if code, _, errs := e.run("", "events", "get", "WF-000123", "projects/acme-dw/workflows/wf-000123/events/EV-8102"); code != 0 {
		t.Fatal(errs)
	}
	if f.last().Path != "/v1/projects/acme-dw/workflows/wf-000123/events/ev-8102" {
		t.Fatalf("%+v", f.last())
	}
}

func TestAttachmentGet(t *testing.T) {
	// foreign storage host: must not see the API token
	var storageAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = r.Header.Get("Authorization")
		io.WriteString(w, "hello evidence")
	}))
	defer storage.Close()
	f := newFake(t)
	e := newEnv(t, f)
	dl := storage.URL + "/blob?sig=1"
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if strings.HasSuffix(r.URL.Path, "/attachments/at-000009:download") {
			reply(w, `{"download_url":"`+dl+`"}`)
			return true
		}
		return false
	}
	dst := filepath.Join(t.TempDir(), "ev.bin")
	code, out, errs := e.run("", "attachment", "get", "WF-000123", "AT-000009", "-o", dst)
	if code != 0 {
		t.Fatal(errs)
	}
	b, _ := os.ReadFile(dst)
	m := mustJSON(t, out)
	if string(b) != "hello evidence" || m["bytes"].(float64) != 14 || len(m["sha256"].(string)) != 64 || storageAuth != "" {
		t.Fatalf("out=%s file=%q auth=%q", out, b, storageAuth)
	}
	// relative URL served by the server itself gets the token
	var gwAuth string
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, ":download"):
			reply(w, `{"download_url":"/v1/blob/1"}`)
		case r.URL.Path == "/v1/blob/1":
			gwAuth = r.Header.Get("Authorization")
			io.WriteString(w, "x")
		default:
			return false
		}
		return true
	}
	if code, _, errs := e.run("", "attachment", "get", "WF-000123", "at-000009", "--output", dst); code != 0 || gwAuth != "Bearer good-token" {
		t.Fatal(errs, gwAuth)
	}
	if code, _, _ := e.run("", "attachment", "get", "WF-000123", "at-000009"); code != 2 {
		t.Fatal("--output required")
	}
	// 501 from server surfaces as UNIMPLEMENTED exit 1
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		w.WriteHeader(501)
		reply(w, `{"error":{"code":501,"status":"UNIMPLEMENTED","message":"nope"}}`)
		return true
	}
	code, _, errs = e.run("", "attachment", "get", "WF-000123", "at-000009", "-o", dst)
	if code != 1 || !strings.Contains(errs, "UNIMPLEMENTED") {
		t.Fatal(code, errs)
	}
}

func TestMembers(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	if code, _, errs := e.run("", "member", "list", "--page-size", "5"); code != 0 {
		t.Fatal(errs)
	}
	if r := f.last(); r.Path != "/v1/projects/acme-dw/members" || r.RawQuery != "page_size=5" {
		t.Fatalf("%+v", r)
	}
	if code, _, errs := e.run("", "member", "add", "accounts/Bob", "--role", "editor", "--side", "customer"); code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	m := mustJSON(t, r.Body)
	if r.Method != "PUT" || r.Path != "/v1/projects/acme-dw/members/bob" || m["role"] != "EDITOR" || m["side"] != "CUSTOMER" {
		t.Fatalf("%+v", r)
	}
	n := len(f.reqs)
	code, out, _ := e.run("", "member", "add", "bob", "--role", "admin", "--side", "delivery", "--dry-run")
	if code != 0 || len(f.reqs) != n || mustJSON(t, out)["dry_run"] != true {
		t.Fatalf("dry-run must not send: %s", out)
	}
	for _, args := range [][]string{
		{"member", "add", "bob", "--side", "delivery"},
		{"member", "add", "bob", "--role", "root", "--side", "delivery"},
		{"member", "add", "bob", "--role", "admin", "--side", "system"},
		{"member", "add", "bob", "--role", "admin", "--side", "delivery", "--etag", "x"},
	} {
		if code, _, _ := e.run("", args...); code != 2 {
			t.Errorf("%v: want exit 2", args)
		}
	}
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return true
		}
		return false
	}
	code, out, errs := e.run("", "member", "remove", "bob")
	if code != 0 || mustJSON(t, out)["removed"] != true {
		t.Fatal(code, out, errs)
	}
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		w.WriteHeader(403)
		reply(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"admin only"}}`)
		return true
	}
	if code, _, errs := e.run("", "member", "remove", "bob"); code != 1 || !strings.Contains(errs, "PERMISSION_DENIED") {
		t.Fatal(code, errs)
	}
}

func TestTokens(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, body string) bool {
		switch {
		case r.URL.Path == "/v1/me":
			reply(w, `{"account":"accounts/alice"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/tokens"):
			reply(w, `{"name":"accounts/alice/tokens/9","secret":"oe_SECRET"}`)
		case strings.HasSuffix(r.URL.Path, ":revoke"):
			reply(w, `{"name":"accounts/alice/tokens/9","revoke_time":"2026-10-31T00:00:00Z"}`)
		case r.Method == "GET":
			reply(w, `{"tokens":[{"name":"accounts/alice/tokens/9"}]}`)
		default:
			return false
		}
		return true
	}
	code, out, errs := e.run("", "token", "create", "--ttl", "24h")
	if code != 0 || mustJSON(t, out)["secret"] != "oe_SECRET" || !strings.Contains(errs, "only once") {
		t.Fatal(code, out, errs)
	}
	r := *f.find("POST", "/tokens")
	if r.Path != "/v1/accounts/alice/tokens" {
		t.Fatalf("%+v", r)
	}
	exp, err := time.Parse(time.RFC3339, mustJSON(t, r.Body)["expire_time"].(string))
	if err != nil || time.Until(exp) < 23*time.Hour || time.Until(exp) > 25*time.Hour {
		t.Fatalf("expire_time: %v %v", exp, err)
	}
	// secret file is private and nothing lands in the credential store
	dst := filepath.Join(t.TempDir(), "tok.json")
	if code, out, errs := e.run("", "token", "create", "--account", "bob", "-o", dst); code != 0 || strings.Contains(out, "oe_SECRET") {
		t.Fatal(code, out, errs)
	}
	if st, _ := os.Stat(dst); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode: %v", st)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".config", "openetl")); err == nil {
		t.Fatal("token create must not write credentials")
	}
	if code, _, _ := e.run("", "token", "create", "--ttl", "1h", "--expire-time", "2027-01-01T00:00:00Z"); code != 2 {
		t.Fatal("ttl+expire-time must conflict")
	}
	if code, out, _ := e.run("", "token", "list"); code != 0 || !strings.Contains(out, "tokens/9") {
		t.Fatal(out)
	}
	if code, _, errs := e.run("", "token", "revoke", "accounts/carol/tokens/9"); code != 0 {
		t.Fatal(errs)
	}
	if r := f.last(); r.Path != "/v1/accounts/carol/tokens/9:revoke" {
		t.Fatalf("%+v", r)
	}
	if code, out, _ := e.run("", "token", "revoke", "9", "--dry-run"); code != 0 || mustJSON(t, out)["dry_run"] != true {
		t.Fatal(out)
	}
}

func TestConfig(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if strings.HasSuffix(r.URL.Path, "/config") {
			reply(w, `{"name":"projects/acme-dw/config","etag":"cfg-4","stages":[{"stage":"development","seq":1}],"settings":{"a":"1"}}`)
			return true
		}
		return false
	}
	code, out, errs := e.run("", "config", "get", "--section", "stages")
	if code != 0 || out != `[{"stage":"development","seq":1}]`+"\n" {
		t.Fatal(code, out, errs)
	}
	if code, _, _ := e.run("", "config", "get", "--section", "nope"); code != 2 {
		t.Fatal("bad section")
	}
	// whole-section replace, JSON from stdin, wrapped in the section key
	code, _, errs = e.run(`{"rules":[{"k_pattern":"stage.%","side":"ANY"}]}`, "config", "set", "--rules", "-", "--etag", "cfg-4")
	if code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	m := mustJSON(t, r.Body)
	if r.Method != "PATCH" || r.RawQuery != "update_mask=rules" || m["etag"] != "cfg-4" || len(m["rules"].([]any)) != 1 {
		t.Fatalf("%+v", r)
	}
	// single setting merges into the current map and carries the etag
	if code, _, errs = e.run("", "config", "set", "--setting", "b=2"); code != 0 {
		t.Fatal(errs)
	}
	r = f.last()
	m = mustJSON(t, r.Body)
	st := m["settings"].(map[string]any)
	if r.RawQuery != "update_mask=settings" || st["a"] != "1" || st["b"] != "2" || m["etag"] != "cfg-4" {
		t.Fatalf("%+v", r)
	}
	// stages from a file, dry-run sends nothing
	p := filepath.Join(t.TempDir(), "stages.json")
	os.WriteFile(p, []byte(`[{"stage":"a","seq":1}]`), 0o600)
	n := len(f.reqs)
	code, out, _ = e.run("", "config", "set", "--stages", p, "--dry-run")
	if code != 0 || len(f.reqs) != n || mustJSON(t, out)["dry_run"] != true {
		t.Fatal(out)
	}
	if code, _, _ := e.run("", "config", "set"); code != 2 {
		t.Fatal("nothing to set")
	}
	if code, _, _ := e.run("not json", "config", "set", "--stages", "-"); code != 2 {
		t.Fatal("invalid JSON")
	}
}

// ------------------------------------------------------------------ import

const importMap = `
source_system: legacy
reference: [id]
fields: {name: pname, domain: dom}
`

func writeImportFiles(t *testing.T) (src, mp string) {
	t.Helper()
	d := t.TempDir()
	src = filepath.Join(d, "rows.csv")
	mp = filepath.Join(d, "mapping.yaml")
	os.WriteFile(src, []byte("id,pname,dom\n2,B,HR\n1,A,Sales\n"), 0o600)
	os.WriteFile(mp, []byte(importMap), 0o600)
	return
}

func TestImportStage(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	src, mp := writeImportFiles(t)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/importJobs") {
			reply(w, `{"name":"projects/acme-dw/importJobs/17","state":"STAGED","summary":{"NEW":2}}`)
			return true
		}
		return false
	}
	code, out, errs := e.run("", "import", "stage", "--from", src, "--map", mp, "--request-id", "7d6f7b0e-2f3a-4c1c-9d0e-1a2b3c4d5e6f")
	if code != 0 || mustJSON(t, out)["name"] != "projects/acme-dw/importJobs/17" {
		t.Fatal(code, out, errs)
	}
	r := f.last()
	if r.Path != "/v1/projects/acme-dw/importJobs" || !strings.Contains(r.RawQuery, "request_id=7d6f7b0e") || strings.Contains(r.RawQuery, "validate_only") {
		t.Fatalf("%+v", r)
	}
	b := mustJSON(t, r.Body)
	rows := b["rows"].([]any)
	row0 := rows[0].(map[string]any)
	if b["source"] != "legacy" || len(rows) != 2 || row0["source"].(map[string]any)["reference"] != "1" ||
		len(row0["source"].(map[string]any)["row_hash"].(string)) != 64 ||
		row0["data"].(map[string]any)["display_name"] != "A" || row0["data"].(map[string]any)["labels"].(map[string]any)["domain"] != "sales" {
		t.Fatalf("body: %s", r.Body)
	}
	// --source-system overrides; --dry-run sends nothing
	n := len(f.reqs)
	code, out, _ = e.run("", "import", "stage", "--from", src, "--map", mp, "--source-system", "x", "--dry-run")
	m := mustJSON(t, out)
	if code != 0 || len(f.reqs) != n || m["dry_run"] != true || m["source_system"] != "x" || m["rows"].(float64) != 2 {
		t.Fatal(out)
	}
	for _, args := range [][]string{
		{"import", "stage", "--map", mp},
		{"import", "stage", "--from", src},
		{"import", "stage", "--from", src, "--map", "/nonexistent.yaml"},
		{"import", "stage", "--from", "/nonexistent.csv", "--map", mp},
		{"import", "stage", "--from", src, "--map", mp, "--note", "n"},
	} {
		if code, _, _ := e.run("", args...); code != 2 {
			t.Errorf("%v: want exit 2, got %d", args, code)
		}
	}
	// a mapping failure (duplicate reference) is a runtime error naming the row
	os.WriteFile(src, []byte("id,pname,dom\n1,A,x\n1,B,y\n"), 0o600)
	if code, _, errs := e.run("", "import", "stage", "--from", src, "--map", mp); code != 1 || !strings.Contains(errs, "duplicate reference") {
		t.Fatal(code, errs)
	}
}

func TestImportStageMySQLPasswordNeverLeaves(t *testing.T) {
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"DROP TABLE IF EXISTS cli_imp_src",
		"CREATE TABLE cli_imp_src (id INT PRIMARY KEY, pname VARCHAR(20), dom VARCHAR(20))",
		"INSERT INTO cli_imp_src VALUES (1,'A','Sales'),(2,'B','HR')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS cli_imp_src") })

	f := newFake(t)
	e := newEnv(t, f)
	e.vars["OPENETL_SOURCE_PASSWORD"] = cfg.Passwd
	mp := filepath.Join(t.TempDir(), "m.yaml")
	os.WriteFile(mp, []byte("reference: [id]\nsource: {table: cli_imp_src, order_by: id}\nfields: {name: pname, domain: dom}\n"), 0o600)
	from := "mysql://" + cfg.User + "@" + cfg.Addr + "/" + cfg.DBName
	code, out, errs := e.run("", "import", "stage", "--from", from, "--map", mp)
	if code != 0 {
		t.Fatal(errs)
	}
	r := f.last()
	if strings.Contains(r.Body+r.RawQuery, ":"+cfg.Passwd+"@") || strings.Contains(out+errs, ":"+cfg.Passwd+"@") || r.Path != "/v1/projects/acme-dw/importJobs" {
		t.Fatalf("password leaked: %+v", r)
	}
	b := mustJSON(t, r.Body)
	if !strings.HasPrefix(b["source"].(string), "mysql://"+cfg.User+"@") || len(b["rows"].([]any)) != 2 {
		t.Fatalf("body: %s", r.Body)
	}
	// wrong password: exit 1 and the password is not echoed
	e.vars["OPENETL_SOURCE_PASSWORD"] = "bad-pw-QQQ"
	code, out, errs = e.run("", "import", "stage", "--from", from, "--map", mp)
	if code != 1 || strings.Contains(out+errs, "bad-pw-QQQ") {
		t.Fatal(code, out, errs)
	}
}

const diffCounts = `{"name":"projects/acme-dw/importJobs/17","state":"STAGED","source":"legacy","summary":{"NEW":12,"CHANGED":3,"UNCHANGED":50,"CONFLICT":1,"SUSPECT":0,"MISSING":2}}`

func diffHandler(w http.ResponseWriter, r *http.Request, _ string) bool {
	switch {
	case strings.HasSuffix(r.URL.Path, ":diff"):
		q := r.URL.Query()
		act := strings.TrimPrefix(q.Get("filter"), "action = ")
		n := map[string]int{"NEW": 12, "CHANGED": 3, "CONFLICT": 1, "MISSING": 2, "UNCHANGED": 50}[act]
		size := 10
		if q.Get("page_size") != "" {
			size = map[string]int{"2": 2, "10": 10}[q.Get("page_size")]
		}
		var rows []string
		for i := 0; i < n && i < size; i++ {
			rows = append(rows, `{"row_no":`+strconv.Itoa(i+1)+`,"action":"`+act+`"}`)
		}
		next := ""
		if n > size {
			next = `,"next_page_token":"more"`
		}
		reply(w, `{"rows":[`+strings.Join(rows, ",")+`]`+next+`}`)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/importJobs/17"):
		reply(w, diffCounts)
	default:
		return false
	}
	return true
}

func TestImportShowAndDiff(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = diffHandler
	code, out, errs := e.run("", "import", "show", "17")
	m := mustJSON(t, out)
	if code != 0 || m["total"].(float64) != 68 || m["needs_review"] != true || m["state"] != "STAGED" {
		t.Fatal(code, out, errs)
	}
	code, out, errs = e.run("", "import", "diff", "projects/acme-dw/importJobs/17", "--limit", "2")
	if code != 0 {
		t.Fatal(errs)
	}
	m = mustJSON(t, out)
	cls := m["classes"].(map[string]any)
	if m["needs_review"] != true || m["counts"].(map[string]any)["NEW"].(float64) != 12 {
		t.Fatal(out)
	}
	if _, has := cls["UNCHANGED"]; has {
		t.Fatal("UNCHANGED rows are not listed by default")
	}
	nw := cls["NEW"].(map[string]any)
	if nw["count"].(float64) != 12 || len(nw["rows"].([]any)) != 2 || nw["truncated"] != true {
		t.Fatalf("NEW: %v", nw)
	}
	cf := cls["CONFLICT"].(map[string]any)
	if len(cf["rows"].([]any)) != 1 || cf["truncated"] != false {
		t.Fatalf("CONFLICT: %v", cf)
	}
	if sp := cls["SUSPECT"].(map[string]any); len(sp["rows"].([]any)) != 0 {
		t.Fatal("SUSPECT has zero rows")
	}
	// zero-count classes cost no request
	for _, r := range f.reqs {
		if strings.Contains(r.RawQuery, "SUSPECT") {
			t.Fatal("SUSPECT has count 0, should not be fetched")
		}
	}
	if order := m["order"].([]any); order[0] != "CONFLICT" {
		t.Fatalf("order: %v", order)
	}
	// --action narrows; UNCHANGED can be requested
	code, out, errs = e.run("", "import", "diff", "17", "--action", "unchanged")
	if code != 0 {
		t.Fatal(errs)
	}
	cls = mustJSON(t, out)["classes"].(map[string]any)
	if code != 0 || len(cls) != 1 || len(cls["UNCHANGED"].(map[string]any)["rows"].([]any)) != 10 {
		t.Fatal(out)
	}
	if code, _, _ := e.run("", "import", "diff", "17", "--action", "bogus"); code != 2 {
		t.Fatal("bad action")
	}
	// --raw passes server pages through
	code, out, _ = e.run("", "import", "diff", "17", "--raw", "--filter", "action = NEW", "--page-size", "2", "--page-count", "1")
	if code != 0 || !strings.Contains(out, `"rows"`) {
		t.Fatal(out)
	}
	if code, _, _ := e.run("", "import", "show", "../x"); code != 2 {
		t.Fatal("bad job id")
	}
}

func TestImportApplyConfirmation(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	f.h = func(w http.ResponseWriter, r *http.Request, b string) bool {
		if diffHandler(w, r, b) {
			return true
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":apply") {
			reply(w, `{"name":"projects/acme-dw/importJobs/17","done":true,"response":{"state":"APPLIED"}}`)
			return true
		}
		return false
	}
	posts := func() int {
		n := 0
		for _, r := range f.reqs {
			if r.Method == "POST" {
				n++
			}
		}
		return n
	}
	// non-interactive without --yes: usage error, no request at all
	n := len(f.reqs)
	code, _, errs := e.runTTY("", false, "import", "apply", "17")
	if code != 2 || !strings.Contains(errs, "--yes") || len(f.reqs) != n {
		t.Fatal(code, errs)
	}
	// TTY: declined
	code, _, errs = e.runTTY("n\n", true, "import", "apply", "17")
	if code != 1 || !strings.Contains(errs, "CANCELLED") || posts() != 0 || !strings.Contains(errs, "CONFLICT=1") {
		t.Fatal(code, errs, posts())
	}
	// TTY: accepted
	code, out, errs := e.runTTY("y\n", true, "import", "apply", "17", "--request-id", "7d6f7b0e-2f3a-4c1c-9d0e-1a2b3c4d5e6f", "--reason", "x")
	if code != 0 || mustJSON(t, out)["done"] != true || posts() != 1 {
		t.Fatal(code, out, errs)
	}
	body := mustJSON(t, f.last().Body)
	if body["request_id"] != "7d6f7b0e-2f3a-4c1c-9d0e-1a2b3c4d5e6f" || body["reason"] != "x" {
		t.Fatalf("%v", body)
	}
	// --yes works without a terminal and warns about conflicts
	code, _, errs = e.runTTY("", false, "import", "apply", "17", "--yes")
	if code != 0 || !strings.Contains(errs, "CONFLICT/SUSPECT") {
		t.Fatal(code, errs)
	}
	// --dry-run needs no confirmation and sets validate_only
	code, _, _ = e.runTTY("", false, "import", "apply", "17", "--dry-run")
	if mustJSON(t, f.last().Body)["validate_only"] != true || code != 0 {
		t.Fatalf("%+v", f.last())
	}
}

func TestImportApplyPollsAndRollback(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	polls := 0
	state := "APPLIED"
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, ":apply") || strings.HasSuffix(r.URL.Path, ":rollback"):
			reply(w, `{"name":"projects/acme-dw/importJobs/17","done":false}`)
		case r.Method == "GET":
			polls++
			st := "STAGED"
			if polls >= 3 {
				st = state
			}
			reply(w, `{"name":"projects/acme-dw/importJobs/17","state":"`+st+`","summary":{"NEW":1}}`)
		default:
			return false
		}
		return true
	}
	code, out, errs := e.runTTY("", false, "import", "apply", "17", "--yes")
	m := mustJSON(t, out)
	if code != 0 || m["done"] != true || m["response"].(map[string]any)["state"] != "APPLIED" || polls < 3 {
		t.Fatal(code, out, errs, polls)
	}
	// --no-wait returns immediately
	polls = -100
	code, out, _ = e.runTTY("", false, "import", "apply", "17", "--yes", "--no-wait")
	if code != 0 || mustJSON(t, out)["done"] != false {
		t.Fatal(out)
	}
	// FAILED job: exit 1
	polls, state = 0, "FAILED"
	code, out, _ = e.runTTY("", false, "import", "apply", "17", "--yes")
	if code != 1 || !strings.Contains(out, "FAILED") {
		t.Fatal(code, out)
	}
	// rollback waits for ROLLED_BACK
	polls, state = 0, "ROLLED_BACK"
	code, out, errs = e.runTTY("", false, "import", "rollback", "17", "--yes")
	if code != 0 || mustJSON(t, out)["response"].(map[string]any)["state"] != "ROLLED_BACK" {
		t.Fatal(code, out, errs)
	}
	if r := f.find("POST", ":rollback"); r == nil {
		t.Fatal("rollback not called")
	}
	// an Operation error is a runtime error on stderr
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method == "POST" {
			reply(w, `{"name":"x","done":true,"error":{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"job not staged"}}}`)
			return true
		}
		reply(w, `{"state":"APPLIED"}`)
		return true
	}
	code, _, errs = e.runTTY("", false, "import", "apply", "17", "--yes")
	if code != 1 || !strings.Contains(errs, "FAILED_PRECONDITION") {
		t.Fatal(code, errs)
	}
	// 501 while the server side is missing
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		w.WriteHeader(501)
		reply(w, `{"error":{"code":501,"status":"UNIMPLEMENTED","message":"not yet"}}`)
		return true
	}
	if code, _, errs := e.runTTY("", false, "import", "show", "17"); code != 1 || !strings.Contains(errs, "UNIMPLEMENTED") {
		t.Fatal(code, errs)
	}
}

var _ = json.Marshal
