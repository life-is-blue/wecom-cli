package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// wfRow builds a workflow import row with tasks given as (reference, name, hash) triples.
func wfRow(system, ref, name, hash string, tasks ...[3]string) json.RawMessage {
	var ts []map[string]any
	for _, t := range tasks {
		ts = append(ts, map[string]any{
			"source": map[string]any{"reference": t[0], "row_hash": t[2]},
			"data":   map[string]any{"display_name": t[1]},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"source": map[string]any{"system": system, "reference": ref, "row_hash": hash},
		"data":   map[string]any{"display_name": name},
		"tasks":  ts,
	})
	return b
}

func (e *env) liveTasks(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id=?`, e.p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// taskByRef returns the live task imported with the reference.
func (e *env) taskByRef(t *testing.T, ref string) *Entity {
	t.Helper()
	ex, err := loadImportExisting(e.ctx, e.st.DB, e.p.ID, "task")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ex {
		if r, _ := x.Vals["source.ref"], 0; string(r) == strconv.Quote(ref) && !x.Deleted {
			en, err := loadEntity(e.ctx, e.st.DB, e.p.ID, "task", x.ID)
			if err != nil {
				t.Fatal(err)
			}
			return en
		}
	}
	t.Fatalf("no task %s", ref)
	return nil
}

func TestImportTasks(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	id, j := e.stage(t, sys, []json.RawMessage{
		wfRow(sys, "a", "A", "ha", [3]string{"a1", "A one", "h1"}, [3]string{"a2", "A two", "h2"}),
		wfRow(sys, "b", "B", "hb", [3]string{"b1", "B one", "h3"}),
	})
	if sum(j, "NEW") != 2 || sum(j, "TASK_NEW") != 3 {
		t.Fatalf("stage summary %v", *j.Summary)
	}
	a := e.apply(t, id, nil)
	if sum(a, "CREATED") != 2 || sum(a, "TASK_CREATED") != 3 || e.liveTasks(t) != 3 {
		t.Fatalf("apply summary %v tasks=%d", *a.Summary, e.liveTasks(t))
	}
	wfA := e.byRef(t, sys, "a")
	a1 := e.taskByRef(t, "a1")
	if a1.str("name") != "A one" || a1.str("in."+strconv.FormatInt(wfA, 10)) != "primary" {
		t.Fatalf("task a1: %v", a1.Vals)
	}
	var primary int64
	e.st.DB.QueryRow(`SELECT primary_workflow_id FROM tasks WHERE name='A two' AND project_id=?`, e.p.ID).Scan(&primary)
	if primary != wfA {
		t.Fatalf("view primary_workflow_id=%d want %d", primary, wfA)
	}

	// a person labels a1; the source renames it, drops b1 and keeps a2
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: a1.ID, Actor: e.actor("delivery", "editor"), Plan: set("attr.priority", "p1")}); err != nil {
		t.Fatal(err)
	}
	id2, j2 := e.stage(t, sys, []json.RawMessage{
		wfRow(sys, "a", "A", "ha", [3]string{"a1", "A one renamed", "h1x"}, [3]string{"a2", "A two", "h2"}),
		wfRow(sys, "b", "B", "hb"),
	})
	if sum(j2, "UNCHANGED") != 2 || sum(j2, "TASK_CHANGED") != 1 || sum(j2, "TASK_UNCHANGED") != 1 || sum(j2, "TASK_MISSING") != 1 {
		t.Fatalf("restage summary %v", *j2.Summary)
	}
	d := e.diff(t, id2)
	if len(d) == 0 {
		t.Fatal("no diff lines")
	}
	a2 := e.apply(t, id2, nil)
	if sum(a2, "TASK_UPDATED") != 1 || sum(a2, "TASK_CREATED") != 0 || e.liveTasks(t) != 3 {
		t.Fatalf("apply2 summary %v", *a2.Summary)
	}
	if a1 = e.taskByRef(t, "a1"); a1.str("name") != "A one renamed" || a1.str("attr.priority") != "p1" {
		t.Fatalf("source-owned name must update, platform label stay: %v", a1.Vals)
	}
	e.verifyLatest(t)
}

func TestImportTasksRollback(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	id, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "a", "A", "ha", [3]string{"a1", "A one", "h1"}, [3]string{"a2", "A two", "h2"})})
	e.apply(t, id, nil)
	id2, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "a", "A", "ha", [3]string{"a1", "A one v2", "h1x"}, [3]string{"a2", "A two", "h2"}, [3]string{"a3", "A three", "h3"})})
	e.apply(t, id2, nil)
	if e.liveTasks(t) != 3 {
		t.Fatalf("tasks after second apply: %d", e.liveTasks(t))
	}
	// the second job: one task updated, one created; rolling it back restores a1 and removes a3
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id2, ImportApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if e.liveTasks(t) != 2 || e.taskByRef(t, "a1").str("name") != "A one" {
		t.Fatalf("rollback of job 2: tasks=%d", e.liveTasks(t))
	}
	e.verifyLatest(t)
	// the first job creates the workflow and its tasks; rolling it back soft-deletes them all
	e2 := newEnv(t)
	id3, _ := e2.stage(t, sys, []json.RawMessage{wfRow(sys, "a", "A", "ha", [3]string{"a1", "A one", "h1"}, [3]string{"a2", "A two", "h2"})})
	e2.apply(t, id3, nil)
	if _, err := e2.st.RollbackImport(e2.ctx, e2.p, e2.actor("delivery", "admin"), id3, ImportApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if e2.liveTasks(t) != 0 || e2.liveWorkflows(t) != 0 {
		t.Fatalf("after rollback: tasks=%d workflows=%d", e2.liveTasks(t), e2.liveWorkflows(t))
	}
	e2.verifyLatest(t)
}

func TestImportScaleTasks(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	const wfs, perWf = 3000, 3 // 3000 workflows, ~10000 tasks
	var rr []json.RawMessage
	nTasks := 0
	for i := 0; i < wfs; i++ {
		var ts [][3]string
		for k := 0; k < perWf+(i%2); k++ { // 3 or 4 tasks, 10500 in total
			ts = append(ts, [3]string{fmt.Sprintf("t_%04d_%d", i, k), fmt.Sprintf("task %d.%d", i, k), fmt.Sprintf("th%d_%d", i, k)})
			nTasks++
		}
		rr = append(rr, wfRow(sys, fmt.Sprintf("w_%04d", i), fmt.Sprintf("wf %d", i), fmt.Sprintf("wh%d", i), ts...))
	}
	t0 := time.Now()
	id, j := e.stage(t, sys, rr)
	tStage := time.Since(t0)
	if sum(j, "TASK_NEW") != nTasks {
		t.Fatalf("summary %v", *j.Summary)
	}
	t0 = time.Now()
	a := e.apply(t, id, nil)
	tApply := time.Since(t0)
	if sum(a, "TASK_CREATED") != nTasks || e.liveTasks(t) != nTasks {
		t.Fatalf("apply: %v", *a.Summary)
	}
	t.Logf("stage %v, apply %d workflows + %d tasks %v", tStage, wfs, nTasks, tApply)
	t0 = time.Now()
	if _, j2 := e.stage(t, sys, rr); sum(j2, "TASK_UNCHANGED") != nTasks {
		t.Fatalf("restage %v", *j2.Summary)
	}
	t.Logf("re-stage %v", time.Since(t0))
	t0 = time.Now()
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("rollback %v", time.Since(t0))
	if e.liveTasks(t) != 0 {
		t.Fatal("tasks remain after rollback")
	}
}
