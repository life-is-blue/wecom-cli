# 0009. Resource conventions: names, custom methods, etag, soft delete

Status: accepted (adopt, with documented deviations) · Date: 2026-10-31

## Context

`docs/m0-naming-spec.md` fixed AIP naming. This ADR checks it against the pages (AIP 121/122/131-136/154/164) and records
deviations, so nobody re-opens them.

What the sources say, and how openetl stands:

| Rule (source) | openetl | Verdict |
| --- | --- | --- |
| `name` is the resource name, no other field called `name` (G2) | `name` OUTPUT_ONLY; old key `name` became `display_name` | conforms |
| Collection ids plural camelCase `[a-z][a-zA-Z0-9]*` (G2) | `workflows`, `tasks`, `importJobs`, `comments` | conforms |
| User IDs lowercase RFC-1034 style `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$` (G2) | `wf-000123`, project slugs | conforms (project slug rule: check the regex is enforced, unverified) |
| `uid` OUTPUT_ONLY (G2) | yes | conforms |
| Get: GET, no body (G3) | yes | conforms |
| Create: `{resource}_id` on the request; `ALREADY_EXISTS` (G3) | `?workflow_id=` | conforms |
| Update: PATCH, `update_mask`; omitted mask = all populated fields; `*` full replace; "state fields must not be directly writable in update methods" (G5) | `PATCH ... ?update_mask=`; stages change via `:advanceStage` etc. | conforms in shape. Behaviour for an omitted mask and for stage fields inside `PATCH` is unverified in code |
| Delete soft: marks, not removes; `Undelete`; list hides by default, `show_deleted`; `delete_time` + `purge_time` (G12) | `delete_time`, `:undelete`, `show_deleted` | conforms; `purge_time` absent by design (0005) |
| Delete carries `etag`; `allow_missing` optional (G3) | `DELETE ?etag=` | conforms |
| Custom methods `:verb`, POST if side effects, GET if retrieval (G6); "prefer standard methods" (G1) | `:advanceStage`, `:regressStage`, `:setScope`, `:resolve`, `:reopen`, `:assign`, `:undelete`, `:diff` (GET), `:apply` (POST), `:query` (POST) | conforms, except `:query` (POST for retrieval, see 0006) |
| etag: string, named `etag`, server-computed; match must succeed; mismatch `ABORTED`; omitted etag permitted (G8) | `etag` from the entity's latest event id; `ETAG_MISMATCH` | conforms |
| `request_id` on the request, not the resource (G9) | yes | conforms; see 0004 |

Two points where evidence suggests care, neither a conflict:

* AIP-154: "A strongly validated etag means that two resources bearing the same etag are byte-for-byte identical." openetl's
  etag derives from the entity's own latest event. A workflow's derived fields (`health`, `open_issues`, `current_stage`)
  change when a *comment* entity changes, without a new workflow event. So the etag is not strong for those fields. It is
  adequate for its job (detect concurrent writes to the entity's own keys). Document it as such; do not weaken
  `ETAG_MISMATCH` semantics.
* AIP-122: "Resources must not expose tuples, self-links" (G2) and `docs/m0-naming-spec.md` says display codes (`WF-000123`)
  are parsed case-insensitively. The `code` column is a display convenience; `name` stays canonical in API output.

## Options

1. Keep all of the above and record the deviations (`:query` POST, etag scope, Operation name).
2. Chase strict AIP conformance (strong etags covering derived fields, Operations service).

## Decision

**Adopt option 1.** Strict conformance buys nothing for a one-project-per-instance foundation and would add a second etag
source (a project-wide version counter) or an Operations service. Re-open only if a client demonstrates a lost update through
a derived field.

## Consequences

* Add a "Deviations" paragraph to `api/openapi.yaml` info (other workers own that file): `:query` is POST; etag scope;
  Operation name is the job path.
* AIP-164 documentation clause is met by 0005.

## Evidence

G1-G3, G5, G6, G8, G9, G12; `docs/m0-naming-spec.md`, `api/openapi.yaml`.
