-- openetl 0004: entity_latest, a rebuildable cache of "latest value per (entity, key)".
--  * delta stays the only source of truth. entity_latest is derived from it and is maintained by the
--    gateway inside the same transaction that inserts deltas (upsert only when the event_id is greater).
--  * `openetl-gateway admin rebuild-latest` rebuilds it from delta, `admin verify-latest` checks it.
--  * v_latest becomes a plain projection of the cache, so every view that joins v_latest goes
--    through the primary key (entity_id, k) instead of re-materialising a GROUP BY over delta per join.
--  * A removed key keeps its row with v = NULL (same as the old v_latest, which returned new_v NULL).
-- Written for MySQL 8.0 and MariaDB 10.6+. (A grant that is INSERT+SELECT only on delta must also
-- allow INSERT, UPDATE, DELETE, SELECT on entity_latest.)

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS entity_latest (
  entity_id BIGINT UNSIGNED NOT NULL,
  k VARCHAR(96) NOT NULL,
  v JSON NULL,
  event_id BIGINT UNSIGNED NOT NULL,       -- the event that set v (max event_id of the key in delta)
  PRIMARY KEY (entity_id, k),
  KEY idx_k (k, entity_id)                 -- views and lookups that start from a key ('on', 'storage_key', ...)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- one-time backfill from the delta log
DELETE FROM entity_latest;

INSERT INTO entity_latest (entity_id, k, v, event_id)
SELECT d.entity_id, d.k, d.new_v, d.event_id
FROM delta d
JOIN (SELECT entity_id, k, MAX(event_id) AS event_id FROM delta GROUP BY entity_id, k) m
  ON m.entity_id = d.entity_id AND m.k = d.k AND m.event_id = d.event_id;

CREATE OR REPLACE VIEW v_latest AS      -- latest value of every key per entity (cache table)
SELECT entity_id, k, v, event_id FROM entity_latest;

-- ---------------------------------------------------------------- views rewritten without GROUP BY
-- Same columns and semantics as 0001/0002. Instead of pivoting v_latest with GROUP BY + MAX(CASE ..)
-- (a derived table that cannot be filtered by project/id and is re-built for every join), each key is
-- fetched with a LEFT JOIN on the primary key (entity_id, k). The views stay mergeable, so a query for
-- one workflow, or a project, only touches those rows. An entity without any delta stays invisible.

CREATE OR REPLACE VIEW v_workflow_base AS
SELECT e.id, e.project_id,
  JSON_UNQUOTE(ln.v) AS name,
  COALESCE(JSON_UNQUOTE(ls.v), 'in') AS scope
FROM entity e
LEFT JOIN entity_latest ln ON ln.entity_id = e.id AND ln.k = 'name'
LEFT JOIN entity_latest ls ON ls.entity_id = e.id AND ls.k = 'scope'
LEFT JOIN entity_latest ld ON ld.entity_id = e.id AND ld.k = 'deleted'
WHERE e.type = 'workflow'
  AND COALESCE(JSON_UNQUOTE(ld.v), 'false') <> 'true'
  AND EXISTS (SELECT 1 FROM entity_latest x WHERE x.entity_id = e.id);

CREATE OR REPLACE VIEW v_current_stage AS   -- first stage that is neither pass nor na
SELECT e.id AS workflow_id,
  (SELECT d.stage_key FROM stage_def d
     LEFT JOIN entity_latest l ON l.entity_id = e.id AND l.k = CONCAT('stage.', d.stage_key)
    WHERE COALESCE(JSON_UNQUOTE(l.v), 'open') NOT IN ('pass', 'na')
    ORDER BY d.seq LIMIT 1) AS current_stage
FROM entity e
WHERE e.type = 'workflow'
  AND EXISTS (SELECT 1 FROM stage_def d
                LEFT JOIN entity_latest l ON l.entity_id = e.id AND l.k = CONCAT('stage.', d.stage_key)
               WHERE COALESCE(JSON_UNQUOTE(l.v), 'open') NOT IN ('pass', 'na'));

CREATE OR REPLACE VIEW v_comment AS
SELECT e.id, e.project_id,
  CAST(JSON_UNQUOTE(lo.v) AS UNSIGNED) AS workflow_id,
  JSON_UNQUOTE(lk.v) AS kind,
  JSON_UNQUOTE(lb.v) AS body,
  COALESCE(JSON_UNQUOTE(lr.v), 'false') = 'true' AS resolved,
  COALESCE(JSON_UNQUOTE(ld.v), 'false') = 'true' AS deleted
FROM entity e
LEFT JOIN entity_latest lo ON lo.entity_id = e.id AND lo.k = 'on'
LEFT JOIN entity_latest lk ON lk.entity_id = e.id AND lk.k = 'kind'
LEFT JOIN entity_latest lb ON lb.entity_id = e.id AND lb.k = 'body'
LEFT JOIN entity_latest lr ON lr.entity_id = e.id AND lr.k = 'resolved'
LEFT JOIN entity_latest ld ON ld.entity_id = e.id AND ld.k = 'deleted'
WHERE e.type = 'comment'
  AND EXISTS (SELECT 1 FROM entity_latest x WHERE x.entity_id = e.id);

CREATE OR REPLACE VIEW v_task AS
SELECT t.id, CONCAT('TK-', LPAD(t.id, 6, '0')) AS code, t.project_id, JSON_UNQUOTE(ln.v) AS name,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 6), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'attr.%' AND l.v IS NOT NULL) AS attrs,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 10), l.v) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'assignee.%' AND l.v IS NOT NULL) AS assignees,
  t.create_time,
  (SELECT CAST(SUBSTRING(l.k, 4) AS UNSIGNED) FROM entity_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'in.%' AND JSON_UNQUOTE(l.v) = 'primary' LIMIT 1) AS primary_workflow_id
FROM entity t
LEFT JOIN entity_latest ln ON ln.entity_id = t.id AND ln.k = 'name'
LEFT JOIN entity_latest ld ON ld.entity_id = t.id AND ld.k = 'deleted'
WHERE t.type = 'task'
  AND COALESCE(JSON_UNQUOTE(ld.v), 'false') <> 'true'
  AND EXISTS (SELECT 1 FROM entity_latest x WHERE x.entity_id = t.id);

CREATE OR REPLACE VIEW v_workflow AS
SELECT w.id, CONCAT('WF-', LPAD(w.id, 6, '0')) AS code, w.project_id, w.name, w.scope,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 6), l.v) FROM entity_latest l
    WHERE l.entity_id = w.id AND l.k LIKE 'attr.%' AND l.v IS NOT NULL) AS attrs,
  (SELECT JSON_OBJECTAGG(SUBSTRING(l.k, 10), l.v) FROM entity_latest l
    WHERE l.entity_id = w.id AND l.k LIKE 'assignee.%' AND l.v IS NOT NULL) AS assignees,
  mt.merged_into_id, en.create_time,
  cs.current_stage,
  COALESCE(o.open_issues, 0) AS open_issues,
  COALESCE(o.open_risks, 0) AS open_risks,
  CASE WHEN COALESCE(o.open_issues, 0) > 0 THEN 'blocked'
       WHEN COALESCE(o.open_risks, 0) > 0 THEN 'at_risk'
       ELSE 'ok' END AS health
FROM v_workflow_base w
JOIN entity en ON en.id = w.id
LEFT JOIN v_merge_target mt ON mt.entity_id = w.id AND w.scope = 'out'
LEFT JOIN v_current_stage cs ON cs.workflow_id = w.id
LEFT JOIN v_open_items o ON o.workflow_id = w.id;
