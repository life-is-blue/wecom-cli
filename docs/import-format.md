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
      }
    }
  ]
}
```

* `source` (job) required, <= 64 chars; it is the default `rows[].source.system` and the system used to find missing rows.
* `data.display_name` required. `data.name` (platform code, wf-NNN, optional) matches directly.
* `source.reference` (<= 255) is the stable id in the source system; `row_hash` (<= 64) is optional and defaults to a sha256 of the normalized data.
* Only workflows are imported (no tasks). `merged_into` is rejected. `scope` is only taken when `OUT_OF_SCOPE`.
* A row that fails validation is reported in the response `row_errors` (`row_no` 1-based, `reason`, `message`; also counted as `summary.INVALID`) and left out of the diff; the batch is still staged. Duplicate (system, reference) or duplicate platform code within the batch: the later row is invalid.
* Response: ImportJob with `state` STAGED and `summary` (NEW, CHANGED, UNCHANGED, MISSING, CONFLICT, SUSPECT, INVALID).

## Classification (diff)

Matching order: platform code, then `source.system` + `source.reference`; a row with no code and no reference is matched by name + `labels.group` (case-insensitive) -> SUSPECT; otherwise NEW. Soft-deleted workflows are matched too.

* UNCHANGED: matched and `row_hash` equals the stored `source.hash`.
* CHANGED: otherwise; source-owned keys (display_name, source.*) differ, or platform-owned keys that were never set can be filled.
* CONFLICT: a platform-owned key (stages, scope, assignees, labels, annotations) differs from the row, was last edited by a person (cli/api), and the source changed it since the last applied import of that reference. A workflow soft-deleted in the platform is also a CONFLICT.
* SUSPECT: no source id, name + group match an existing workflow.
* MISSING: live workflow with `source.system` = job source whose reference is absent from the batch (diff line has `row_no` 0).

Diff lines: `{row_no, source, action, match, changes:[{key, old_value, new_value, kind}]}`; `kind` (extension) is `source`, `fill` or `conflict`. Keys are API names (display_name, labels.x, assignees.x, stages.x, source.reference, source.row_hash); stage and scope values are the stored ones (pass/open/na, in/out). `filter=action=NEW OR action=CONFLICT`; `page_token` is an offset.

## Apply / rollback

`:apply` body (all optional): `{"note", "reason", "validate_only", "resolutions":[{"row_no":2,"resolution":"merge"}]}` (extension: `resolutions`). Conflict rows accept `merge` (apply source-owned updates only) or `skip`; suspect rows accept `link`, `create` or `skip`. Unresolved conflicts and suspects are skipped. One transaction, channel `import`; events carry `idempotency_key = imp-<job>-<row_no>` and note `import job <id> row <n> (...)`. Platform-owned keys are never overwritten, MISSING workflows are only counted. Second apply returns the same Operation without writing. Summary of an applied job adds CREATED, UPDATED, SKIPPED.

`:rollback` writes correcting events (`corrects_event_id` = the import event, key `imprb-<job>-<event>`); created workflows are reverted and soft-deleted. Refused with FAILED_PRECONDITION `ROLLBACK_BLOCKED` (metadata `count`, `workflows`) if any event not written by the job touched one of its entities afterwards (person edit or a later import). Second rollback is a no-op.

Both return `Operation{name, done:true, response: ImportJob}`.
