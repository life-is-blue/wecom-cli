# Security model

## Roles and scope

* Authentication is a bearer token (`oetl_...`); only its hash is stored. A token belongs to an **account**; an account has a
  **membership** (role `viewer` / `editor` / `admin`, side `delivery` / `customer`) per project.
* `viewer` reads; `editor` also writes workflows, tasks, comments and attachments; `admin` additionally manages
  members, tokens and config of **its** project. Agent accounts can never be admin.
* Every request is resolved to one project through the caller's membership; storage keys, entities and events all carry the
  project id.

## Members and tokens (cross-project rules)

Tokens are account-wide, so token power equals power over every project of the account.

* `PUT members/{account}`: an admin may create new accounts, edit accounts that are already members of the project, or add an
  existing account only if **all** of its memberships are in projects the caller administers (`ACCOUNT_IN_OTHER_PROJECT`
  otherwise).
* Token create / list / revoke (`/v1/accounts/{a}/tokens`): allowed only if the caller administers **every** project the target
  belongs to (or the target is the caller and the caller is an admin somewhere). An account without memberships cannot be managed.
* The operator tool `openetl-server admin create-token` bypasses these rules by design (it needs database access).

## Query isolation

`POST /v1/projects/{p}:query` is the only path that executes client-supplied SQL.

1. **Allowlist guard** (`internal/store/sqlguard.go`): exactly one `SELECT`; after `FROM`/`JOIN` only a bare public view name with
   an optional alias (no parentheses, schema qualification, subqueries, derived tables, CTEs, `UNION`/`INTERSECT`/`EXCEPT`,
   `TABLE`/`VALUES`, comma joins, index hints); function calls limited to a deterministic allowlist; no comments, `;`, `@`,
   double-quoted strings, backslashes in strings or escaped backticks. Every view reference is replaced by
   `(SELECT * FROM view WHERE project_id = N) AS alias`, case-insensitively. `FuzzGuardQuery` asserts that anything accepted only
   reads project-filtered public views (`go test ./internal/store -fuzz FuzzGuardQuery`).
2. **Read-only transaction**, `page_size` <= 1000, response <= 8 MiB (`RESULT_TOO_LARGE`), 15 s context deadline and a server-side
   statement timeout (MySQL `MAX_EXECUTION_TIME`, MariaDB `max_statement_time`); the query is refused if neither can be set.
3. **Separate database user** (`OPENETL_QUERY_DSN`, `scripts/create-query-user.sql`): `SELECT` on the seven views only. Without
   the variable the main DSN credentials are used and the server logs a warning at startup. Production deployments should set it.
4. **Dedicated, small pool and concurrency limits.** User SQL always runs on its own pool (`QueryPoolSize` = 8 connections), also
   when it reuses the main DSN, so it cannot starve authentication and writes (pool of 20). Concurrent queries are capped per
   account (`OPENETL_QUERY_MAX_PER_ACCOUNT`, default 3) and in total (`OPENETL_QUERY_MAX_TOTAL`, default 6); beyond that the
   request is refused immediately (no queueing) with `429 RESOURCE_EXHAUSTED` (`QUERY_TOO_MANY_CONCURRENT` / `QUERY_SERVER_BUSY`)
   and a `Retry-After` header. The caps are per server process.
5. **Guard extras:** a `JOIN` needs `ON`/`USING` on real columns (no `CROSS`/`NATURAL`/bare `JOIN`, no constant `ON 1`, `ON true`,
   `ON 1=1`, `... OR 1`); system-information words (`current_user`, `session_user`, `system_user`, `user`, `database`, `schema`,
   `version`, `connection_id`, `last_insert_id`, `row_count`, `found_rows`, ...) and `@variables` are rejected, with or without
   parentheses. Database errors are never echoed: `QUERY_FAILED` carries only the numeric database error code for errors caused
   by the SQL text; the raw error goes to the server log.

## Attachments and storage

* Clients upload to a **staging** key (`staging/<project>/<workflow>/<random>`) through a time-limited signed URL (fs: HMAC,
  s3: SigV4). Registration (`POST .../attachments`) makes the **server** hash the staged bytes, compares size and sha256 with
  the request, and commits the object to the immutable final key (fs: hard link, fails if it exists; s3: copy pinned to the
  verified ETag, then delete). The upload URL can therefore no longer overwrite registered evidence. Downloads serve committed
  keys only.
* `storage_key` is unique per project; a replay with the same `request_id` returns the first result, anything else is
  `STORAGE_KEY_REGISTERED`.
* Total registered attachment size per project is capped by `OPENETL_PROJECT_STORAGE_QUOTA_BYTES` (default 10 GiB), checked at
  registration (`429 RESOURCE_EXHAUSTED`, `PROJECT_STORAGE_QUOTA_EXCEEDED`). Concurrent registrations may overshoot it slightly
  (check, then commit).
* Pending uploads per project are capped (`MaxPendingUploads` = 50, `MaxPendingUploadBytes` = 1 GiB, package variables in
  `internal/server/handlers_attachment.go`); staging objects older than 1 h are deleted at startup and hourly.
* Signed URLs are built from `OPENETL_PUBLIC_URL` when set; `X-Forwarded-Proto/Host` are honoured only with `OPENETL_TRUST_PROXY=1`.

## CLI transport

The CLI requires `https` for non-loopback base URLs (`localhost`, `127.0.0.0/8`, `::1` are exempt) unless `OPENETL_ALLOW_HTTP=1`.
The bearer token is sent to presigned upload/download URLs only when scheme, host **and** port equal the base URL, and is
dropped on redirects to another origin. `auth login` stores the verified base URL with the token; a stored token is never sent to
a base URL with another scheme/host/port (`--base-url` / `OPENETL_BASE_URL`): the CLI exits with a usage error asking for
`auth login` for that URL. A token from `OPENETL_TOKEN` is explicit and goes to the effective base URL. Credential files written
by older versions have no saved base URL: they stay usable (unbound) and print a warning suggesting a new login. Stored credentials are encrypted with a key in the OS keyring; see below for the fallback.

## Known limits and decisions

* **S3 commit is not fully atomic and the S3 backend is untested.** `Commit` does a `HEAD` of the final key and then
  `CopyObject`; the copy carries `If-None-Match: *` so S3 refuses to overwrite an existing key, but S3-compatible stores that ignore
  the header leave a narrow race in which a concurrent commit to the same final key could overwrite it. The S3 backend has no
  automated tests (no S3 in CI); only the fs backend is exercised.

* **Config is global.** Stage definitions, reasons, rules and settings have no `project_id`. Phase 1 is one project per instance:
  config `PATCH` is admin-only and is refused (`GLOBAL_CONFIG_SHARED`) while more than one project exists, unless the server
  runs with `OPENETL_ALLOW_GLOBAL_CONFIG=1`, in which case any project admin edits the rules of all projects.
* **Import channel skips stage-order validation by design**; importing is admin-only and every import is a reviewable, rollbackable job.
* **Key-file fallback**: when no OS keyring is available the client keeps the encryption key in a file next to the credentials;
  this adds no protection beyond file permissions (0600).
* The fs backend's signing secret is random per process unless `OPENETL_STORAGE_SECRET` is set (URLs do not survive restarts).
* An existing account whose memberships were all removed can be re-linked by any admin (it has no remaining project to protect);
  revoke its tokens when removing the last membership.
* Unregistered staging blobs live up to one hour; a client that fails after commit but before the event is written triggers a
  best-effort delete of the committed blob.
