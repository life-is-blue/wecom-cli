# Import jobs: row format and semantics

Endpoints (see `api/openapi.yaml`): `POST /v1/projects/{p}/importJobs` (stage), `GET importJobs/{id}`,
`GET importJobs/{id}:diff`, `POST importJobs/{id}:apply` and `:rollback` (admin). Stage and diff need editor.

## Stage request

```json
{
  "source": "legacy_console",
  "request_id": "optional-uuid (replay returns the same job)",
  "rows": [
    {
      "source": {"system": "legacy_console", "reference": "daily_loader", "row_hash": "<hash of the whole source row>"},
      "data": {
        "display_name": "daily_loader",
        "name": "projects/p/workflows/wf-000123",
        "labels": {"group": "dwh"}, "annotations": {"memo": "text"},
        "assignees": {"dev_owner": "accounts/alice"},
        "stages": [{"stage": "development", "state": "PASSED"}],
        "scope": "OUT_OF_SCOPE"
      },
      "tasks": [
        {"source": {"reference": "t-101", "row_hash": "<hash of the source task row>"},
         "data": {"display_name": "extract", "labels": {"kind": "sql"}, "annotations": {}, "assignees": {"dev_owner": "accounts/alice"}}}
      ]
    }
  ]
}
```

* `source` (job) required, <= 64 chars; it is the default `rows[].source.system` and the system used to find missing rows.
* `data.display_name` required. `data.name` (platform code, wf-NNN, optional) matches directly.
* `source.reference` (<= 255) is the stable id in the source system; `row_hash` (<= 64) is optional and defaults to a sha256 of the normalized data.
* `tasks` (optional, 1..n per row) are the tasks of that workflow. A task needs `source.reference` (<= 255, unique per source system within the batch and stable across imports; its system is the row's) and `data.display_name`; `labels`, `annotations` and `assignees` follow the workflow rules, `row_hash` defaults to a sha256 of the normalized data. Other Task fields are ignored. A bad task (or a duplicate task reference) makes its whole row invalid (`INVALID_TASK`, `DUPLICATE_SOURCE_REFERENCE`). `merged_into` is rejected. `scope` is only taken when `OUT_OF_SCOPE`.
* A row that fails validation is reported in the response `row_errors` (`row_no` 1-based, `reason`, `message`; also counted as `summary.INVALID`) and left out of the diff; the batch is still staged. Duplicate (system, reference) or duplicate platform code within the batch: the later row is invalid.
* Response: ImportJob with `state` STAGED and `summary`: workflow counts NEW, CHANGED, UNCHANGED, MISSING, CONFLICT, SUSPECT, INVALID, plus the same classes for tasks as `TASK_NEW`, `TASK_CHANGED`, `TASK_UNCHANGED`, `TASK_MISSING`, `TASK_CONFLICT` (task keys appear only when non-zero). Keys without the `TASK_` prefix always count workflow rows.

## Classification (diff)

Matching order: platform code, then `source.system` + `source.reference`; a row with no code and no reference is matched by name + `labels.group` (case-insensitive) -> SUSPECT; otherwise NEW. Soft-deleted workflows are matched too; a row whose matched workflow is deleted when the job is applied (even one staged UNCHANGED) counts as a CONFLICT that no resolution applies to: the row and its tasks are skipped, never created or changed under a deleted workflow.

* UNCHANGED: matched and `row_hash` equals the stored `source.hash`.
* CHANGED: otherwise; source-owned keys (display_name, source.*) differ, or platform-owned keys that were never set can be filled.
* CONFLICT: a platform-owned key (stages, scope, assignees, labels, annotations) differs from the row, was last edited by a person (cli/api), and the source changed it since the last applied import of that reference. A workflow soft-deleted in the platform is also a CONFLICT.
* SUSPECT: no source id, name + group match an existing workflow.
* MISSING: live workflow with `source.system` = job source whose reference is absent from the batch (diff line has `row_no` 0).

Tasks are classified the same way against the live tasks (matched on `source.system` + task reference only; there is no SUSPECT), independently of whether the workflow row is UNCHANGED. A task is TASK_MISSING when its workflow is matched by a row of this batch but the batch no longer lists the task (tasks of absent workflows are not reported). The task's own `source.hash`/name are source-owned; labels, annotations and assignees are platform-owned exactly as for workflows.

Diff lines: `{row_no, source, action, match, changes:[{key, old_value, new_value, kind}]}`; `kind` (extension) is `SOURCE`, `FILL` or `CONFLICT` (UPPER_SNAKE like every enum; `KIND_UNSPECIFIED` is reserved). Task lines carry `action` `TASK_NEW|TASK_CHANGED|TASK_UNCHANGED|TASK_MISSING|TASK_CONFLICT`, the `row_no` of their workflow (0 for TASK_MISSING), the task's `source.reference` and `match` = the task resource name; `filter=action=TASK_NEW` selects them (and `action=NEW` selects workflow lines only). After apply the diff lists workflow rows only; task counts stay in the summary. Keys are API names (display_name, labels.x, assignees.x, stages.x, source.reference, source.row_hash); stage and scope values are the stored ones (pass/open/na, in/out). `filter=action=NEW OR action=CONFLICT`; `page_token` is an opaque signed token bound to the job and filter.

## Apply / rollback

`:apply` body (all optional): `{"note", "reason", "validate_only", "resolutions":[{"row_no":2,"resolution":"merge"}]}` (extension: `resolutions`). Conflict rows accept `merge` (apply source-owned updates only) or `skip`; suspect rows accept `link`, `create` or `skip`. Unresolved conflicts and suspects are skipped. One transaction, channel `import`; events carry `idempotency_key = imp-<job>-<row_no>` and note `import job <id> row <n> (...)`. Platform-owned keys are never overwritten, MISSING workflows and tasks are only counted. Second apply returns the same Operation without writing. Summary of an applied job adds CREATED, UPDATED, SKIPPED and `TASK_CREATED`, `TASK_UPDATED`, `TASK_SKIPPED`.

Tasks: a NEW task becomes a `task` entity with its source keys and its workflow (`workflow` = the workflow's entity id, the workflow being the matched one or the one this job creates; a task has exactly one) and one event `imp-<job>-<row_no>-<n>` (note `import job <id> row <n> task <system>:<reference>`); a CHANGED task gets its source-owned and fill changes. Tasks follow their row: they are skipped when the workflow row itself is skipped (unresolved CONFLICT/SUSPECT or `skip`), and a TASK_CONFLICT is only merged (source-owned updates) when the row's resolution is `merge`. An existing task is not re-linked to another workflow. Writes are batched (about 3000 workflows with 10500 tasks apply in roughly 4 s on a local MySQL 8).

`:rollback` writes correcting events (`corrects_event_id` = the import event, key `imprb-<job>-<event>`); created workflows and tasks are reverted and soft-deleted, updated ones get correcting events. Refused with FAILED_PRECONDITION `ROLLBACK_BLOCKED` (metadata `count`, `workflows`; the list may name tasks too) if any event not written by the job touched one of its entities (workflows or tasks) afterwards (person edit or a later import), or if a live task that is not the job's sits under a workflow the job created (rolling back deletes that workflow without cascading, and never deletes what a person created). Second rollback is a no-op.

Both return `Operation{name, done:true, response: ImportJob}`.
