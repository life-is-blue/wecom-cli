-- openetl 0005: bind an idempotency key to the request that first used it.
-- idem_fp is a hash of (method, path, canonical body/query without request_id, validate_only, etag).
-- NULL for events written before this migration and for import events: those replay without a payload check.
ALTER TABLE event ADD COLUMN idem_fp CHAR(32) NULL;
