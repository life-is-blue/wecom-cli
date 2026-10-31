-- openetl M0 migration 0001_init  (MySQL 8.0, utf8mb4, all times UTC)
-- Order: tables -> internal helper views (v_*) -> public query-surface views -> seed data.
-- Business data lives only in entity/event/delta (append-only; grant app account INSERT+SELECT only).
-- Public views: workflows, tasks, events, comments, workflow_stages, stage_distribution, open_items.
-- No secrets here; api_token stores hashes only, setting stores secret references only.

SET NAMES utf8mb4;

-- ---------------------------------------------------------------- project / auth
CREATE TABLE IF NOT EXISTS project (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS account (
  user_name VARCHAR(64) PRIMARY KEY,
  display_name VARCHAR(128) NULL,
  kind ENUM('human','agent') NOT NULL DEFAULT 'human',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS api_token (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  user_name VARCHAR(64) NOT NULL,
  token_hash CHAR(64) NOT NULL UNIQUE,   -- hash only; plaintext shown once at creation
  expires_at DATETIME(6) NOT NULL,
  revoked_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  FOREIGN KEY (user_name) REFERENCES account(user_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS member (
  project_id BIGINT UNSIGNED NOT NULL,
  user_name VARCHAR(64) NOT NULL,
  role ENUM('viewer','editor','admin') NOT NULL,
  side ENUM('delivery','customer') NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (project_id, user_name),
  FOREIGN KEY (project_id) REFERENCES project(id),
  FOREIGN KEY (user_name) REFERENCES account(user_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------- entity / event / delta
CREATE TABLE IF NOT EXISTS entity (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  project_id BIGINT UNSIGNED NOT NULL,
  type ENUM('workflow','task','comment','attachment') NOT NULL,
  create_time DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  KEY idx_type (project_id, type, id),
  FOREIGN KEY (project_id) REFERENCES project(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS event (      -- one write = one event on one entity
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  project_id BIGINT UNSIGNED NOT NULL,
  entity_id BIGINT UNSIGNED NOT NULL,
  actor VARCHAR(64) NOT NULL,           -- account name
  side ENUM('delivery','customer','system') NOT NULL,
  channel VARCHAR(16) NOT NULL,         -- cli|api|import|legacy
  reason_key VARCHAR(48) NULL,          -- from reason table
  note TEXT NULL,
  idempotency_key VARCHAR(64) NULL,
  corrects_event_id BIGINT UNSIGNED NULL,
  event_time DATETIME(6) NOT NULL,      -- when it happened (business time)
  create_time DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),   -- when recorded
  UNIQUE KEY uq_idem (project_id, idempotency_key),
  KEY idx_entity (entity_id, id),
  KEY idx_time (project_id, create_time),
  FOREIGN KEY (entity_id) REFERENCES entity(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS delta (      -- one key change inside one event
  event_id BIGINT UNSIGNED NOT NULL,
  entity_id BIGINT UNSIGNED NOT NULL,   -- redundant, for latest-value lookup
  k VARCHAR(96) NOT NULL,
  old_v JSON NULL,
  new_v JSON NULL,
  PRIMARY KEY (event_id, k),
  KEY idx_latest (entity_id, k, event_id),
  FOREIGN KEY (event_id) REFERENCES event(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------- config
CREATE TABLE IF NOT EXISTS stage_def (
  stage_key VARCHAR(32) PRIMARY KEY,
  seq TINYINT UNSIGNED NOT NULL,
  name_zh VARCHAR(64) NOT NULL,
  name_en VARCHAR(64) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS reason (
  reason_key VARCHAR(48) PRIMARY KEY,
  kind ENUM('gate_override','discard','not_applicable','reject') NOT NULL,
  owner_side ENUM('delivery','customer','unknown') NOT NULL,
  name_zh VARCHAR(64) NOT NULL,
  name_en VARCHAR(64) NOT NULL,
  note_required TINYINT(1) NOT NULL DEFAULT 0,
  active TINYINT(1) NOT NULL DEFAULT 1
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS rule (
  id SMALLINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  k_pattern VARCHAR(96) NOT NULL,       -- LIKE pattern on key, e.g. stage.%
  to_value VARCHAR(64) NULL,            -- NULL = any value
  side ENUM('delivery','customer','any') NOT NULL,
  reason_kind ENUM('gate_override','discard','not_applicable','reject') NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS setting (    -- config only; secrets by reference
  k VARCHAR(128) PRIMARY KEY,
  v TEXT NOT NULL,
  updated_by VARCHAR(64) NOT NULL,
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------- import
CREATE TABLE IF NOT EXISTS import_batch (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  project_id BIGINT UNSIGNED NOT NULL,
  source VARCHAR(64) NOT NULL,
  state ENUM('staged','applied','failed','rolled_back') NOT NULL DEFAULT 'staged',
  created_by VARCHAR(64) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  applied_at DATETIME(6) NULL,
  summary JSON NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS import_row (
  batch_id BIGINT UNSIGNED NOT NULL,
  row_no INT UNSIGNED NOT NULL,
  source_ref VARCHAR(255) NULL,
  row_hash CHAR(64) NOT NULL,
  row_json JSON NOT NULL,
  match_entity_id BIGINT UNSIGNED NULL,
  action ENUM('new','changed','unchanged','missing','conflict','suspect') NULL,
  PRIMARY KEY (batch_id, row_no),
  FOREIGN KEY (batch_id) REFERENCES import_batch(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------------------------------------------- internal helper views
CREATE OR REPLACE VIEW v_latest AS      -- latest value of every key per entity
SELECT d.entity_id, d.k, d.new_v AS v, d.event_id
FROM delta d
JOIN (SELECT entity_id, k, MAX(event_id) AS event_id FROM delta GROUP BY entity_id, k) m
  ON m.entity_id = d.entity_id AND m.k = d.k AND m.event_id = d.event_id;

CREATE OR REPLACE VIEW v_attrs AS       -- attr.* folded into one JSON object
SELECT entity_id, JSON_OBJECTAGG(SUBSTRING(k, 6), v) AS attrs
FROM v_latest WHERE k LIKE 'attr.%' GROUP BY entity_id;

CREATE OR REPLACE VIEW v_workflow_base AS
SELECT e.id, e.project_id,
  MAX(CASE WHEN l.k = 'name' THEN JSON_UNQUOTE(l.v) END) AS name,
  COALESCE(MAX(CASE WHEN l.k = 'scope' THEN JSON_UNQUOTE(l.v) END), 'in') AS scope
FROM entity e JOIN v_latest l ON l.entity_id = e.id
WHERE e.type = 'workflow'
GROUP BY e.id, e.project_id
HAVING COALESCE(MAX(CASE WHEN l.k = 'deleted' THEN JSON_UNQUOTE(l.v) END), 'false') <> 'true';

CREATE OR REPLACE VIEW v_stage AS       -- every stage of every workflow; no delta = open
SELECT e.id AS workflow_id, d.stage_key, d.seq,
       COALESCE(JSON_UNQUOTE(l.v), 'open') AS status
FROM entity e
CROSS JOIN stage_def d
LEFT JOIN v_latest l ON l.entity_id = e.id AND l.k = CONCAT('stage.', d.stage_key)
WHERE e.type = 'workflow';

CREATE OR REPLACE VIEW v_current_stage AS   -- first stage that is neither pass nor na
SELECT s.workflow_id, d.stage_key AS current_stage
FROM (SELECT workflow_id, MIN(seq) AS seq FROM v_stage
       WHERE status NOT IN ('pass', 'na') GROUP BY workflow_id) s
JOIN stage_def d ON d.seq = s.seq;

CREATE OR REPLACE VIEW v_comment AS
SELECT e.id, e.project_id,
  CAST(MAX(CASE WHEN l.k = 'on' THEN JSON_UNQUOTE(l.v) END) AS UNSIGNED) AS workflow_id,
  MAX(CASE WHEN l.k = 'kind' THEN JSON_UNQUOTE(l.v) END) AS kind,
  MAX(CASE WHEN l.k = 'body' THEN JSON_UNQUOTE(l.v) END) AS body,
  COALESCE(MAX(CASE WHEN l.k = 'resolved' THEN JSON_UNQUOTE(l.v) END), 'false') = 'true' AS resolved,
  COALESCE(MAX(CASE WHEN l.k = 'deleted'  THEN JSON_UNQUOTE(l.v) END), 'false') = 'true' AS deleted
FROM entity e JOIN v_latest l ON l.entity_id = e.id
WHERE e.type = 'comment'
GROUP BY e.id, e.project_id;

CREATE OR REPLACE VIEW v_open_items AS  -- unresolved issues / risks / questions
SELECT workflow_id,
  SUM(kind = 'issue') AS open_issues,
  SUM(kind = 'risk') AS open_risks,
  SUM(kind = 'question') AS open_questions
FROM v_comment WHERE NOT resolved AND NOT deleted GROUP BY workflow_id;

CREATE OR REPLACE VIEW v_workflow AS
SELECT w.id, CONCAT('WF-', LPAD(w.id, 6, '0')) AS code, w.project_id, w.name, w.scope, a.attrs,
  cs.current_stage,
  COALESCE(o.open_issues, 0) AS open_issues,
  COALESCE(o.open_risks, 0) AS open_risks,
  CASE WHEN COALESCE(o.open_issues, 0) > 0 THEN 'blocked'
       WHEN COALESCE(o.open_risks, 0) > 0 THEN 'at_risk'
       ELSE 'ok' END AS health
FROM v_workflow_base w
LEFT JOIN v_attrs a ON a.entity_id = w.id
LEFT JOIN v_current_stage cs ON cs.workflow_id = w.id
LEFT JOIN v_open_items o ON o.workflow_id = w.id;

CREATE OR REPLACE VIEW v_stage_distribution AS
SELECT project_id, COALESCE(current_stage, 'done') AS stage, health, COUNT(*) AS workflows
FROM v_workflow
WHERE scope = 'in'
GROUP BY project_id, COALESCE(current_stage, 'done'), health;

CREATE OR REPLACE VIEW v_task AS
SELECT t.id, CONCAT('TK-', LPAD(t.id, 6, '0')) AS code, t.project_id, t.name, a.attrs,
  (SELECT CAST(SUBSTRING(l.k, 4) AS UNSIGNED) FROM v_latest l
    WHERE l.entity_id = t.id AND l.k LIKE 'in.%' AND JSON_UNQUOTE(l.v) = 'primary' LIMIT 1) AS primary_workflow_id
FROM (SELECT e.id, e.project_id, MAX(CASE WHEN l.k = 'name' THEN JSON_UNQUOTE(l.v) END) AS name
        FROM entity e JOIN v_latest l ON l.entity_id = e.id
       WHERE e.type = 'task' GROUP BY e.id, e.project_id
      HAVING COALESCE(MAX(CASE WHEN l.k = 'deleted' THEN JSON_UNQUOTE(l.v) END), 'false') <> 'true') t
LEFT JOIN v_attrs a ON a.entity_id = t.id;

CREATE OR REPLACE VIEW v_event AS       -- audit: who changed what to what, when
SELECT ev.id AS event_id, ev.project_id, ev.entity_id, en.type, ev.actor, ev.side, ev.channel,
       ev.reason_key, ev.note, ev.corrects_event_id, ev.event_time, ev.create_time,
       d.k, d.old_v, d.new_v
FROM event ev
JOIN entity en ON en.id = ev.entity_id
LEFT JOIN delta d ON d.event_id = ev.id;

-- ---------------------------------------------------------------- public query-surface views
CREATE OR REPLACE VIEW workflows AS          SELECT * FROM v_workflow;
CREATE OR REPLACE VIEW tasks AS              SELECT * FROM v_task;
CREATE OR REPLACE VIEW events AS             SELECT * FROM v_event;
CREATE OR REPLACE VIEW comments AS           SELECT * FROM v_comment;
CREATE OR REPLACE VIEW workflow_stages AS    SELECT * FROM v_stage;
CREATE OR REPLACE VIEW stage_distribution AS SELECT * FROM v_stage_distribution;
CREATE OR REPLACE VIEW open_items AS         SELECT * FROM v_open_items;

-- ---------------------------------------------------------------- seed data (default config)
INSERT IGNORE INTO stage_def (stage_key, seq, name_zh, name_en) VALUES
('development', 1, '开发', 'Development'),
('deployment',  2, '部署', 'Deployment'),
('verify',      3, '校验', 'Verification'),
('accept',      4, '验收', 'Acceptance'),
('dual_run',    5, '双跑', 'Dual run'),
('cutover',     6, '割接', 'Cutover');

-- name_en is not given in the contract; English names below are authored here.
INSERT IGNORE INTO reason (reason_key, kind, owner_side, name_zh, name_en, note_required) VALUES
('customer_env_issue',     'gate_override',  'customer', '客户环境问题',         'Customer environment issue', 0),
('customer_source_data',   'gate_override',  'customer', '客户源数据问题',       'Customer source data issue', 0),
('customer_takeover',      'discard',        'customer', '移交客户自行迁移',     'Handed over to customer', 0),
('duplicate',              'discard',        'customer', '重复',                 'Duplicate', 0),
('known_acceptable_diff',  'gate_override',  'delivery', '已知可接受差异',       'Known acceptable difference', 1),
('lookup_static_verified', 'gate_override',  'delivery', '维表抽检通过无日常对账', 'Lookup table spot-checked, no routine reconciliation', 1),
('merged',                 'discard',        'customer', '已合并',               'Merged', 0),
('na_code_outside_repo',   'not_applicable', 'delivery', '代码不在本仓库',       'Code outside this repository', 1),
('na_continuous_stream',   'not_applicable', 'delivery', '常驻流任务',           'Continuous streaming job', 1),
('na_event_or_queue',      'not_applicable', 'delivery', '事件或队列触发',       'Event or queue triggered', 1),
('na_external_scheduler',  'not_applicable', 'unknown',  '外部系统调度',         'External scheduler', 1),
('na_lookup_passive',      'not_applicable', 'delivery', '维表无独立调度',       'Lookup table without own schedule', 1),
('na_manual_trigger',      'not_applicable', 'delivery', '手工触发',             'Manually triggered', 1),
('na_other',               'not_applicable', 'unknown',  '其他',                 'Other', 1),
('no_legacy_output',       'gate_override',  'customer', '老系统没有可比对的输出', 'No comparable legacy output', 0),
('no_source',              'discard',        'customer', '无数据源',             'No data source', 0),
('obsolete',               'discard',        'customer', '业务不再需要',         'No longer needed by the business', 0),
('other_discard',          'discard',        'unknown',  '其他',                 'Other', 1),
('other_override',         'gate_override',  'unknown',  '其他',                 'Other', 1),
('rej_business_logic',     'reject',         'delivery', '业务逻辑错误',         'Business logic error', 0),
('rej_other',              'reject',         'unknown',  '其他',                 'Other', 1),
('rej_row_count',          'reject',         'delivery', '行数不一致',           'Row count mismatch', 0),
('rej_value_or_schema',    'reject',         'delivery', '数值或结构不一致',     'Value or schema mismatch', 0),
('rej_verification_method','reject',         'unknown',  '验数方法不认可',       'Verification method not accepted', 0),
('rule_not_applicable',    'gate_override',  'delivery', '验数规则不适用',       'Verification rule not applicable', 1),
('source_retired',         'discard',        'customer', '源已下线',             'Source retired', 0);

-- rule has no natural key: this block is for a fresh install and must run once.
INSERT INTO rule (k_pattern, to_value, side, reason_kind) VALUES
('stage.development', 'pass', 'delivery', NULL),
('stage.deployment',  'pass', 'delivery', NULL),
('stage.verify',      'pass', 'delivery', NULL),
('stage.accept',      'pass', 'customer', NULL),
('stage.dual_run',    'pass', 'delivery', NULL),
('stage.cutover',     'pass', 'delivery', NULL),
('stage.%',           'na',   'any',      'not_applicable'),
('stage.development', 'open', 'delivery', 'reject'),
('stage.deployment',  'open', 'delivery', 'reject'),
('stage.verify',      'open', 'delivery', 'reject'),
('stage.verify',      'open', 'customer', 'reject'),
('stage.accept',      'open', 'customer', 'reject'),
('stage.dual_run',    'open', 'delivery', 'reject'),
('stage.cutover',     'open', 'delivery', 'reject'),
('scope',             'out',  'any',      'discard'),
('scope',             'in',   'any',      NULL);
