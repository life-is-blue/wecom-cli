package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// orphanTasks counts live tasks whose workflow is soft-deleted (INV-30 says: none, ever).
func (e *env) orphanTasks(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT COUNT(*) FROM entity t
		JOIN entity_latest lw ON lw.entity_id = t.id AND lw.k = 'workflow'
		JOIN entity_latest wd ON wd.entity_id = CAST(JSON_UNQUOTE(lw.v) AS UNSIGNED) AND wd.k = 'deleted' AND JSON_UNQUOTE(wd.v) = 'true'
		LEFT JOIN entity_latest td ON td.entity_id = t.id AND td.k = 'deleted'
		WHERE t.project_id = ? AND t.type = 'task' AND COALESCE(JSON_UNQUOTE(td.v), 'false') <> 'true'`, e.p.ID)
}

// INV-30 and import: a row whose workflow was deleted after the job was staged (unchanged row hash, so the
// plan says UNCHANGED) must not create or move tasks; it is a conflict that no resolution applies to.
func TestImportNeverPlacesTasksUnderDeletedWorkflow(t *testing.T) {
	const sys = "legacy"
	{
		e := newEnv(t)
		id1, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "w", "W", "hw", [3]string{"a", "A", "ha"})})
		e.apply(t, id1, nil)
		w := e.byRef(t, sys, "w")
		id2, j2 := e.stage(t, sys, []json.RawMessage{wfRow(sys, "w", "W", "hw", [3]string{"a", "A", "ha"}, [3]string{"b", "B", "hb"}, [3]string{"c", "C", "hc"})})
		if sum(j2, "UNCHANGED") != 1 {
			t.Fatalf("stage %v", *j2.Summary)
		}
		if err := e.del(t, "workflow", w, true); err != nil {
			t.Fatal(err)
		}
		a := e.apply(t, id2, nil)
		if sum(a, "TASK_CREATED") != 0 || sum(a, "CONFLICT") != 1 || sum(a, "UNCHANGED") != 0 {
			t.Fatalf("apply summary %v", *a.Summary)
		}
		if n := e.orphanTasks(t); n != 0 {
			t.Fatalf("%d live tasks under the deleted workflow", n)
		}
		if n := e.count(t, `SELECT COUNT(*) FROM tasks WHERE project_id=?`, e.p.ID); n != 0 {
			t.Fatalf("%d live tasks", n)
		}
		e.verifyLatest(t)
	}
	// staged after the delete: still unchanged by hash, same outcome
	e := newEnv(t)
	id1, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "w", "W", "hw", [3]string{"a", "A", "ha"})})
	e.apply(t, id1, nil)
	if err := e.del(t, "workflow", e.byRef(t, sys, "w"), true); err != nil {
		t.Fatal(err)
	}
	id2, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "w", "W", "hw", [3]string{"a", "A", "ha"}, [3]string{"b", "B", "hb"})})
	e.apply(t, id2, nil)
	if n := e.orphanTasks(t); n != 0 || e.liveTasks(t) != 0 {
		t.Fatalf("orphans=%d live=%d", n, e.liveTasks(t))
	}
}

// The same race for real: the workflow is deleted while the job applies.
func TestImportApplyRacingWorkflowDelete(t *testing.T) {
	const sys = "legacy"
	e := newEnv(t)
	for i := 0; i < 20; i++ {
		ref := fmt.Sprintf("w%d", i)
		id1, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, ref, "W", "h"+ref, [3]string{ref + "a", "A", "ha"})})
		e.apply(t, id1, nil)
		w := e.byRef(t, sys, ref)
		id2, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, ref, "W", "h"+ref, [3]string{ref + "a", "A", "ha"}, [3]string{ref + "b", "B", "hb"}, [3]string{ref + "c", "C", "hc"})})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.st.ApplyImport(e.ctx, e.p, e.actor("delivery", "admin"), id2, ImportApplyOpts{})
		}()
		go func() {
			defer wg.Done()
			_ = e.del(t, "workflow", w, true)
		}()
		wg.Wait()
		if n := e.orphanTasks(t); n != 0 {
			t.Fatalf("iteration %d: %d live tasks under a deleted workflow", i, n)
		}
	}
	e.verifyLatest(t)
}

// INV-30 and rollback: rolling back a job that created a workflow deletes it without a cascade, so a task someone
// else put under it blocks the rollback (ROLLBACK_BLOCKED names it) instead of ending up under a deleted workflow.
func TestImportRollbackBlockedByForeignTaskUnderCreatedWorkflow(t *testing.T) {
	const sys = "legacy"
	e := newEnv(t)
	id, _ := e.stage(t, sys, []json.RawMessage{wfRow(sys, "w", "W", "hw", [3]string{"a", "A", "ha"})})
	e.apply(t, id, nil)
	w := e.byRef(t, sys, "w")
	mine := e.task(t, w, "added by a person")
	_, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{})
	coreErr(t, err, core.StatusFailedPrecondition, "ROLLBACK_BLOCKED")
	if want := e.p.taskName(mine); !contains(err.Error(), want) {
		t.Fatalf("error %q does not name %s", err, want)
	}
	if e.liveWorkflows(t) != 1 || e.liveTasks(t) != 2 || e.orphanTasks(t) != 0 {
		t.Fatalf("a refused rollback changed data: workflows=%d tasks=%d", e.liveWorkflows(t), e.liveTasks(t))
	}
	// once the person's task is gone, the rollback goes through
	if err := e.del(t, "task", mine, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if e.liveWorkflows(t) != 0 || e.liveTasks(t) != 0 || e.orphanTasks(t) != 0 {
		t.Fatalf("after rollback: workflows=%d tasks=%d", e.liveWorkflows(t), e.liveTasks(t))
	}
}

// INV-30: `:assign` racing the delete of the task's old workflow. The cascade selects tasks without a lock; a task
// that moved out meanwhile must be left alone (it lives in the live target), and the rest must be cascaded.
func TestAssignRacingWorkflowDelete(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		a, b := e.createWF(t, fmt.Sprintf("A%d", i)), e.createWF(t, fmt.Sprintf("B%d", i))
		tasks := []int64{e.task(t, a, "t1"), e.task(t, a, "t2"), e.task(t, a, "t3")}
		var wg sync.WaitGroup
		for _, tk := range tasks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: tk, Actor: e.actor("delivery", "editor"),
					Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
						return []PlannedChange{{Key: "workflow", New: core.JSON(b)}}, nil
					}})
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.del(t, "workflow", a, true)
		}()
		wg.Wait()
		for _, tk := range tasks {
			en, err := loadEntity(e.ctx, e.st.DB, e.p.ID, "task", tk)
			if err != nil {
				t.Fatal(err)
			}
			wf := core.Scalar(en.Vals["workflow"])
			switch wf {
			case strconv.FormatInt(b, 10):
				if en.deleted() || en.Vals[cascadeKey] != nil {
					t.Fatalf("iteration %d: task %d moved to live B but deleted=%v cascade=%s", i, tk, en.deleted(), en.Vals[cascadeKey])
				}
			case strconv.FormatInt(a, 10):
				if !en.deleted() || en.Vals[cascadeKey] == nil {
					t.Fatalf("iteration %d: task %d stayed in deleted A but deleted=%v cascade=%s", i, tk, en.deleted(), en.Vals[cascadeKey])
				}
			default:
				t.Fatalf("task %d in workflow %s", tk, wf)
			}
		}
		// A's undelete restores exactly the tasks that stayed
		if err := e.del(t, "workflow", a, false); err != nil {
			t.Fatal(err)
		}
		if n := e.orphanTasks(t); n != 0 {
			t.Fatalf("%d orphan tasks", n)
		}
	}
	e.verifyLatest(t)
}

// A cascade marker means "restore with the workflow" and only on a deleted task: restoring the task on its own, or
// any other write to a live one, drops a stale marker.
func TestStaleCascadeMarkerIsDropped(t *testing.T) {
	e := newEnv(t)
	w := e.createWF(t, "w")
	tk := e.task(t, w, "t")
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: tk, Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "deleted", New: core.JSON(true)}, {Key: cascadeKey, New: core.JSON(99)}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	if err := e.del(t, "task", tk, false); err != nil {
		t.Fatal(err)
	}
	if d, c, _ := e.taskState(t, tk); d || c != "" {
		t.Fatalf("after a manual undelete: deleted=%v cascade=%q", d, c)
	}
}
