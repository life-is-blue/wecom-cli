# CLI reference

Generated from the live `openetl --help` tree by `python3 scripts/audit/audit.py --write-cli-reference`; `audit.py` fails when this
page is stale, so every command and flag below exists and every real command and flag is listed. Workflows are in `skills/`, the
import mapping in `docs/cli-import.md`, vocabulary (stages, reasons, enums) in `scripts/audit/vocab.md`.

Conventions: output is one compact JSON document on stdout (`--output FILE` writes a file, `--page-count N` prints NDJSON, one page per line);
errors are `{"error":{"code","status","message","details"}}` on stderr. Exit codes: `0` ok, `1` runtime or API error, `2` usage error.
`WF` is `WF-000123`, `wf-000123` or `projects/p/workflows/wf-000123`; `TK`, `CM`, `AT` likewise. Write commands accept `--dry-run`
(`validate_only`), `--etag`, `--request-id`, `--note` and `--reason`.

Environment: `OPENETL_BASE_URL`, `OPENETL_PROJECT`, `OPENETL_TOKEN` (wins over the saved login), `OPENETL_ALLOW_HTTP=1`, `OPENETL_SOURCE_PASSWORD`.

## Global flags

```
      --base-url string   server base URL (env OPENETL_BASE_URL)
  -h, --help              help for openetl
  -o, --output string     write output to FILE instead of stdout
      --page-count int    list/query: fetch up to N pages and print NDJSON (one page per line)
      --project string    project id (env OPENETL_PROJECT)
  -v, --version           version for openetl
```

## Commands

- `openetl attachment` — get
- `openetl auth` — login, logout, status
- `openetl comment` — add, delete, list, reopen, resolve
- `openetl config` — get, set
- `openetl events` — get, list
- `openetl import` — apply, diff, rollback, show, stage
- `openetl member` — add, list, remove
- `openetl scope` — set
- `openetl stage` — advance, regress
- `openetl task` — assign, create, delete, get, list, restore, update
- `openetl token` — create, list, revoke
- `openetl workflow` — create, delete, get, list, restore, update

### openetl attachment get

```
Download an attachment to --output (the file bytes; stdout gets a JSON summary)

Usage:
  openetl attachment get WF AT --output FILE [flags]

Flags:
  -h, --help   help for get
```

### openetl auth login

```
Verify an API token via GET /v1/me and save it encrypted

Usage:
  openetl auth login [--token-stdin] [flags]

Flags:
  -h, --help          help for login
      --token-stdin   read the token from stdin (default when stdin is not a terminal)
```

### openetl auth logout

```
Delete the saved credentials

Usage:
  openetl auth logout [flags]

Flags:
  -h, --help   help for logout
```

### openetl auth status

```
Show whether a valid token is available (exit 1 if not)

Usage:
  openetl auth status [flags]

Flags:
  -h, --help   help for status
```

### openetl comment add

```
Add a comment, optionally with file attachments as evidence

Usage:
  openetl comment add WF --body TEXT [--kind note|question|issue|risk] [--attach FILE]... [flags]

Flags:
      --attach stringArray   file to upload as an attachment (repeatable, max 100 MiB)
      --body string          comment text (immutable after creation)
      --dry-run              validate only (validate_only); nothing is written
      --etag string          expected etag of the target (optimistic concurrency)
  -h, --help                 help for add
      --kind string          note, question, issue or risk (default "note")
      --note string          free-text note recorded with the change
      --reason string        reason_key from the project config
      --request-id string    idempotency key, a UUID (default: generated)
```

### openetl comment delete

```
Soft-delete a comment

Usage:
  openetl comment delete WF CM [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for delete
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl comment list

```
List comments of a workflow

Usage:
  openetl comment list WF [--filter 'resolved = false'] [--page-count N] [flags]

Flags:
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
```

### openetl comment reopen

```
Mark a resolved comment unresolved

Usage:
  openetl comment reopen WF CM [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for reopen
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl comment resolve

```
Mark a comment resolved

Usage:
  openetl comment resolve WF CM [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for resolve
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl config get

```
Get the project configuration

Usage:
  openetl config get [--section stages|reasons|rules|settings] [flags]

Flags:
  -h, --help             help for get
      --section string   print only one section
```

### openetl config set

```
Replace whole config sections (JSON in) or merge single settings

Usage:
  openetl config set [--stages FILE|-] [--reasons FILE|-] [--rules FILE|-] [--settings FILE|-] [--setting k=v]... [flags]

Flags:
      --dry-run               validate only (validate_only); nothing is written
      --etag string           expected etag of the target (optimistic concurrency)
  -h, --help                  help for set
      --note string           free-text note recorded with the change
      --reason string         reason_key from the project config
      --reasons string        reasons JSON (file or - for stdin); replaces the whole section
      --request-id string     idempotency key, a UUID (default: generated)
      --rules string          rules JSON (file or - for stdin); replaces the whole section
      --setting stringArray   settings key=value, merged into the current settings (repeatable)
      --settings string       settings JSON (file or - for stdin); replaces the whole section
      --stages string         stages JSON (file or - for stdin); replaces the whole section
```

### openetl events get

```
Get one audit event

Usage:
  openetl events get WF EV [flags]

Flags:
  -h, --help   help for get
```

### openetl events list

```
List audit events of a workflow

Usage:
  openetl events list WF [--filter EXPR] [--as-of RFC3339] [--page-count N] [flags]

Flags:
      --as-of string        only events recorded (create_time) up to this instant
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
```

### openetl import apply

```
Merge a staged job into the workflows (admin; asks for confirmation)

Usage:
  openetl import apply JOB [--yes] [--dry-run] [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for apply
      --no-wait             return the Operation immediately instead of waiting for completion
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --wait duration       how long to wait for completion (default 10m0s)
      --yes                 confirm without prompting (required when stdin is not a terminal)
```

### openetl import diff

```
Classes: CONFLICT and SUSPECT need a human decision; CHANGED and NEW are merged on apply; MISSING and UNCHANGED are informational.
TASK_* classes are the same for tasks (a TASK_CONFLICT is merged with its workflow's merge resolution).
By default rows of every class except UNCHANGED are listed (first --limit each). --raw prints the server's diff pages instead (--filter, --page-size, --page-count).

Usage:
  openetl import diff JOB [--limit N] [--action CLASS]... [--raw] [flags]

Flags:
      --action stringArray   only this class (repeatable): CONFLICT, SUSPECT, CHANGED, NEW, MISSING, TASK_CONFLICT, TASK_CHANGED, TASK_NEW, TASK_MISSING, UNCHANGED, TASK_UNCHANGED
      --filter string        AIP-160 filter expression
  -h, --help                 help for diff
      --limit int            rows listed per class (default 10)
      --order-by string      e.g. 'create_time desc'
      --page-size int        items per page (server default 50, max 1000)
      --page-token string    continue from a next_page_token
      --raw                  print the server's diff pages (use with --filter/--page-size/--page-count)
```

### openetl import rollback

```
Roll back a whole applied job (admin; asks for confirmation)

Usage:
  openetl import rollback JOB [--yes] [--dry-run] [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for rollback
      --no-wait             return the Operation immediately instead of waiting for completion
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --wait duration       how long to wait for completion (default 10m0s)
      --yes                 confirm without prompting (required when stdin is not a terminal)
```

### openetl import show

```
Show an import job: state and row counts per class

Usage:
  openetl import show JOB [flags]

Flags:
  -h, --help   help for show
```

### openetl import stage

```
Stages rows only; no workflow changes until `import apply`.
A mysql:// source is scanned read-only; its password comes from the DSN or OPENETL_SOURCE_PASSWORD and is never printed or sent to the server.
--dry-run reads and maps the source locally and reports what would be staged; nothing is sent.

Usage:
  openetl import stage --from SRC --map mapping.yaml [--source-system NAME] [flags]

Flags:
      --dry-run                validate only (validate_only); nothing is written
      --etag string            expected etag of the target (optimistic concurrency)
      --format string          force csv or ndjson for file/stdin sources
      --from string            source: file path (.csv or NDJSON), - for NDJSON on stdin, or mysql://user@host:port/db
  -h, --help                   help for stage
      --map string             mapping.yaml (see docs/cli-import.md)
      --max-rows int           refuse to stage more rows than this in one job (default 100000)
      --note string            free-text note recorded with the change
      --reason string          reason_key from the project config
      --request-id string      idempotency key, a UUID (default: generated)
      --source-system string   source system label (default: mapping source_system, else the source)
```

### openetl me

```
Show the current identity and project memberships

Usage:
  openetl me [flags]

Flags:
  -h, --help   help for me
```

### openetl member add

```
Add a member or change role/side (idempotent PUT)

Usage:
  openetl member add ACCOUNT --role viewer|editor|admin --side delivery|customer [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for add
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --role string         viewer, editor or admin (agents cannot be admin)
      --side string         delivery or customer
```

### openetl member list

```
List project members

Usage:
  openetl member list [--page-count N] [flags]

Flags:
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
```

### openetl member remove

```
Remove a member

Usage:
  openetl member remove ACCOUNT [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for remove
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl query

```
Run a read-only SQL SELECT over the documented views (see `openetl views`)

Usage:
  openetl query "<SELECT ...>" [--page-size N] [--page-count N] [flags]

Flags:
  -h, --help                help for query
      --page-size int       rows per page (max 1000)
      --page-token string   continue from a next_page_token
```

### openetl scope set

```
Set scope; out needs a discard reason, `--reason merged` needs --into

Usage:
  openetl scope set WF --to in|out [--reason R] [--into WF] [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for set
      --into string         target workflow when --reason merged
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --to string           in or out
```

### openetl stage advance

```
Mark a stage PASSED

Usage:
  openetl stage advance WF --stage STAGE [flags]

Flags:
      --dry-run                validate only (validate_only); nothing is written
      --etag string            expected etag of the target (optimistic concurrency)
      --evidence stringArray   attachment resource name as evidence (repeatable)
  -h, --help                   help for advance
      --note string            free-text note recorded with the change
      --reason string          reason_key from the project config
      --request-id string      idempotency key, a UUID (default: generated)
      --stage string           stage key, e.g. development
```

### openetl stage regress

```
Set PASSED stages back to OPEN with a reject reason

Usage:
  openetl stage regress WF --stage STAGE [--stage STAGE] --reason REJECT_REASON [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for regress
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --stage strings       stage to set back to OPEN (repeatable)
```

### openetl task assign

```
Attach a task to a workflow, change its membership, or detach (none)

Usage:
  openetl task assign TK --workflow WF [--membership primary|shared|none] [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for assign
      --membership string   primary, shared or none (detach) (default "primary")
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
      --workflow string     workflow to attach to / detach from
```

### openetl task create

```
Create a task (the first --workflow becomes primary)

Usage:
  openetl task create --name NAME [--workflow WF]... [flags]

Flags:
      --annotation stringArray   annotation key=value (repeatable, free text)
      --assignee stringArray     role=account, e.g. dev_owner=alice (repeatable)
      --dry-run                  validate only (validate_only); nothing is written
      --etag string              expected etag of the target (optimistic concurrency)
  -h, --help                     help for create
      --id string                reserved: client-assigned ids are rejected with CLIENT_ID_UNSUPPORTED; ids are server-assigned
      --label stringArray        label key=value (repeatable, filterable)
      --name string              display name
      --note string              free-text note recorded with the change
      --reason string            reason_key from the project config
      --request-id string        idempotency key, a UUID (default: generated)
      --workflow stringArray     workflow to attach to (repeatable)
```

### openetl task delete

```
Soft-delete a task

Usage:
  openetl task delete TK [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for delete
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl task get

```
Get a task

Usage:
  openetl task get TK [flags]

Flags:
  -h, --help   help for get
```

### openetl task list

```
List tasks

Usage:
  openetl task list [--workflow WF] [--filter EXPR] [--page-count N] [flags]

Flags:
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
      --show-deleted        include soft-deleted items
      --workflow string     only tasks attached to this workflow
```

### openetl task restore

```
Restore a soft-deleted task

Usage:
  openetl task restore TK [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for restore
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl task update

```
Update a task (each given field is replaced as a whole)

Usage:
  openetl task update TK [--name N] [--label k=v] [--annotation k=v] [--assignee role=account] [flags]

Flags:
      --annotation stringArray   annotation key=value (repeatable, free text)
      --assignee stringArray     role=account, e.g. dev_owner=alice (repeatable)
      --dry-run                  validate only (validate_only); nothing is written
      --etag string              expected etag of the target (optimistic concurrency)
  -h, --help                     help for update
      --label stringArray        label key=value (repeatable, filterable)
      --name string              display name
      --note string              free-text note recorded with the change
      --reason string            reason_key from the project config
      --request-id string        idempotency key, a UUID (default: generated)
```

### openetl token create

```
Create a token; the secret is printed once and never stored by the CLI

Usage:
  openetl token create [--account ACCOUNT] [--ttl 720h | --expire-time RFC3339] [flags]

Flags:
      --account string       account to mint the token for (default: the caller)
      --dry-run              validate only (validate_only); nothing is written
      --etag string          expected etag of the target (optimistic concurrency)
      --expire-time string   absolute expiry, RFC 3339
  -h, --help                 help for create
      --note string          free-text note recorded with the change
      --reason string        reason_key from the project config
      --request-id string    idempotency key, a UUID (default: generated)
      --ttl duration         token lifetime, e.g. 720h (default: server decides)
```

### openetl token list

```
List tokens of an account (no secrets)

Usage:
  openetl token list [--account ACCOUNT] [--page-count N] [flags]

Flags:
      --account string      account (default: the caller)
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
```

### openetl token revoke

```
Revoke a token (id or accounts/alice/tokens/9)

Usage:
  openetl token revoke TOKEN_ID [--account ACCOUNT] [flags]

Flags:
      --account string      account owning the token (default: from the name, else the caller)
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for revoke
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl views

```
List the views and columns that `openetl query` can read

Usage:
  openetl views [flags]

Flags:
  -h, --help   help for views
```

### openetl workflow create

```
Create a workflow

Usage:
  openetl workflow create --name NAME [--id ID] [--label k=v] [--assignee role=account] [flags]

Flags:
      --annotation stringArray   annotation key=value (repeatable, free text)
      --assignee stringArray     role=account, e.g. dev_owner=alice (repeatable)
      --dry-run                  validate only (validate_only); nothing is written
      --etag string              expected etag of the target (optimistic concurrency)
  -h, --help                     help for create
      --id string                reserved: client-assigned ids are rejected with CLIENT_ID_UNSUPPORTED; ids are server-assigned
      --label stringArray        label key=value (repeatable, filterable)
      --name string              display name
      --note string              free-text note recorded with the change
      --reason string            reason_key from the project config
      --request-id string        idempotency key, a UUID (default: generated)
```

### openetl workflow delete

```
Soft-delete a workflow

Usage:
  openetl workflow delete WF [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for delete
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl workflow get

```
Get a workflow (WF-000123, wf-000123 or full resource name)

Usage:
  openetl workflow get WF [flags]

Flags:
  -h, --help   help for get
```

### openetl workflow list

```
List workflows

Usage:
  openetl workflow list [--filter EXPR] [--page-count N] [flags]

Flags:
      --filter string       AIP-160 filter expression
  -h, --help                help for list
      --order-by string     e.g. 'create_time desc'
      --page-size int       items per page (server default 50, max 1000)
      --page-token string   continue from a next_page_token
      --show-deleted        include soft-deleted items
```

### openetl workflow restore

```
Restore a soft-deleted workflow

Usage:
  openetl workflow restore WF [flags]

Flags:
      --dry-run             validate only (validate_only); nothing is written
      --etag string         expected etag of the target (optimistic concurrency)
  -h, --help                help for restore
      --note string         free-text note recorded with the change
      --reason string       reason_key from the project config
      --request-id string   idempotency key, a UUID (default: generated)
```

### openetl workflow update

```
Update a workflow (each given field is replaced as a whole)

Usage:
  openetl workflow update WF [--name N] [--label k=v] [--annotation k=v] [--assignee role=account] [flags]

Flags:
      --annotation stringArray   annotation key=value (repeatable, free text)
      --assignee stringArray     role=account, e.g. dev_owner=alice (repeatable)
      --dry-run                  validate only (validate_only); nothing is written
      --etag string              expected etag of the target (optimistic concurrency)
  -h, --help                     help for update
      --label stringArray        label key=value (repeatable, filterable)
      --name string              display name
      --note string              free-text note recorded with the change
      --reason string            reason_key from the project config
      --request-id string        idempotency key, a UUID (default: generated)
```
