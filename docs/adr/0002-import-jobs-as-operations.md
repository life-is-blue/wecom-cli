# 0002. Import jobs: synchronous Operation, two state axes

Status: accepted (adapt) · Date: 2026-10-31

## Context

`importJobs` is openetl's only long-running resource: `:apply` and `:rollback` return an `Operation`.

What the sources say:

* BigQuery jobs have a lifecycle and an outcome as separate axes. `JobStatus.state`: "Valid states include 'PENDING',
  'RUNNING', and 'DONE'." `errorResult`: "If present, indicates that the job has completed and was unsuccessful." (B1). So
  failure is `DONE` + `errorResult`, not a fourth state.
* `jobs.query` is synchronous with a wait budget: `timeoutMs` default 10 s; "If the query has not yet completed, jobComplete is
  false", the response carries `jobReference` "even if the original request timed out", and the client keeps calling
  `getQueryResults` (B1). `jobs.insert` "Starts a new asynchronous job" (B1). `JobReference.jobId` is client-suppliable
  (letters, digits, `_`, `-`, max 1,024 chars) (B1). Whether a duplicate `jobId` is rejected is not stated in the sources
  read (B7, unverified).
* BigQuery job history is read through `jobs.get`/`jobs.list` (6 months, B1) and `INFORMATION_SCHEMA.JOBS` (180 days, B4),
  with `creation_time`, `start_time`, `end_time` columns.
* AIP-151: methods that "might take a significant amount of time" return `google.longrunning.Operation`; "A good rule of
  thumb is 10 seconds." A finished one has `done` true; failures go in `Operation.error`; the metadata type carries progress
  (G7).
* Airflow `DagRunState` = `queued, running, success, failed` (A1); a run is the unit that outlives a request.

openetl today: `ImportJob.state` = `STAGED, APPLIED, FAILED, ROLLED_BACK` (`api/openapi.yaml`, `import_batch.state`). Apply
runs in one transaction and measured ~4 s for 3000 workflows + 10500 tasks (`docs/import-format.md`); the handler always
returns `Operation{done:true}`. `docs/cli-import.md` says the CLI "waits until the job leaves the running state", but no
running state exists.

## Options

1. Keep synchronous apply; keep the `Operation` envelope with `done:true` so a future async apply is not a breaking change.
2. Make apply asynchronous now with `APPLYING` state and a background worker (BigQuery/AIP-151 style).
3. Drop `Operation`, return the `ImportJob` directly.

## Decision

**Adapt: option 1.**

* Keep `Operation` (`name`, `done`, `response`, `error`) and keep `GET importJobs/{id}` as the polling call. Document that
  today `done` is always true on return, because apply is below AIP-151's 10 s guideline.
* Keep the two-axis idea: `Operation.done/error` is the lifecycle/outcome axis (BigQuery `DONE`+`errorResult`);
  `ImportJob.state` is the domain axis. Do not add `RUNNING`/`APPLYING` until measured apply time exceeds ~10 s; adding a state
  value is additive, the envelope is already in place.
* Deviation from AIP-151 to record: the Operation `name` is the importJob path, there is no `operations/*` service. Acceptable
  for one operation type; AIP-151 requires the `Operations` service only when you adopt full LRO.
* Concurrency: `getBatch(..., forUpdate)` row lock (`internal/store/import.go:114`) is the guard that makes the second `:apply`
  return the same result without writing. Keep it.

## Consequences

* Fix the CLI doc sentence about a "running state" (conflicts table, effort XS). `--wait`/`--no-wait` flags can stay as
  forward-compatible no-ops.
* No migration. No new feature.

## Evidence

B1, B4, G7, A1; `docs/import-format.md` (timing, "Second apply returns the same Operation"), `internal/server/handlers_import.go:234-278`.
