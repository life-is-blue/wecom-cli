# 0007. Error model: AIP-193 body, stable reasons, no backend detail

Status: accepted (adopt; one low-severity deviation) · Date: 2026-10-31

## Context

What the sources say:

* AIP-193: the JSON body is an `error` object with `code`, `message`, `status`, `details`; the JSON `code` "is an HTTP status
  code, _not_ the direct value of `google.rpc.Status.code`"; `status` is the `google.rpc.Code` name; services "must use the
  canonical error codes defined in `google.rpc.Code`" (G13).
* `ErrorInfo.reason` "at most 63 characters and match ... `[A-Z][A-Z0-9_]+[A-Z0-9]`"; `domain` "must be a globally unique
  value" (typically the service name, e.g. `pubsub.googleapis.com`); `metadata` keys match `[a-z][a-zA-Z0-9-_]+`, "ideally be
  lowerCamelCase"; once a key is published for a (reason, domain) it must keep being included (G13, G14). `RetryInfo.retry_delay`
  exists for retry hints; clients should back off exponentially (G14).
* AIP-154: etag mismatch is `ABORTED` (G8). AIP-133: duplicate id is `ALREADY_EXISTS`, or `PERMISSION_DENIED` if the caller
  cannot see the duplicate (G3).
* BigQuery `ErrorProto` = `{reason, location, debugInfo, message}`; `debugInfo` is "internal to Google and should not be used"
  (B1). `JobStatus.errors` lists "the first errors" and "do not necessarily mean that the job has not completed or was
  unsuccessful" while `errorResult` is the final verdict (B1).
* Airflow's error body is `HTTPExceptionResponse {detail: string | object}`; execution routes put a machine key in
  `detail.reason` (`not_found`, `invalid_state`, `running_elsewhere`, `invalid_loop_gate`) plus `previous_state`, with status
  `404/409/410/422` (A7, A4). Machine-readable, but each route invents its own keys and there is no status-name field.

openetl today: `api/openapi.yaml` `Error {error:{code,status,message,details[]}}`; `core.Error{Status, Reason, Message,
Metadata}` with the status strings of `google.rpc.Code`; a grep of every literal reason passed to `NewError`/`invalid`/
`invalidArg` finds none violating the reason regex; metadata keys in use (`blocking_stage`, `required_kind`, `valid_reasons`,
`current_etag`, ...) mix snake_case with the lowerCamelCase the proto recommends, but still match the permitted regex.
`QUERY_FAILED` carries only the numeric DB error code (`docs/security.md`).

## Options

1. Keep the AIP-193 shape and reason catalogue as is.
2. Switch to Airflow's `{detail:{reason,...}}`.
3. Keep, and align `domain` / `@type` to values AIP-193 and the proto call for.

## Decision

**Adopt option 1.** Airflow's shape is a convenience of one codebase; ours is the better contract for agents because `status`
gives a coarse retry class and `reason` a stable key. Specifics:

* Reasons are API: never rename one, only add. Add the catalogue of reasons per status to `api/openapi.yaml` (the sample list
  in `docs/m0-naming-spec.md` is already the start). Metadata keys, once published, stay (AIP-193 rule above).
* Failure after partial work is `Operation.error`, final verdict only (BigQuery `errorResult` semantics).
* Retry class for clients, derived from `status`: retry `RESOURCE_EXHAUSTED` after `Retry-After`; `UNAVAILABLE`/`INTERNAL`
  with backoff; re-read then retry `ABORTED`; never retry `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, `PERMISSION_DENIED`,
  `NOT_FOUND`, `ALREADY_EXISTS`. (Skill text should say this once.)
* Never echo backend errors (BigQuery withholds `debugInfo` for the same reason). Already true.
* **Deviation, not fixed now:** `domain: "openetl"` is not "globally unique" and the `@type` is written as the short string
  `ErrorInfo`. The exact JSON `@type` URL form is not shown on the pages read (G15, unverified), so do not change it without
  checking `google/rpc/error_details.proto`'s JSON mapping. Smallest change when needed: make the domain configurable, default
  to a hostname-like constant. Clients must treat both as opaque strings meanwhile.

## Consequences

* No code change. One doc task (reason catalogue) and one optional config.

## Evidence

G3, G8, G13, G14, B1, A4, A7; `api/openapi.yaml` (`Error`, `EtagMismatch` example), `internal/core/errors.go`.
