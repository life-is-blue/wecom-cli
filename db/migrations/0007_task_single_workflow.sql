-- openetl 0007: a task belongs to exactly one workflow (additive: CREATE OR REPLACE VIEW only; 0001-0006 are frozen).
--  * The task keeps one key, `workflow` (the workflow's entity id), instead of the multi-link `in.<workflow>` keys with a
--    primary flag. tasks.primary_workflow_id becomes tasks.workflow_id and reads that key.
--  * Existing databases created before this migration hold tasks under the old keys: they have no `workflow` key, so
--    tasks.workflow_id is NULL for them. There is no production data (pre-release); recreate such databases.
--  * Deleting a workflow soft-deletes its live tasks in the same transaction (the server writes the events), so the
--    public `tasks` view needs no join with the workflow.
-- Column list: the last column is renamed, everything else is unchanged. The public view is recreated because MySQL
-- freezes the column list of SELECT * views.

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

CREATE OR REPLACE VIEW v_task AS
SELECT t.id, CONCAT('TK-', LPAD(t.id, 6, '0')) COLLATE utf8mb4_unicode_ci AS code, t.project_id, JSON_UNQUOTE(ln.v) AS name,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 6), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'attr.%' AND l.v IS NOT NULL) AS attrs,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 10), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'assignee.%' AND l.v IS NOT NULL) AS assignees,
  t.create_time,
  CAST(JSON_UNQUOTE(lw.v) AS UNSIGNED) AS workflow_id
FROM entity t
LEFT JOIN entity_latest ln ON ln.entity_id = t.id AND ln.k = 'name'
LEFT JOIN entity_latest lw ON lw.entity_id = t.id AND lw.k = 'workflow'
LEFT JOIN entity_latest ld ON ld.entity_id = t.id AND ld.k = 'deleted'
WHERE t.type = 'task'
  AND COALESCE(JSON_UNQUOTE(ld.v), 'false') <> 'true'
  AND EXISTS (SELECT 1 FROM entity_latest x WHERE x.entity_id = t.id);

CREATE OR REPLACE VIEW tasks AS SELECT * FROM v_task;
