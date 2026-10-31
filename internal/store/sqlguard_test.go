package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

var guardBypasses = []string{
	"SELECT * FROM (api_token)",
	"SELECT * FROM tasks JOIN (api_token) ON 1",
	"SELECT * FROM (tasks)",
	"SELECT * FROM (tasks JOIN workflows ON 1)",
	"SELECT * FROM (openetl_x.api_token)",
	"SELECT * FROM openetl_x.api_token",
	"SELECT * FROM `openetl_x`.`api_token`",
	"SELECT * FROM `openetl_x.api_token`",
	"SELECT * FROM ((api_token))",
	"SELECT * FROM tasks, api_token",
	"SELECT * FROM tasks t, (api_token) a",
	"SELECT * FROM tasks UNION SELECT * FROM tasks",
	"SELECT 1 UNION TABLE api_token",
	"SELECT 1 UNION VALUES ROW(1)",
	"SELECT 1 UNION ALL SELECT 2",
	"SELECT 1 INTERSECT SELECT 1",
	"SELECT 1 EXCEPT SELECT 1",
	"TABLE api_token",
	"VALUES ROW(1)",
	"WITH x AS (SELECT 1) SELECT * FROM x",
	"SELECT * FROM (SELECT * FROM api_token) t",
	"SELECT x.n FROM (SELECT COUNT(*) AS n FROM comments) x",
	"SELECT (SELECT secret FROM api_token)",
	"SELECT * FROM tasks WHERE id IN (SELECT id FROM api_token)",
	"SELECT * FROM tasks WHERE EXISTS (SELECT 1)",
	"SELECT * FROM tasks WHERE id = ANY (SELECT 1)",
	"SELECT SLEEP(5)", "SELECT sleep (5)", "SELECT `sleep`(5)", "SELECT BENCHMARK(1,1)", "SELECT NOW()", "SELECT UUID()",
	"SELECT RAND()", "SELECT LOAD_FILE('/etc/passwd')", "SELECT GET_LOCK('a',1)", "SELECT VERSION()", "SELECT DATABASE()", "SELECT USER()",
	"SELECT openetl.sleep(1)", "SELECT a.COUNT(1)",
	"SELECT * FROM tasks FOR UPDATE", "SELECT * FROM tasks INTO OUTFILE '/tmp/x'", "SELECT 1 INTO @a",
	"SELECT * FROM tasks -- x", "SELECT /* x */ 1", "SELECT /*!50000 1 */", "SELECT 1 # x",
	"SELECT 1; SELECT 2", "SELECT 1; DROP TABLE x", "SELECT @@version", "SELECT @a", "SELECT 1 FROM dual",
	"SELECT * FROM tasks USE INDEX (x)", "SELECT * FROM tasks STRAIGHT_JOIN api_token", "SELECT * FROM tasks PARTITION (p0)",
	"SELECT * FROM tasks FROM api_token", "SELECT SUBSTRING(a FROM 1) FROM tasks",
	"SELECT * FROM tasks JOIN LATERAL (SELECT 1) x",
	"SELECT * FROM tasks\f(api_token)", "SELECT * FROM\v(api_token)", "SELECT * FROM tasks WHERE a = \"x\"",
	"SELECT '\\' FROM tasks WHERE '", "SELECT * FROM `tasks``` WHERE 1", "SELECT 1from tasks",
	"SELECT * FROM", "SELECT * FROM tasks JOIN", "SELECT * FROM tasks JOIN ON 1", "SELECT * FROM tasks AS",
	"SELECT * FROM entity", "SELECT * FROM v_workflow", "SELECT * FROM information_schema.tables", "SELECT * FROM mysql.user",
	"SELECT * FROM Api_Token", "SELECT * FROM `API_TOKEN`", "SELECT * FROM tasks)", "SELECT (1", "UPDATE tasks SET a=1", "DELETE FROM tasks",
	"SELECT * FROM tasks WINDOW w AS ()", "SELECT COUNT(*) OVER () FROM tasks", "HANDLER x OPEN", "SELECT 1 PROCEDURE ANALYSE()",
	"SELECT :=1",
	// cartesian-looking joins
	"SELECT count(*) FROM workflows a JOIN workflows b ON 1 JOIN workflows c ON 1",
	"SELECT * FROM workflows a JOIN workflows b", "SELECT * FROM workflows a CROSS JOIN workflows b",
	"SELECT * FROM workflows a NATURAL JOIN workflows b", "SELECT * FROM workflows a JOIN workflows b ON true",
	"SELECT * FROM workflows a JOIN workflows b ON 1=1", "SELECT * FROM workflows a JOIN workflows b ON (1)",
	"SELECT * FROM workflows a JOIN workflows b ON 'a'='a'", "SELECT * FROM workflows a LEFT JOIN workflows b ON a.id = b.id OR 1",
	"SELECT * FROM workflows a JOIN workflows b ON a.id = b.id OR 1=1", "SELECT * FROM workflows a JOIN workflows b ON a.id = b.id JOIN tasks c",
	"SELECT * FROM workflows a JOIN workflows b ON a.id = b.id OR TRUE",
	// system information, with or without parentheses
	"SELECT current_user", "SELECT CURRENT_USER()", "SELECT session_user", "SELECT system_user", "SELECT user", "SELECT user()",
	"SELECT database", "SELECT schema", "SELECT version", "SELECT connection_id", "SELECT last_insert_id", "SELECT row_count",
	"SELECT found_rows", "SELECT id FROM workflows WHERE name = database", "SELECT @@global.version", "SELECT @@hostname", "SELECT 1 FROM tasks LIMIT 1 OFFSET 1 UNION SELECT 2",
}

func TestQueryGuardBypasses(t *testing.T) {
	for _, q := range guardBypasses {
		if out, err := GuardQuery(q, 1); err == nil {
			t.Errorf("accepted %q -> %q", q, out)
		}
	}
}

func TestQueryGuardAccepts(t *testing.T) {
	good := []string{
		"SELECT stage, health, workflows FROM stage_distribution",
		"SELECT w.id, t.id FROM workflows w JOIN tasks t ON t.primary_workflow_id = w.id WHERE w.name LIKE 'a;b%' ORDER BY 1 LIMIT 5;",
		"select count(*) from WORKFLOWS where health = 'blocked'",
		"SELECT * FROM `workflows`",
		"SELECT w.id FROM workflows AS w LEFT JOIN tasks AS t ON (t.primary_workflow_id = w.id) WHERE w.id IN (1,2,3)",
		"SELECT stage, COUNT(*) c, COALESCE(MAX(id), 0) FROM tasks GROUP BY stage HAVING COUNT(*) > 1 ORDER BY c DESC",
		"SELECT CAST(id AS CHAR), JSON_UNQUOTE(JSON_EXTRACT(attrs, '$.a')) FROM workflows",
		"SELECT 1",
		"SELECT a.id FROM workflows a JOIN tasks b ON b.primary_workflow_id = a.id AND b.id > 0 JOIN events e ON e.workflow_id = a.id",
		"SELECT a.id FROM workflows a JOIN tasks b USING (project_id)",
		"SELECT a.id FROM workflows a LEFT JOIN tasks b ON a.id = b.primary_workflow_id OR b.id = a.id",
		"SELECT a.id FROM workflows a INNER JOIN tasks `b` ON `b`.primary_workflow_id = a.id WHERE 1 = 1",
	}
	for _, q := range good {
		out, err := GuardQuery(q, 7)
		if err != nil {
			t.Errorf("rejected %q: %v", q, err)
			continue
		}
		if !strings.Contains(strings.ToLower(q), "from") {
			continue
		}
		if !strings.Contains(out, "project_id = 7") {
			t.Errorf("no project filter in %q -> %q", q, out)
		}
	}
	// case-insensitive names and aliases are still rewritten
	out, _ := GuardQuery("SELECT * FROM TASKS T JOIN Workflows W ON T.id = W.id", 3)
	if strings.Count(out, "project_id = 3") != 2 {
		t.Errorf("both references must be filtered: %q", out)
	}
}

// checkAccepted asserts the invariant on an accepted, rewritten statement: only one outer SELECT,
// and every FROM/JOIN is followed by a project-filtered derived table over a public view.
func checkAccepted(in, out string, pid int64) error {
	toks, err := tokenizeSQL(out)
	if err != nil {
		return fmt.Errorf("output does not tokenize: %v", err)
	}
	selects, refs := 0, 0
	for i, tk := range toks {
		up := strings.ToUpper(tk.text)
		if tk.kind == 'w' && up == "SELECT" {
			selects++
		}
		if tk.kind != 'w' || (up != "FROM" && up != "JOIN") {
			continue
		}
		// inner "(SELECT * FROM `view` WHERE project_id = N)" : FROM preceded by "SELECT *"
		if i >= 2 && !(i+1 < len(toks) && toks[i+1].text == "(") && toks[i-2].kind == 'w' && strings.EqualFold(toks[i-2].text, "SELECT") && toks[i-1].text == "*" {
			if i+5 >= len(toks) || toks[i+1].kind != 'q' || !PublicViews[toks[i+1].text] ||
				!strings.EqualFold(toks[i+2].text, "WHERE") || !strings.EqualFold(toks[i+3].text, "project_id") ||
				toks[i+4].text != "=" || toks[i+5].text != fmt.Sprint(pid) {
				return fmt.Errorf("inner FROM is not a filtered view: %q", out)
			}
			refs++
			continue
		}
		// outer FROM/JOIN must be followed by "( SELECT * FROM"
		if i+4 >= len(toks) || toks[i+1].text != "(" || !strings.EqualFold(toks[i+2].text, "SELECT") || toks[i+3].text != "*" || !strings.EqualFold(toks[i+4].text, "FROM") {
			return fmt.Errorf("unfiltered %s in %q (from %q)", up, out, in)
		}
	}
	if selects != 1+refs {
		return fmt.Errorf("select count %d vs refs %d in %q", selects, refs, out)
	}
	return nil
}

func FuzzGuardQuery(f *testing.F) {
	for _, q := range guardBypasses {
		f.Add(q)
	}
	f.Add("SELECT * FROM tasks t JOIN workflows w ON t.id = w.id")
	f.Add("select a from `comments` where x = 'y'")
	f.Fuzz(func(t *testing.T, q string) {
		out, err := GuardQuery(q, 42)
		if err != nil {
			return
		}
		if err := checkAccepted(q, out, 42); err != nil {
			t.Fatal(err)
		}
	})
}

func TestQueryLimits(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.createWF(t, fmt.Sprintf("wf-%d-%s", i, strings.Repeat("x", 200)))
	}
	// page_size is capped at 1000
	r, err := e.st.Query(e.ctx, e.p, "SELECT id FROM workflows", 1000000, "")
	if err != nil || len(*r.Rows) != 5 {
		t.Fatalf("%v %v", r, err)
	}
	// response byte cap
	old := queryMaxBytes
	queryMaxBytes = 500
	defer func() { queryMaxBytes = old }()
	_, err = e.st.Query(e.ctx, e.p, "SELECT name FROM workflows", 0, "")
	coreErr(t, err, core.StatusInvalidArgument, "RESULT_TOO_LARGE")
	queryMaxBytes = old
	// the statement timeout is installed on the session (MySQL or MariaDB variable)
	conn, err := e.st.DB.Conn(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reset, err := setStatementTimeout(e.ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if conn.QueryRowContext(e.ctx, "SELECT @@max_statement_time").Scan(&v) != nil || v == "0" || v == "0.000000" {
		if conn.QueryRowContext(e.ctx, "SELECT @@max_execution_time").Scan(&v) != nil || v == "0" {
			t.Fatalf("no timeout set: %q", v)
		}
	}
	reset()
}
