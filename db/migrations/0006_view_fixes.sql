-- openetl 0006: public view fixes (additive: CREATE OR REPLACE VIEW only; 0001-0005 are frozen).
--  * workflow_stages, open_items and comments no longer expose rows of soft-deleted workflows: they join v_workflow_base, the
--    same liveness as `workflows`. The internal v_stage, v_current_stage, v_open_items and v_comment stay unfiltered on
--    purpose: the server reads them to render a soft-deleted workflow (show_deleted); only the public layer hides it.
--    Tasks are separate entities: a task stays listed when one of its workflows is deleted (its link is kept).
--  * open_items has a row only for a workflow with at least one open issue, risk or question (notes alone give none).
--  * tasks.primary_workflow_id is the lowest workflow id among the task's primary links (deterministic).
--  * every string column a view builds from a literal (code, scope, health, stage, status) carries an explicit collation, so
--    comparing it with a literal never depends on the collation of the querying connection (error 1267 otherwise).
--    utf8mb4_unicode_ci exists on MySQL 8.0 and MariaDB and compares like the former default for these ASCII values.
-- Column lists are unchanged; the public views are recreated because MySQL freezes the column list of SELECT * views.

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

CREATE OR REPLACE VIEW v_open_items AS  -- unresolved issues / risks / questions, one row per workflow that has any
SELECT workflow_id,
  SUM(kind = 'issue') AS open_issues,
  SUM(kind = 'risk') AS open_risks,
  SUM(kind = 'question') AS open_questions
FROM v_comment WHERE NOT resolved AND NOT deleted GROUP BY workflow_id
HAVING SUM(kind IN ('issue', 'risk', 'question')) > 0;

CREATE OR REPLACE VIEW v_open_items_p AS
SELECT o.workflow_id, w.project_id, o.open_issues, o.open_risks, o.open_questions
FROM v_open_items o JOIN v_workflow_base w ON w.id = o.workflow_id;

CREATE OR REPLACE VIEW v_stage_p AS
SELECT s.workflow_id, w.project_id, s.stage_key, s.seq, s.status COLLATE utf8mb4_unicode_ci AS status
FROM v_stage s JOIN v_workflow_base w ON w.id = s.workflow_id;

CREATE OR REPLACE VIEW v_comment_p AS
SELECT c.id, c.project_id, c.workflow_id, c.kind, c.body, c.resolved, c.deleted
FROM v_comment c JOIN v_workflow_base w ON w.id = c.workflow_id;

CREATE OR REPLACE VIEW v_workflow AS
SELECT w.id, CONCAT('WF-', LPAD(w.id, 6, '0')) COLLATE utf8mb4_unicode_ci AS code, w.project_id, w.name,
  w.scope COLLATE utf8mb4_unicode_ci AS scope,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 6), l.v) FROM entity_latest l
    WHERE l.entity_id = w.id AND l.k LIKE 'attr.%' AND l.v IS NOT NULL) AS attrs,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 10), l.v) FROM entity_latest l
    WHERE l.entity_id = w.id AND l.k LIKE 'assignee.%' AND l.v IS NOT NULL) AS assignees,
  mt.merged_into_id, en.create_time,
  cs.current_stage,
  COALESCE(o.open_issues, 0) AS open_issues,
  COALESCE(o.open_risks, 0) AS open_risks,
  (CASE WHEN COALESCE(o.open_issues, 0) > 0 THEN 'blocked'
        WHEN COALESCE(o.open_risks, 0) > 0 THEN 'at_risk'
        ELSE 'ok' END) COLLATE utf8mb4_unicode_ci AS health
FROM v_workflow_base w
JOIN entity en ON en.id = w.id
LEFT JOIN v_merge_target mt ON mt.entity_id = w.id AND w.scope = 'out'
LEFT JOIN v_current_stage cs ON cs.workflow_id = w.id
LEFT JOIN v_open_items o ON o.workflow_id = w.id;

CREATE OR REPLACE VIEW v_stage_distribution AS
SELECT project_id, COALESCE(current_stage, 'done') COLLATE utf8mb4_unicode_ci AS stage, health, COUNT(*) AS workflows
FROM v_workflow
WHERE scope = 'in'
GROUP BY project_id, COALESCE(current_stage, 'done') COLLATE utf8mb4_unicode_ci, health;

CREATE OR REPLACE VIEW v_task AS
SELECT t.id, CONCAT('TK-', LPAD(t.id, 6, '0')) COLLATE utf8mb4_unicode_ci AS code, t.project_id, JSON_UNQUOTE(ln.v) AS name,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 6), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'attr.%' AND l.v IS NOT NULL) AS attrs,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 10), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'assignee.%' AND l.v IS NOT NULL) AS assignees,
  t.create_time,
  (SELECT MIN(CAST(SUBSTRING(l.k, 4) AS UNSIGNED)) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'in.%' AND JSON_UNQUOTE(l.v) = 'primary') AS primary_workflow_id
FROM entity t
LEFT JOIN entity_latest ln ON ln.entity_id = t.id AND ln.k = 'name'
LEFT JOIN entity_latest ld ON ld.entity_id = t.id AND ld.k = 'deleted'
WHERE t.type = 'task'
  AND COALESCE(JSON_UNQUOTE(ld.v), 'false') <> 'true'
  AND EXISTS (SELECT 1 FROM entity_latest x WHERE x.entity_id = t.id);

CREATE OR REPLACE VIEW workflows AS          SELECT * FROM v_workflow;
CREATE OR REPLACE VIEW tasks AS              SELECT * FROM v_task;
CREATE OR REPLACE VIEW events AS             SELECT * FROM v_event;
CREATE OR REPLACE VIEW comments AS           SELECT * FROM v_comment_p;
CREATE OR REPLACE VIEW workflow_stages AS    SELECT * FROM v_stage_p;
CREATE OR REPLACE VIEW stage_distribution AS SELECT * FROM v_stage_distribution;
CREATE OR REPLACE VIEW open_items AS         SELECT * FROM v_open_items_p;
