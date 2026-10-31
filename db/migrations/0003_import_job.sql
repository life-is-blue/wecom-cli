-- openetl 0004: import job idempotency (request_id) and a lookup index on import_row.source_ref.
-- Import events are attributable to their batch through event.idempotency_key = 'imp-<batch>-<row>'
-- (rollback events: 'imprb-<batch>-<event>'), so no extra event column is needed.

SET NAMES utf8mb4;

ALTER TABLE import_batch
  ADD COLUMN request_id VARCHAR(64) NULL,
  ADD UNIQUE KEY uq_import_request (project_id, request_id);

ALTER TABLE import_row ADD KEY idx_import_ref (source_ref);
