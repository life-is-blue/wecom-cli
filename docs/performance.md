# Performance

Target scale: about 3000 workflows, 10000 tasks, tens of thousands of events/deltas. Every public view
and every list/get API must answer in well under a second.

## What was wrong

`v_latest` was `delta JOIN (SELECT entity_id, k, MAX(event_id) ... GROUP BY entity_id, k)`. Views such as
`v_workflow`, `v_stage`, `v_current_stage`, `v_task`, `v_attrs` join it about ten times; each join
re-materialised the grouped derived table without a usable index (quadratic). On the 3000-workflow project in
`openetl_perf` (MariaDB 10.11) `SELECT COUNT(*) FROM workflows` did not return within 30 s (earlier: 300 s).

## What changed (migration `0004_entity_latest.sql`)

* `entity_latest(entity_id, k, v JSON NULL, event_id, PRIMARY KEY (entity_id, k), KEY idx_k (k, entity_id))`:
  the newest value of every key per entity. A removed key keeps a row with `v = NULL` (as the old `v_latest` did).
* It is a **cache of `delta`, not a second source of truth**. `Apply` and the import bulk writer upsert it in the
  same transaction that inserts the deltas, only when the new `event_id` is greater, so rollback / `validate_only` /
  rejected writes leave it untouched. The migration backfills it once from `delta`.
* `v_latest` is now `SELECT entity_id, k, v, event_id FROM entity_latest`; all views keep working.
* `v_workflow_base`, `v_current_stage`, `v_comment`, `v_task`, `v_workflow` were rewritten without `GROUP BY` pivots:
  fixed keys are fetched by `LEFT JOIN entity_latest ... ON entity_id = e.id AND k = '...'` (primary-key lookups) and
  `attrs` / `assignees` by correlated `JSON_OBJECTAGG` subqueries over a PK range. Column names, types and
  semantics are unchanged (outputs of all public views were diffed against the old schema on the seeded data:
  identical). The views stay mergeable, so a query for one project or one workflow touches only its rows.
* Store code uses `entity_latest` for current values (`loadEntities`, validation `txEnv.latest`, list filters/sorting,
  event scope, attachment storage-key check). **Time travel (`as_of`) still reads `delta` directly** with its own query.
* Operations: `openetl-gateway admin verify-latest` compares the cache with a recomputation from `delta`
  (prints mismatches, exit 1) and `openetl-gateway admin rebuild-latest` empties and refills it in one transaction
  (`DELETE` + `INSERT ... SELECT`, not `TRUNCATE`, so it is transactional; run it in a quiet period).
  If the app account has restricted grants, it needs `INSERT, UPDATE, DELETE, SELECT` on `entity_latest`.
* Not touched: `loadImportExisting` (import diff) folds the whole `delta` history of the project on purpose
  (it needs which keys were ever touched and by which channel); one linear pass.

## Reproducing

```bash
# fresh database with 3000 workflows, 10000 tasks, ~6000 comments, stage advances (old schema, 0001-0003)
PERF_DB=openetl_perf_new scripts/perf-seed.sh
# after: apply migration 0004 (backfills entity_latest from delta)
OPENETL_DB_DSN='root@tcp(127.0.0.1:33306)/openetl_perf_new' openetl-gateway migrate
PERF_DB=openetl_perf_new scripts/perf-measure.sh                      # views via mysql, server-side 60 s limit
OPENETL_PERF_DSN='root@tcp(127.0.0.1:33306)/openetl_perf_new' go test -tags perf -run TestPerf -v ./internal/store   # store/API level
```

Seed: 19000 entities, 28793 events, 66793 deltas (3000 workflows, 6000 comments, 10000 tasks).
MariaDB 10.11.14, local, seconds / milliseconds, median of 5 for the Go numbers.

## Results

`mysql` client, `SELECT COUNT(*) FROM <view>` (no project filter):

| query | before | after |
| --- | --- | --- |
| workflows | 3.07 s | 0.10 s |
| tasks | 9.12 s | 0.05 s |
| events | 0.08 s | 0.07 s |
| comments | 0.20 s | 0.02 s |
| workflow_stages | 0.02 s | 0.02 s |
| stage_distribution | 3.32 s | 0.11 s |
| open_items | 0.18 s | 0.06 s |
| workflows page of 100 (ORDER BY id) | 0.54 s | 0.06 s |
| tasks page of 100 | 9.19 s | 0.01 s |

On the pathological copy of `openetl_perf` (3000 workflows with identical stage history) `COUNT(*) FROM workflows`:
not finished after 30 s before, 0.03 s after.

Store/API level (`Store.Query` adds the project filter, which is why its "before" is lower than the raw view):

| operation | before | after |
| --- | --- | --- |
| workflows list page of 100 | 341 ms | 53 ms |
| workflows list, filter current_stage + health | 871 ms | 140 ms |
| workflows list, order by display_name | 370 ms | 63 ms |
| workflow get | 338 ms | 45 ms |
| tasks list page of 100 | 8.6 ms | 6.4 ms |
| task get | 1.0 ms | 1.2 ms |
| comments list (one workflow) | 162 ms | 17 ms |
| events list (one workflow) | 30 ms | 18 ms |
| query COUNT(*) workflows | 412 ms | 66 ms |
| query COUNT(*) tasks | 9160 ms | 29 ms |
| query COUNT(*) comments | 164 ms | 19 ms |
| query COUNT(*) events | 58 ms | 66 ms |
| query COUNT(*) workflow_stages | 120 ms | 108 ms |
| query COUNT(*) open_items | 184 ms | 50 ms |
| query COUNT(*) stage_distribution | 589 ms | 108 ms |
| stage advance (Apply) | 3.9 ms | 4.2 ms |

Nothing is above 1 s. The slowest remaining items are the filtered workflow list (140 ms) and the view
counts that must scan every workflow (about 100 ms): they are linear in the number of entities.
`workflow get` spends most of its 45 ms in `v_open_items`, which aggregates all comments of the project.
