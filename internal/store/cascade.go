package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// cascadeKey marks a task that was soft-deleted because its workflow was: its value is the id of the
// workflow's delete event. The workflow's undelete restores exactly the tasks that carry it (INV-30).
const cascadeKey = "cascade"

// cascadeTasks runs inside the transaction of a workflow delete (del) or undelete: one event per
// task, written as the same actor, with a note that names the workflow event. Deleting takes the
// workflow's live tasks along; undeleting restores the tasks that carry cascadeKey and only those
// (a task the user deleted on its own stays deleted).
func (s *Store) cascadeTasks(ctx context.Context, tx *sql.Tx, req ChangeRequest, wf, wfEvent int64, eventTime time.Time, del bool) error {
	q := `SELECT e.id FROM entity e
		JOIN entity_latest lw ON lw.entity_id = e.id AND lw.k = 'workflow' AND JSON_UNQUOTE(lw.v) = ?
		LEFT JOIN entity_latest lc ON lc.entity_id = e.id AND lc.k = ?
		LEFT JOIN entity_latest ld ON ld.entity_id = e.id AND ld.k = 'deleted'
		WHERE e.project_id = ? AND e.type = 'task' AND `
	if del {
		q += `COALESCE(JSON_UNQUOTE(ld.v), 'false') <> 'true'`
	} else {
		q += `lc.v IS NOT NULL`
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY e.id`, strconv.FormatInt(wf, 10), cascadeKey, req.ProjectID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	verb := "deleted"
	if !del {
		verb = "restored"
	}
	note := fmt.Sprintf("cascade: task %s with workflow %s (delete event ev-%d)", verb, FormatID(PrefixWorkflow, wf), wfEvent)
	if !del {
		note = fmt.Sprintf("cascade: task %s with workflow %s (undelete of its delete event)", verb, FormatID(PrefixWorkflow, wf))
	}
	for _, id := range ids {
		_, err := s.applyTx(ctx, tx, ChangeRequest{
			ProjectID: req.ProjectID, EntityType: "task", EntityID: id, Actor: req.Actor, Note: note, EventTime: req.EventTime,
			Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
				if del {
					return []PlannedChange{{Key: "deleted", New: core.JSON(true)}, {Key: cascadeKey, New: core.JSON(wfEvent)}}, nil
				}
				return []PlannedChange{{Key: "deleted", New: core.JSON(false)}, {Key: cascadeKey}}, nil
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}
