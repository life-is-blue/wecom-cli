package store

import (
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

func (e *env) taskState(t *testing.T, id int64) (deleted bool, cascade string, events int) {
	t.Helper()
	en, err := loadEntity(e.ctx, e.st.DB, e.p.ID, "task", id)
	if err != nil {
		t.Fatal(err)
	}
	return en.deleted(), core.Scalar(en.Vals[cascadeKey]), e.count(t, `SELECT COUNT(*) FROM event WHERE entity_id = ?`, id)
}

// INV-30: deleting a workflow soft-deletes its live tasks in the same transaction (one event each, noting the
// workflow's delete event); undeleting it restores exactly those tasks, not the ones deleted on their own.
func TestWorkflowDeleteCascadesToTasks(t *testing.T) {
	e := newEnv(t)
	w, other := e.createWF(t, "w"), e.createWF(t, "other")
	t1, t2, t3, keep := e.task(t, w, "t1"), e.task(t, w, "t2"), e.task(t, w, "own"), e.task(t, other, "keep")
	if err := e.del(t, "task", t3, true); err != nil { // deleted on its own, before the workflow
		t.Fatal(err)
	}
	_, _, n3 := e.taskState(t, t3)

	// validate_only runs the cascade and rolls it back
	delRes, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: w, Actor: e.actor("delivery", "editor"), ValidateOnly: true,
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "deleted", New: core.JSON(true)}}, nil
		}})
	if err != nil || delRes.EventID != 0 {
		t.Fatalf("validate_only: %+v %v", delRes, err)
	}
	if d, _, ev := e.taskState(t, t1); d || ev != 1 {
		t.Fatalf("validate_only touched t1: deleted=%v events=%d", d, ev)
	}

	if err := e.del(t, "workflow", w, true); err != nil {
		t.Fatal(err)
	}
	var wfEv int64
	e.st.DB.QueryRow(`SELECT MAX(id) FROM event WHERE entity_id = ?`, w).Scan(&wfEv)
	for _, id := range []int64{t1, t2} {
		d, c, ev := e.taskState(t, id)
		if !d || c == "" || ev != 2 {
			t.Fatalf("task %d after the workflow delete: deleted=%v cascade=%q events=%d", id, d, c, ev)
		}
		var note string
		e.st.DB.QueryRow(`SELECT note FROM event WHERE entity_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&note)
		if !strings.Contains(note, "cascade") || !strings.Contains(note, FormatID(PrefixWorkflow, w)) || !strings.Contains(note, "ev-"+c) || c != core.Scalar(core.JSON(wfEv)) {
			t.Fatalf("note %q cascade %q workflow event %d", note, c, wfEv)
		}
	}
	if d, c, ev := e.taskState(t, t3); !d || c != "" || ev != n3 {
		t.Fatalf("a task deleted earlier is left alone: deleted=%v cascade=%q events=%d (was %d)", d, c, ev, n3)
	}
	if d, _, ev := e.taskState(t, keep); d || ev != 1 {
		t.Fatalf("a task of another workflow was touched")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM tasks WHERE workflow_id = ?`, w); n != 0 {
		t.Fatalf("tasks view lists %d tasks of a deleted workflow", n)
	}

	// a task cannot be restored, or moved there, while its workflow is deleted
	coreErr(t, e.del(t, "task", t1, false), core.StatusFailedPrecondition, "PARENT_DELETED")
	coreErr(t, e.del(t, "task", t3, false), core.StatusFailedPrecondition, "PARENT_DELETED")
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "task", EntityID: keep, Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "workflow", New: core.JSON(w)}}, nil
		}})
	coreErr(t, err, core.StatusFailedPrecondition, "ENTITY_DELETED")

	if err := e.del(t, "workflow", w, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{t1, t2} {
		d, c, ev := e.taskState(t, id)
		if d || c != "" || ev != 3 {
			t.Fatalf("task %d after the workflow undelete: deleted=%v cascade=%q events=%d", id, d, c, ev)
		}
	}
	if d, _, ev := e.taskState(t, t3); !d || ev != n3 {
		t.Fatalf("a task deleted on its own must stay deleted: deleted=%v events=%d", d, ev)
	}
	if err := e.del(t, "task", t3, false); err != nil { // now its workflow is live: restorable
		t.Fatal(err)
	}
	// a second delete/undelete round cascades again
	if err := e.del(t, "workflow", w, true); err != nil {
		t.Fatal(err)
	}
	if d, c, _ := e.taskState(t, t1); !d || c == "" {
		t.Fatalf("second delete did not cascade: deleted=%v cascade=%q", d, c)
	}
	e.verifyLatest(t)
}
