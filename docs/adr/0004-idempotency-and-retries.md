# 0004. Idempotency: request_id plus a request fingerprint

Status: accepted (adapt; one conflict found) · Date: 2026-10-31

## Context

Agents retry. Every write takes an optional `request_id` (UUID); the DB column is `event.idempotency_key` with
`UNIQUE (project_id, idempotency_key)`; `import_batch.request_id` has `UNIQUE (project_id, request_id)`.

What the sources say:

* AIP-155: "Providing a request ID must guarantee idempotency." "If a duplicate request is detected, the server should return
  the response for the previously successful request"; it "may return the current state of the resource instead";
  "APIs may choose any reasonable timeframe for honoring request IDs"; the field belongs on the request, "must not be a field on
  resources themselves" (G9).
* BigQuery `QueryRequest.requestId` is stricter about what a duplicate is: "a request is considered duplicate of another only
  if they have the same request_id and are actually duplicates. ... all parameters in the request that may affect the result are
  considered", while `timeout_ms` is not. Lifetime: "limited to 15 minutes". A duplicate mutating request returns "the results
  of the mutation if it completes ... the running operation if it is still in progress". Read-only queries "can ignore this
  token since they are nullipotent". It is "different from the job_id" (B1).
* Airflow's guard is uniqueness plus state: `UniqueConstraint("dag_id", "run_id", name="dag_run_dag_id_run_id_key")` (A2); the
  Execution API tolerates a repeated start from the same `(hostname, unixname, pid)` (A4). Airflow retries are modelled as data:
  `try_number`, `max_tries`, `UP_FOR_RETRY` (A1, A3). openetl executes nothing, so it has no retry state to model.

openetl today (`internal/store/apply.go:96-117`): if an event with that key exists, it returns the stored result with
`Replayed: true`; if the existing event belongs to a different entity, `ALREADY_EXISTS / REQUEST_ID_REUSED`. It does **not**
compare the new request's changes with the stored event's changes. A client reusing a `request_id` with a different payload on
the same entity (or on create, where `EntityID == 0`) gets the old result and believes its new payload was applied.

## Options

1. Status quo.
2. Store a fingerprint of the normalised request (`sha256` of `{method, name, update_mask, canonical changes, reason, note}`),
   compare on replay, `ALREADY_EXISTS REQUEST_ID_REUSED` on mismatch (BigQuery semantics).
3. Compare the stored event's delta rows with the recomputed changes at replay time, no new column.

## Decision

**Adapt: option 2 if a column is acceptable, otherwise option 3.** Option 3 needs no migration: the replay path already
calls `loadEventChanges`; compare it with the changes the request would produce on the same pre-state and fail on any
difference. Prefer 3 for the foundation (smaller blast radius, no schema change); revisit 2 if comparing proves ambiguous
(e.g. a replay after other edits changes the computed diff).

Keep unchanged:
* Server-side retention is unbounded (AIP-155 allows any timeframe; BigQuery's 15 minutes exists because it does not keep
  history forever, and openetl does). State this in `docs/cli-import.md` and the OpenAPI description of `request_id`.
* `validate_only` does not consume a `request_id` (BigQuery: "Dry run query requests are never considered duplicate").
  Verify with a test; unverified in code.
* Reads ignore `request_id` (BigQuery "nullipotent").
* Retry guidance for clients: retry on network errors, `429` (honour `Retry-After`) and `ABORTED/ETAG_MISMATCH` only after a
  re-read; never retry `FAILED_PRECONDITION`. `scripts` and skills should send a fresh UUID per logical action and reuse it on
  retry of that action only.

## Consequences

* Closes the "silent wrong success" gap. One new error path, reason `REQUEST_ID_REUSED` already exists.
* Import events use `imp-<job>-<row>` keys in the same namespace; they cannot collide with client keys only because the API
  requires UUIDs. Keep that validation.

## Evidence

G9, B1 (`QueryRequest.requestId`), A2, A4; `internal/store/apply.go:96-117`, `db/migrations/0003_import_job.sql`.
