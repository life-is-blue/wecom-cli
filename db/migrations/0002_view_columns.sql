-- openetl 0002: columns the gateway/API needs on the public views (0001 is never edited).
--  * v_attrs ignores removed (SQL NULL) attributes
--  * v_assignees: assignee.* folded into one JSON object
--  * workflows/tasks gain create_time, assignees (+ merged_into_id on workflows)
--  * workflow_stages and open_items gain project_id so the query endpoint can isolate projects
-- Public views are recreated because MySQL freezes the column list of SELECT * views.

SET NAMES utf8mb4;

CREATE OR REPLACE VIEW v_attrs AS
SELECT entity_id, JSON_OBJECTAGG(SUBSTRING(k, 6), v) AS attrs
FROM v_latest WHERE k LIKE 'attr.%' AND v IS NOT NULL GROUP BY entity_id;

CREATE OR REPLACE VIEW v_assignees AS
SELECT entity_id, JSON_OBJECTAGG(SUBSTRING(k, 10), v) AS assignees
FROM v_latest WHERE k LIKE 'assignee.%' AND v IS NOT NULL GROUP BY entity_id;

CREATE OR REPLACE VIEW v_merge_target AS
SELECT entity_id, CAST(JSON_UNQUOTE(v) AS UNSIGNED) AS merged_into_id
FROM v_latest WHERE k = 'into' AND v IS NOT NULL;

CREATE OR REPLACE VIEW v_workflow AS
SELECT w.id, CONCAT('WF-', LPAD(w.id, 6, '0')) AS code, w.project_id, w.name, w.scope, a.attrs,
  asg.assignees, mt.merged_into_id, en.create_time,
  cs.current_stage,
  COALESCE(o.open_issues, 0) AS open_issues,
  COALESCE(o.open_risks, 0) AS open_risks,
  CASE WHEN COALESCE(o.open_issues, 0) > 0 THEN 'blocked'
       WHEN COALESCE(o.open_risks, 0) > 0 THEN 'at_risk'
       ELSE 'ok' END AS health
FROM v_workflow_base w
JOIN entity en ON en.id = w.id
LEFT JOIN v_attrs a ON a.entity_id = w.id
LEFT JOIN v_assignees asg ON asg.entity_id = w.id
LEFT JOIN v_merge_target mt ON mt.entity_id = w.id AND w.scope = 'out'
LEFT JOIN v_current_stage cs ON cs.workflow_id = w.id
LEFT JOIN v_open_items o ON o.workflow_id = w.id;

CREATE OR REPLACE VIEW v_task AS
SELECT t.id, CONCAT('TK-', LPAD(t.id, 6, '0')) AS code, t.project_id, t.name, a.attrs,
  asg.assignees, en.create_time,
  (SELECT CAST(SUBSTRING(l.k, 4) AS UNSIGNED) FROM v_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'in.%' AND JSON_UNQUOTE(l.v) = 'primary' LIMIT 1) AS primary_workflow_id
FROM (SELECT e.id, e.project_id, MAX(CASE WHEN l.k = 'name' THEN JSON_UNQUOTE(l.v) END) AS name
        FROM entity e JOIN v_latest l ON l.entity_id = e.id
       WHERE e.type = 'task' GROUP BY e.id, e.project_id
      HAVING COALESCE(MAX(CASE WHEN l.k = 'deleted' THEN JSON_UNQUOTE(l.v) END), 'false') <> 'true') t
JOIN entity en ON en.id = t.id
LEFT JOIN v_attrs a ON a.entity_id = t.id
LEFT JOIN v_assignees asg ON asg.entity_id = t.id;

CREATE OR REPLACE VIEW v_stage_p AS
SELECT s.workflow_id, e.project_id, s.stage_key, s.seq, s.status
FROM v_stage s JOIN entity e ON e.id = s.workflow_id;

CREATE OR REPLACE VIEW v_open_items_p AS
SELECT o.workflow_id, e.project_id, o.open_issues, o.open_risks, o.open_questions
FROM v_open_items o JOIN entity e ON e.id = o.workflow_id;

CREATE OR REPLACE VIEW workflows AS          SELECT * FROM v_workflow;
CREATE OR REPLACE VIEW tasks AS              SELECT * FROM v_task;
CREATE OR REPLACE VIEW workflow_stages AS    SELECT * FROM v_stage_p;
CREATE OR REPLACE VIEW open_items AS         SELECT * FROM v_open_items_p;
