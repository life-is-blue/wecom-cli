# 0006. Read surface: seven public views, AIP lists, opaque bound page tokens

Status: accepted (adopt views; adapt lists; two conflicts found) · Date: 2026-10-31

## Context

openetl exposes read access two ways: typed list/get endpoints (AIP-132 style) and `POST ...:query` over seven public views
(`workflows, tasks, events, comments, workflow_stages, stage_distribution, open_items`).

What the sources say:

* BigQuery's metadata read API is SQL over system views: `INFORMATION_SCHEMA` views are "read-only, system-defined views"; "An
  INFORMATION_SCHEMA view needs to be qualified with a dataset or region"; "If neither a region qualifier nor a dataset
  qualifier is specified, you will receive an error"; they "cannot be used in DDL statements"; results "are not cached"; "the
  permissions are checked only at the parent level, not at an individual row level" (B3). View `JOBS` has columns `job_id`,
  `state`, `error_result`, `creation_time` (partitioning column), `start_time`, `end_time` (B4). BigQuery's own SQL entry point
  is `POST projects/{projectId}/queries` (B1), i.e. a retrieval done with POST because of the body.
* AIP-136: "`GET` must be used for methods retrieving data"; "`POST` must be used if the method has side effects" (G6). So
  `:query` as POST is a deviation, justified by SQL text length and shared with BigQuery.
* AIP-158: tokens "must be opaque (but URL-safe) strings, and must not be user-parseable"; "Base-64 encoding an otherwise-
  transparent page token is not a sufficient obfuscation mechanism"; "all other parameters ... must match the call that provided
  the page token ... if any arguments are different, the API should send an INVALID_ARGUMENT error"; negative `page_size` is
  `INVALID_ARGUMENT`; empty `next_page_token` at the end (G10).
* AIP-160: "`OR` operator has higher precedence than `AND`, unlike what is found in most programming languages"; "The expression
  `a AND b OR c` evaluates: `a AND (b OR c)`"; whitespace-separated literals are a fuzzy `AND`; invalid filter ->
  `INVALID_ARGUMENT` (G11). AIP-132: `order_by` is "a comma separated list of fields", ` desc` suffix, `show_deleted` (G4).
* Airflow chose `limit` (default 50) + `offset`, `order_by` with a `-` prefix, and `*_pattern` filters (A7). Not AIP; offset
  paging drifts while rows are inserted, which matters for an append-only log.

openetl today: `internal/store/list.go` token is `base64url("k:<id>")` or `"o:<n>"`; it checks only that the kind fits the
query, not that filter/order are unchanged. `internal/store/filter.go` binds `AND` tighter than `OR` (`parseOr` calls
`parseAnd`), the reverse of AIP-160.

## Options

1. Keep views + lists as they are.
2. Adopt the AIP semantics fully: swap filter precedence, HMAC page tokens that embed a hash of (parent, filter, order_by).
3. Replace views with an `INFORMATION_SCHEMA`-style catalog (`openetl.INFORMATION_SCHEMA...`).

## Decision

* **Adopt** the BigQuery lesson for the views, keep our names: small fixed catalog, read-only, documented columns, no DDL,
  one project filter applied by the server. Reject option 3 (qualified catalog names add syntax for one project per instance
  and conflict with the lowercase-plural resource names already decided).
* Keep `:query` as POST and write the reason in the OpenAPI description.
* **Adapt lists** (both are conflicts, see README):
  * Page token: sign it (HMAC with a server secret, same key mechanism as the fs signer) and include a hash of
    `(parent, filter, order_by)`; mismatch -> `INVALID_ARGUMENT INVALID_PAGE_TOKEN`. Smallest change: edit `list.go` token
    encode/decode, no schema. Authorization is unaffected (project scope is enforced per request).
  * Filter precedence: either implement AIP-160 precedence (swap the two parser levels) or reject mixed `AND`/`OR` without
    parentheses with `INVALID_ARGUMENT`. The second is the safer smallest change for agents because it removes ambiguity
    without silently changing the meaning of existing filters; AIP-160 itself says services "should not" require parentheses,
    so full compliance later is the swap.
* Keep keyset paging for id order and offset for other orders, but document that offset pages can repeat or skip rows when
  writes interleave (Airflow's `offset` has the same property).

## Consequences

* Views remain the stable analytics contract; `views_drift_test.go` already pins CLI/view columns. Add the columns to the
  OpenAPI `QueryResponse` description once, not per view.
* `page_size` bound (<= 1000 in query) and 8 MiB cap match BigQuery's "responses are also limited to 10 MB" (B1) in spirit.

## Evidence

B1, B3, B4, G4, G6, G10, G11, A7; `internal/store/list.go:90-125`, `internal/store/filter.go:55-92`, `docs/security.md`.
