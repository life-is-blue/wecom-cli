# Threat model (STRIDE)

Scope: the foundation as it stands (server, CLI, MySQL metadata store, blob store, import channel). Companion pages:
`docs/security.md` (controls and known limits), `docs/contract-checklist.md` (API contract). Mitigations below were
**re-exercised against a running server on 2026-10-31** with `scripts/contract_attacks.py` (run by
`scripts/check-contract.sh`); "verified" refers to that run, "by code" to reading only.

## 1. Assets

| Asset | Why it matters | Where it lives |
| --- | --- | --- |
| API tokens | account-wide bearer credentials (token power = power over every project of the account) | client: encrypted file + OS keyring key; server: `api_token.token_hash` (sha256, plaintext shown once) |
| Evidence blobs | customer sign-off evidence; tampering would falsify acceptance | blob store (fs dir or S3), keys `<project>/<workflow>/<random>`, committed objects immutable |
| Event log integrity | the audit trail is the source of truth (`delta`, `event`); views and `entity_latest` derive from it | MySQL `event`, `delta` (append-only by application convention) |
| Project data | workflows, tasks, comments, config (rules, settings may reference secrets) | MySQL, every row carries `project_id` |
| Source-system credentials | none stored: imports read files supplied by the operator; `setting` holds secret *references* only | n/a |

## 2. Trust boundaries

```
 Agent / human --CLI--(1)--> server --(2)--> MySQL (main DSN, query pool / optional SELECT-only user)
                                  \--(3)--> blob store (fs / S3), signed URLs handed to the client
 operator --(4)--> openetl-server admin / import files (source DB export; no live source connection)
```

1. CLI to server: bearer token over HTTP(S); the CLI is untrusted from the server's view (anyone with a token can
   speak the API directly). TLS is terminated outside the server (no TLS in `openetl-server`).
2. Server to MySQL: the server is trusted; client-supplied SQL crosses this boundary only through `:query`.
3. Server to blob store and client to blob store: the client PUTs/GETs bytes through signed URLs; the signature is
   the credential.
4. Operator tooling: has database access by definition (`admin create-token` bypasses API rules).
   Import data comes from files, not from a live source database, so there is no outbound connection to a source DB.

## 3. Threats

Each row: threat, mitigation present (file), residual risk, backlog item (`docs/backlog/C.md`).

### Spoofing

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Stolen or guessed token | 192-bit random tokens, only the sha256 stored (`internal/store/auth.go` `HashToken`), expiry (default 1 y, max 10 y, `handlers_admin.go`), revocation, secret never logged, `Cache-Control: no-store` on creation | no rate limiting on failed auth; tokens are account-wide; a leaked client file plus keyring is enough | C-1, C-12 |
| CLI sends the saved token to an attacker-chosen URL (`--base-url`, redirect) | token bound to the verified base URL, other scheme/host/port refused with exit 2; explicit `OPENETL_TOKEN` goes to the effective URL only; presigned URLs get the token only on same origin and redirects to another origin drop it (`internal/cli/client.go` `sameBase`, `sameOrigin`). **Verified** (A5a: refused, the other host received 0 requests; A5b: with an explicit env token the first hop gets the token by design, a cross-origin redirect target gets none; A5c: `localhost` vs `127.0.0.1` refused) | env-supplied tokens are explicit and unbound; credential files from older versions are unbound (warning) | none |
| Forged identity header | identity comes only from the bearer token. `X-Openetl-Channel` is client-asserted but only selects `cli` vs `api` in the event | audit `channel` can be spoofed | C-10 |
| Forged signed blob URL | HMAC over method, key, expiry, size, sha, filename (`internal/storage/fs.go` `sign`); secret random per process unless `OPENETL_STORAGE_SECRET` | secret rotation not supported | none |

### Tampering

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Overwrite registered evidence via the upload URL | upload goes to a staging key; registration hashes the staged bytes server-side and commits to an immutable final key (hard link, fails if it exists) (`handlers_attachment.go` `createAttachment`, `fs.go` `Commit`). **Verified** (A4): after registering, a second PUT to the same URL is rejected (400), committed bytes are unchanged, and re-registering the same `storage_key` with other bytes is 409 `STORAGE_KEY_REGISTERED` | S3 commit is not atomic against S3 stores that ignore `If-None-Match` and the S3 backend has no automated tests (known limit) | C-4 |
| Rewrite history in MySQL | application never updates `event`/`delta`; corrections are new events; `admin verify-latest` detects drift between `delta` and `entity_latest` | **no database-level protection**: no triggers, no revoked UPDATE/DELETE, no hash chain; anyone with DB write access can alter events undetected | C-2 |
| Cross-project write through members/tokens | `PUT members` and token create/list/revoke require admin in every project of the target (`store/admin.go` `SetMember`, `CanManageTokens`). **Verified** (A3): admin of `pa` creating/listing/revoking tokens of an admin of `pb`, or adding that account to `pa`: 403 (`ROLE_INSUFFICIENT`, `ACCOUNT_IN_OTHER_PROJECT`); the victim's memberships unchanged | an account with all memberships removed can be re-linked by any admin (known limit) | none |
| SQL injection through `:query` | allowlist guard + project filter rewrite (`store/sqlguard.go`), read-only transaction, optional SELECT-only DB user. **Verified** (A1c): 14 bypass attempts (`project_id` predicate, `OR`, `UNION`, subquery, schema-qualified names, `api_token`, `information_schema`, `/*!50000 */` comments, `;` stacking, `@@version`, `INTO OUTFILE`, CTE): 12 rejected, 2 accepted (`WHERE project_id = 1`, `... OR project_id <> 0`) but they run inside the project-filtered derived table and returned only own rows | guard is a hand-written tokenizer (fuzzed, `FuzzGuardQuery`); the main DSN is used unless `OPENETL_QUERY_DSN` is set (startup warning) | C-3 |
| Parenthesised / multi-table `FROM` to escape the filter | rejected by the guard. **Verified** (A2): `(workflows)`, `(workflows) w`, comma join, parenthesised join, `JOIN (tasks)`, `CROSS JOIN`, `ON 1=1`, `ON 1`, `NATURAL JOIN` all 400; a legitimate `JOIN ... ON w.id = t.primary_workflow_id` still works | | none |
| Replay or reuse of an idempotency key | key bound to a request fingerprint: same key, other body is 409 `IDEMPOTENCY_KEY_REUSED` (migration 0005, verified in the contract pass) | | none |

### Repudiation

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Actor denies a change | every write is an event with actor, side, channel, reason, note, request id; imports are events too and roll back by correcting events | integrity of the log is application-level only (see tampering); access logs are optional (`OPENETL_ACCESS_LOG`) | C-2 |
| Admin actions leave no trail | member/token/config changes are logged to the server log (`handlers_admin.go`), never with secrets | not in the event log, so no tamper evidence; server logs are not shipped anywhere by default | C-11 |

### Information disclosure

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Other-project read | every request resolves the project through the caller's membership first (`server.go` `route`: 403 `NOT_A_MEMBER` even for nonexistent projects, so existence does not leak); every query view is wrapped in `project_id = N`. **Verified** (A1a/A1b): a `pb` admin sees only `pb` rows; `POST /v1/projects/pa:query` is 403 | | none |
| Config `settings` (may hold secret references) read by non-admins | stripped for non-admins (`server.go` config branch; verified in the contract pass: `settings` is null for an editor) | | none |
| Token or secret in logs / errors | create-token response is `no-store`, secret never logged; database errors never echoed (`QUERY_FAILED` carries a numeric code only); 500s return `internal error` | access log records routes and account names | none |
| Blob URLs leaked | short TTL (upload 15 min, download 5 min), staging objects not downloadable (`fs.go`), downloads only for committed keys | anyone holding a URL can use it until expiry; with no `OPENETL_PUBLIC_URL` the host in signed URLs comes from the `Host` header | C-4 |
| Plaintext blobs at rest | none by the server | fs directory permissions / S3 server-side encryption are the operator's job | C-4 |
| Traffic sniffing | CLI refuses non-loopback `http://` unless `OPENETL_ALLOW_HTTP=1` | the server itself speaks plain HTTP; TLS must be terminated in front | none |

### Denial of service

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Expensive user SQL | separate 8-connection pool, per-account (3) and global (6) concurrency caps with immediate 429 + `Retry-After`, 15 s deadline, server-side statement timeout, 8 MiB response cap, page_size <= 1000 (`store/query.go`) | caps are per process | none |
| Upload flooding | per-project pending upload caps (50 / 1 GiB), 100 MiB per object, project quota (default 10 GiB) checked at registration, staging cleanup after 1 h | quota can overshoot slightly under concurrency; reservations are per process | C-4 |
| Request flooding | 4 MiB body cap (`maxBody`), read/write/header timeouts on the HTTP server | no global or per-token rate limit; auth failures hit MySQL | C-1 |
| Unbounded import | import is admin/editor only, rows staged in one request (4 MiB body) | | none |

### Elevation of privilege

| Threat | Mitigation (file) | Residual | Backlog |
| --- | --- | --- | --- |
| Editor acts as admin | role gate in `server.go` (viewers cannot write, only admins reach `members`/`config`, `requireRole` for import apply); verified in the contract pass (editor `applyImportJob` 403, editor `createToken` 403, self-promotion `setMember` 403) | | none |
| Agent account becomes admin | agents can never be admin (`store/admin.go` `SetMember`) | | none |
| Customer side advances delivery stages | rule table with side checks (`SIDE_NOT_ALLOWED`), admins may act for either side by design | an admin can act as either party | none |
| Import channel skips stage-order validation | by design, admin-only, reviewable and rollbackable | | none |
| Cross-project escalation via shared account | see tampering row (A3) | | none |

## 4. Verification record (2026-10-31)

Run: `scripts/check-contract.sh` (builds, recreates database `openetl_c`, starts a server on `127.0.0.1:18203`, then runs
`scripts/contract_conformance.py` and `scripts/contract_attacks.py`). Output of the attack phase:

```
ATTACK A1a PASS - dave queries pb: ['OWN-PB']
ATTACK A1b PASS - dave queries projects/pa -> 403 NOT_A_MEMBER
ATTACK A1c PASS - 14 bypass attempts; 12 rejected, 2 accepted but project-filtered (...); rows of project pa leaked: []
ATTACK A2 PASS - statuses=[400 x9] for 9 statements
ATTACK A2-control PASS - legitimate join still works -> 200
ATTACK A3 PASS - alice(admin pa) on dave(admin pb): create token / list tokens / add to pa / revoke -> [403, 403, 403, 403] ['ROLE_INSUFFICIENT', 'ROLE_INSUFFICIENT', 'ACCOUNT_IN_OTHER_PROJECT', 'ROLE_INSUFFICIENT']
ATTACK A3-control PASS - dave memberships unchanged: ['projects/pb/members/dave']
ATTACK A4 PASS - first PUT 200, register 200, replayed PUT 400, committed bytes unchanged=True, re-register tampered bytes -> 409 STORAGE_KEY_REGISTERED
ATTACK A5a PASS - login rc=0; `me --base-url <other host>` rc=2, requests received by the other host=0
ATTACK A5b PASS - explicit OPENETL_TOKEN: first hop got Authorization=yes (by design), cross-origin redirect target got [None]
ATTACK A5c PASS - stored token to http://localhost:18203 (same port, other host name) rc=2
```

Manual equivalents (token in `$T`, project `pb` admin):

```bash
curl -s -X POST localhost:18203/v1/projects/pa:query -H "Authorization: Bearer $T" -d '{"sql":"SELECT name FROM workflows"}'   # 403 NOT_A_MEMBER
curl -s -X POST localhost:18203/v1/projects/pb:query -H "Authorization: Bearer $T" -d '{"sql":"SELECT name FROM (workflows)"}'  # 400
curl -s -X POST localhost:18203/v1/accounts/dave/tokens -H "Authorization: Bearer $ALICE" -d '{}'                              # 403
HOME=$(mktemp -d) openetl auth login --token-stdin --base-url http://127.0.0.1:18203 <<<"$T"
openetl me --base-url http://127.0.0.1:9                                                                                        # exit 2: "the saved token belongs to ..."
```

What was **not** tested live: S3 backend, TLS, the SELECT-only query user (the run used the main DSN, so the second
defence layer was not in play), multi-process concurrency caps.
