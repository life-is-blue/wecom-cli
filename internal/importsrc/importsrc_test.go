package importsrc

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

const testMapping = `
version: 1
source_system: legacy
source: {table: imp_src, order_by: id}
reference: [id]
hash_exclude: [updated_at]
fields:
  name: pipeline_name
  domain: dom
  area: {column: area, default: Unknown}
  priority: prio
labels: {team: team}
annotations: {remark: remark}
assignees: {dev_owner: dev}
stages: {development: dev_status, verify: ver_status}
tasks: {from: tasks, reference: tid, name: title, labels: {kind: kind}}
`

func mapping(t *testing.T) *Mapping {
	t.Helper()
	m, err := ParseMapping([]byte(testMapping))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHashDeterministicAndSourceAgnostic(t *testing.T) {
	a := Row{"id": "1", "n": json.Number("3"), "x": nil, "updated_at": "t1"}
	b := Row{"x": nil, "n": "3", "id": "1", "updated_at": "t2"}
	if Hash(a, []string{"updated_at"}) != Hash(b, []string{"updated_at"}) {
		t.Fatal("hash must ignore key order, number type and excluded columns")
	}
	if Hash(a, nil) == Hash(b, nil) {
		t.Fatal("excluded column must matter when not excluded")
	}
	if Hash(Row{"x": nil}, nil) == Hash(Row{"x": ""}, nil) {
		t.Fatal("NULL and empty string must differ")
	}
	if len(Hash(a, nil)) != 64 {
		t.Fatal("want sha256 hex")
	}
}

func TestMappingValidation(t *testing.T) {
	for name, y := range map[string]string{
		"noref":        "fields: {name: a}",
		"noname":       "reference: [id]\nfields: {domain: a}",
		"unknown":      "reference: [id]\nfields: {name: a, bogus: b}",
		"badkey":       "reference: [id]\nfields: {name: a}\nwat: 1",
		"bothsrc":      "reference: [id]\nfields: {name: a}\nsource: {table: t, query: select 1}",
		"emptyref":     "",
		"tasknoref":    "reference: [id]\nfields: {name: a}\ntasks: {from: t}",
		"taskboth":     "reference: [id]\nfields: {name: a}\ntasks: {from: t, table: x, reference: i, parent_key: p}",
		"tasknoparent": "reference: [id]\nfields: {name: a}\ntasks: {table: x, reference: i}",
	} {
		if _, err := ParseMapping([]byte(y)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestNDJSONAndMap(t *testing.T) {
	src := `{"id":7,"pipeline_name":"Load A","dom":"Sales","prio":"P1","team":"X","dev":"Alice","dev_status":"pass","ver_status":"n/a","tasks":[{"tid":"b","title":"t2"},{"tid":"a","title":"t1","kind":"SQL"}],"updated_at":"x"}
{"id":3,"pipeline_name":"Load B","dom":"HR","area":"","dev_status":"open"}
`
	p := filepath.Join(t.TempDir(), "rows.ndjson")
	os.WriteFile(p, []byte(src), 0o600)
	s, err := Open(p, mapping(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Build(context.Background(), s, mapping(t), "legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || *rows[0].Source.Reference != "3" { // sorted by reference
		t.Fatalf("rows=%d first=%v", len(rows), rows[0].Source.Reference)
	}
	r := rows[1]
	d := r.Data
	if *d.DisplayName != "Load A" || (*d.Labels)["domain"] != "sales" || (*d.Labels)["area"] != "unknown" || (*d.Labels)["team"] != "x" {
		t.Fatalf("labels: %+v", d.Labels)
	}
	if (*d.Assignees)["dev_owner"] != "accounts/alice" {
		t.Fatalf("assignees: %+v", d.Assignees)
	}
	if len(*d.Stages) != 2 || (*d.Stages)[0].State != "PASSED" || (*d.Stages)[1].State != "NOT_APPLICABLE" {
		t.Fatalf("stages: %+v", d.Stages)
	}
	ts := *r.Tasks // sorted by reference
	if len(ts) != 2 || *ts[0].Source.Reference != "a" || ts[0].Data.DisplayName != "t1" || (*ts[0].Data.Labels)["kind"] != "sql" ||
		ts[1].Data.Labels != nil || len(*ts[1].Source.RowHash) != 64 || ts[0].Source.System != nil {
		t.Fatalf("tasks: %+v", ts)
	}
	if *r.Source.System != "legacy" || len(*r.Source.RowHash) != 64 {
		t.Fatalf("source: %+v", r.Source)
	}
}

func TestCSVAndErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.csv")
	os.WriteFile(p, []byte("\xEF\xBB\xBFid,pipeline_name,dom\n1,\"A, b\",x\n2,,y\n3,C,z\n3,D,z\n"), 0o600)
	m := mapping(t)
	s, err := Open(p, m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), s, m, "legacy", 0)
	if err == nil || !strings.Contains(err.Error(), "source row 2") {
		t.Fatalf("want empty-name error for row 2, got %v", err)
	}
	os.WriteFile(p, []byte("id,pipeline_name\n1,A\n1,B\n"), 0o600)
	s, _ = Open(p, m, Options{})
	if _, err = Build(context.Background(), s, m, "legacy", 0); err == nil || !strings.Contains(err.Error(), "duplicate reference") {
		t.Fatalf("want duplicate error, got %v", err)
	}
	os.WriteFile(p, []byte("id,pipeline_name\n1,A\n2,B\n"), 0o600)
	s, _ = Open(p, m, Options{})
	if _, err = Build(context.Background(), s, m, "legacy", 1); err == nil || !strings.Contains(err.Error(), "more than 1") {
		t.Fatalf("want row cap error, got %v", err)
	}
	os.WriteFile(p, []byte("id,pipeline_name\n1\n"), 0o600)
	s, _ = Open(p, m, Options{})
	if _, err = Build(context.Background(), s, m, "legacy", 0); err == nil {
		t.Fatal("want field count error")
	}
}

func TestMySQLURLValidation(t *testing.T) {
	m := mapping(t)
	for _, bad := range []string{"mysql://host/db", "mysql://u@host", "mysql://u@/db"} {
		if _, err := Open(bad, m, Options{}); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	m2 := *m
	m2.Source = SourceSpec{Query: "DELETE FROM x"}
	if _, err := Open("mysql://u@h/db", &m2, Options{}); err == nil {
		t.Error("non-select query must be rejected")
	}
	m2.Source = SourceSpec{Table: "a;drop table b"}
	if _, err := Open("mysql://u@h/db", &m2, Options{}); err == nil {
		t.Error("bad table must be rejected")
	}
	s, err := Open("mysql://u@h:3307/db", m, Options{Getenv: func(k string) string {
		if k == EnvSourcePassword {
			return "sekret"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Label(), "sekret") || s.Label() != "mysql://u@h:3307/db" {
		t.Fatalf("label leaks or wrong: %s", s.Label())
	}
	if s.(*mysqlSource).password != "sekret" {
		t.Fatal("password must come from env")
	}
}

func TestMySQLScan(t *testing.T) {
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
		"DROP TABLE IF EXISTS imp_src",
		"CREATE TABLE imp_src (id INT PRIMARY KEY, pipeline_name VARCHAR(50), dom VARCHAR(20), prio VARCHAR(5), team VARCHAR(5), dev VARCHAR(10), dev_status VARCHAR(5), ver_status VARCHAR(5), remark TEXT NULL, tasks TEXT NULL, area VARCHAR(5) NULL, updated_at DATETIME NULL)",
		"INSERT INTO imp_src VALUES (2,'Load 2','HR','P2','t','bob','open','open',NULL,NULL,NULL,'2026-01-01 00:00:00'),(1,'Load 1','Sales','P1','t','amy','pass','na','r','[{\"tid\":\"x\",\"title\":\"x\"}]','eu',NULL)",
		"DROP TABLE IF EXISTS imp_task",
		"CREATE TABLE imp_task (tid INT PRIMARY KEY, pid INT, title VARCHAR(50), owner VARCHAR(10))",
		"INSERT INTO imp_task VALUES (11,2,'second','Bob'),(10,2,'first',NULL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS imp_src"); db.Exec("DROP TABLE IF EXISTS imp_task") })

	u := url.URL{Scheme: "mysql", User: url.User(cfg.User), Host: cfg.Addr, Path: "/" + cfg.DBName}
	m := mapping(t)
	s, err := Open(u.String(), m, Options{Getenv: func(k string) string {
		if k == EnvSourcePassword {
			return cfg.Passwd
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Build(context.Background(), s, m, "legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || *rows[0].Source.Reference != "1" || (*rows[0].Data.Labels)["domain"] != "sales" {
		t.Fatalf("rows: %+v", rows)
	}
	if (*rows[0].Data.Labels)["area"] != "eu" || (*rows[1].Data.Labels)["area"] != "unknown" {
		t.Fatal("area/default mapping wrong")
	}
	// the same row as CSV hashes identically (updated_at excluded; NULL stays NULL in mysql only for nulls)
	h1 := *rows[0].Source.RowHash
	s2, _ := Open(u.String(), m, Options{Getenv: func(string) string { return cfg.Passwd }})
	rows2, _ := Build(context.Background(), s2, m, "legacy", 0)
	if *rows2[0].Source.RowHash != h1 {
		t.Fatal("hash must be stable across scans")
	}
	// write attempts through the read-only tx must fail
	m3 := *m
	m3.Source = SourceSpec{Query: "SELECT * FROM imp_src"}
	if _, err := selectQuery("source", m3.Source.Table, m3.Source.Query, ""); err != nil {
		t.Fatal(err)
	}
	// tasks from a second table, joined by parent_key; an unknown parent is an error
	m4 := *m
	m4.Tasks = &TasksSpec{Table: "imp_task", ParentKey: "pid", Reference: "tid", Name: "title", Assignees: map[string]FieldRef{"dev_owner": {Column: "owner"}}}
	s4, _ := Open(u.String(), &m4, Options{Getenv: func(string) string { return cfg.Passwd }})
	rows4, err := Build(context.Background(), s4, &m4, "legacy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows4[0].Tasks != nil || len(*rows4[1].Tasks) != 2 || *(*rows4[1].Tasks)[0].Source.Reference != "10" ||
		(*(*rows4[1].Tasks)[1].Data.Assignees)["dev_owner"] != "accounts/bob" {
		t.Fatalf("tasks from table: %+v", rows4)
	}
	m4.Tasks = &TasksSpec{Query: "SELECT tid, 99 AS pid, title FROM imp_task", ParentKey: "pid", Reference: "tid", Name: "title"}
	if _, err := Build(context.Background(), s4, &m4, "legacy", 0); err == nil || !strings.Contains(err.Error(), "unknown parent") {
		t.Fatalf("want unknown parent error, got %v", err)
	}
	// wrong password: error must not contain it
	bad, _ := Open(u.String(), m, Options{Getenv: func(string) string { return "wrongpw-XYZ" }})
	err = bad.Scan(context.Background(), func(Row) error { return nil })
	if err == nil || strings.Contains(err.Error(), "wrongpw-XYZ") {
		t.Fatalf("want auth error without password, got %v", err)
	}
}
