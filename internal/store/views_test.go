package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

var publicViews = []string{"workflows", "tasks", "events", "comments", "workflow_stages", "stage_distribution", "open_items"}

func (e *env) write(t *testing.T, typ string, id int64, actor core.Actor, kv ...string) *Result {
	t.Helper()
	res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: typ, EntityID: id, Actor: actor, Plan: set(kv...)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) comment(t *testing.T, wf int64, kind string) int64 {
	t.Helper()
	r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "comment", Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "on", New: core.JSON(wf)}, {Key: "kind", New: core.JSON(kind)}, {Key: "body", New: core.JSON("b")}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r.EntityID
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRowContext(e.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *env) del(t *testing.T, typ string, id int64, v bool) error {
	_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: typ, EntityID: id, Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "deleted", New: core.JSON(v)}}, nil
		}})
	return err
}

// A soft-deleted workflow vanishes from every public view that is keyed by workflow, like from `workflows`;
// its audit events stay. A workflow that only has notes has no open_items row.
func TestViewsHideDeletedWorkflows(t *testing.T) {
	e := newEnv(t)
	live, gone, notes := e.createWF(t, "live"), e.createWF(t, "gone"), e.createWF(t, "notes")
	e.comment(t, live, "issue")
	e.comment(t, gone, "issue")
	e.comment(t, gone, "risk")
	e.comment(t, notes, "note")
	perWF := func(view string, wf int64) int {
		return e.count(t, "SELECT COUNT(*) FROM "+view+" WHERE project_id = ? AND "+map[string]string{"comments": "workflow_id", "workflow_stages": "workflow_id", "open_items": "workflow_id", "workflows": "id"}[view]+" = ?", e.p.ID, wf)
	}
	for _, v := range []string{"workflows", "workflow_stages", "open_items", "comments"} {
		if perWF(v, gone) == 0 {
			t.Fatalf("%s: the live workflow has no rows (test setup)", v)
		}
	}
	if err := e.del(t, "workflow", gone, true); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"workflows", "workflow_stages", "open_items", "comments"} {
		if n := perWF(v, gone); n != 0 {
			t.Errorf("%s exposes %d rows of a soft-deleted workflow", v, n)
		}
		if perWF(v, live) == 0 {
			t.Errorf("%s lost the live workflow", v)
		}
	}
	if e.count(t, "SELECT COUNT(*) FROM events WHERE project_id = ? AND entity_id = ?", e.p.ID, gone) == 0 {
		t.Error("the audit trail of a deleted workflow must stay")
	}
	if n := e.count(t, "SELECT COUNT(*) FROM stage_distribution WHERE project_id = ?", e.p.ID); n == 0 {
		t.Error("stage_distribution is empty")
	}
	if n := e.count(t, "SELECT COALESCE(SUM(workflows),0) FROM stage_distribution WHERE project_id = ?", e.p.ID); n != 2 {
		t.Errorf("stage_distribution counts %d workflows, want the 2 live ones", n)
	}
	if n := perWF("open_items", notes); n != 0 {
		t.Errorf("open_items has %d rows for a workflow with only notes", n)
	}
	// the server still renders a deleted workflow with its derived state (internal views are not filtered)
	w, err := e.st.GetWorkflow(e.ctx, e.p, gone)
	if err != nil || w.Health == nil || string(*w.Health) != "BLOCKED" {
		t.Fatalf("deleted workflow render: %v %v", w, err)
	}
	if err := e.del(t, "workflow", gone, false); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"workflows", "workflow_stages", "open_items", "comments"} {
		if perWF(v, gone) == 0 {
			t.Errorf("%s: undelete did not restore the rows", v)
		}
	}
}

// task creates a task of the workflow (the value of the key `workflow` is the workflow's entity id).
func (e *env) task(t *testing.T, wf int64, name string) int64 {
	t.Helper()
	r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "name", New: core.JSON(name)}, {Key: "workflow", New: core.JSON(wf)}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r.EntityID
}

// tasks.workflow_id is the workflow the task belongs to; moving the task changes it, and a task cannot be placed in a
// deleted or a non-numeric workflow.
func TestTaskWorkflowView(t *testing.T) {
	e := newEnv(t)
	a, b := e.createWF(t, "a"), e.createWF(t, "b")
	tk := e.task(t, b, "t")
	wid := func() sql.NullInt64 {
		var n sql.NullInt64
		if err := e.st.DB.QueryRowContext(e.ctx, "SELECT workflow_id FROM tasks WHERE id = ?", tk).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if wid().Int64 != b {
		t.Fatalf("workflow_id %v, want %d", wid(), b)
	}
	move := func(wf int64) error {
		_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: tk, Actor: e.actor("delivery", "editor"),
			Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
				return []PlannedChange{{Key: "workflow", New: core.JSON(wf)}}, nil
			}})
		return err
	}
	if err := move(a); err != nil || wid().Int64 != a {
		t.Fatalf("after the move: %v %v, want %d", wid(), err, a)
	}
	if err := e.del(t, "workflow", b, true); err != nil {
		t.Fatal(err)
	}
	coreErr(t, move(b), core.StatusFailedPrecondition, "ENTITY_DELETED")
	if wid().Int64 != a {
		t.Fatal("refused move changed the task")
	}
	_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: tk, Actor: e.actor("delivery", "editor"), Plan: set("workflow", "5")})
	coreErr(t, err, core.StatusInvalidArgument, "INVALID_VALUE")
}

// A-4: a soft-deleted workflow or task accepts nothing but its restore; a comment of a deleted workflow is read-only.
func TestDeletedEntitiesAreReadOnly(t *testing.T) {
	e := newEnv(t)
	ed := e.actor("delivery", "editor")
	apply := func(typ string, id int64, kv ...string) error {
		_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: typ, EntityID: id, Actor: ed, Plan: set(kv...)})
		return err
	}
	wf := e.createWF(t, "w")
	cm := e.comment(t, wf, "issue")
	tk := e.write(t, "task", 0, ed, "name", "t").EntityID
	for _, c := range []struct {
		typ string
		id  int64
	}{{"workflow", wf}, {"task", tk}} {
		if err := e.del(t, c.typ, c.id, true); err != nil {
			t.Fatal(err)
		}
		coreErr(t, apply(c.typ, c.id, "name", "x"), core.StatusFailedPrecondition, "ENTITY_DELETED")
		coreErr(t, apply(c.typ, c.id, "attr.group", "g"), core.StatusFailedPrecondition, "ENTITY_DELETED")
		if c.typ == "workflow" {
			coreErr(t, apply(c.typ, c.id, "stage.development", "pass"), core.StatusFailedPrecondition, "ENTITY_DELETED")
			coreErr(t, apply(c.typ, c.id, "scope", "out"), core.StatusFailedPrecondition, "ENTITY_DELETED")
			// the comment of a deleted workflow is read-only too
			coreErr(t, apply("comment", cm, "resolved", "true"), core.StatusFailedPrecondition, "ENTITY_DELETED")
			coreErr(t, apply("comment", cm, "deleted", "true"), core.StatusFailedPrecondition, "ENTITY_DELETED")
		}
		if err := e.del(t, c.typ, c.id, true); err != nil { // deleting again changes nothing
			t.Fatalf("repeat delete: %v", err)
		}
		if err := e.del(t, c.typ, c.id, false); err != nil {
			t.Fatalf("undelete: %v", err)
		}
		if err := apply(c.typ, c.id, "name", "x"); err != nil {
			t.Fatalf("write after undelete: %v", err)
		}
	}
	if err := apply("comment", cm, "resolved", "true"); err != nil {
		t.Fatalf("comment after undelete: %v", err)
	}
	// import and legacy events are historical facts and still bypass the rule
	if err := e.del(t, "workflow", wf, true); err != nil {
		t.Fatal(err)
	}
	e.write(t, "workflow", wf, core.Actor{Account: "x", Side: "system", Role: "editor", Channel: core.ChannelLegacy}, "name", "legacy")
}

// Every string column of every public view can be compared with a literal from a connection of any
// collation (error 1267 'Illegal mix of collations' otherwise).
func TestViewsAreCollationSafe(t *testing.T) {
	e := newEnv(t)
	wf := e.createWF(t, "w")
	e.comment(t, wf, "issue")
	e.task(t, wf, "t")

	cols := map[string][]string{}
	rows, err := e.st.DB.QueryContext(e.ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name IN ('`+strings.Join(publicViews, "','")+`')
		AND data_type IN ('varchar','char','text','tinytext','mediumtext','longtext','enum') ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v, c string
		rows.Scan(&v, &c)
		cols[v] = append(cols[v], c)
	}
	rows.Close()
	for _, must := range []string{"workflows.code", "workflows.scope", "workflows.health", "tasks.code", "workflow_stages.status", "stage_distribution.stage"} {
		v, c, _ := strings.Cut(must, ".")
		found := false
		for _, x := range cols[v] {
			found = found || x == c
		}
		if !found {
			t.Fatalf("expected string column %s in the view", must)
		}
	}

	probe := func(collation string) error {
		conn, err := e.st.DB.Conn(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(e.ctx, "SET NAMES utf8mb4 COLLATE "+collation); err != nil {
			t.Fatal(err)
		}
		for v, cs := range cols {
			for _, c := range cs {
				q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = 'blocked' OR %s LIKE 'x%%' OR %s IN ('a', 'b') OR %s <> 'in'", v, c, c, c, c)
				var n int
				if err := conn.QueryRowContext(e.ctx, q).Scan(&n); err != nil {
					return fmt.Errorf("%s.%s under %s: %w", v, c, collation, err)
				}
			}
		}
		for _, v := range publicViews {
			rs, err := conn.QueryContext(e.ctx, "SELECT * FROM "+v+" LIMIT 5")
			if err != nil {
				return fmt.Errorf("SELECT * FROM %s under %s: %w", v, collation, err)
			}
			rs.Close()
		}
		var n int
		if err := conn.QueryRowContext(e.ctx, "SELECT COUNT(*) FROM workflows WHERE project_id = ? AND health = 'blocked' AND scope = 'in'", e.p.ID).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("health = 'blocked' under %s: %d %v", collation, n, err)
		}
		return nil
	}
	for _, c := range []string{"utf8mb4_general_ci", "utf8mb4_0900_ai_ci", "utf8mb4_unicode_ci", "utf8mb4_bin"} {
		if err := probe(c); err != nil {
			t.Error(err)
		}
	}

	// the probe can fail: a view that keeps a bare literal breaks under a different collation
	ctx := context.Background()
	conn, err := e.st.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.ExecContext(ctx, "SET NAMES utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	if _, err := conn.ExecContext(ctx, "CREATE OR REPLACE VIEW zz_collation_probe AS SELECT CASE WHEN 1 THEN 'blocked' END AS health"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "DROP VIEW IF EXISTS zz_collation_probe")
	conn.ExecContext(ctx, "SET NAMES utf8mb4 COLLATE utf8mb4_general_ci")
	var n int
	err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM zz_collation_probe WHERE health = 'blocked'").Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "1267") {
		t.Errorf("control view should fail with 1267 under another collation, got %v", err)
	}
}
