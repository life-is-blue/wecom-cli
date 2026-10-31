# 0003. State transitions: guard under the row lock, split table vs code

Status: accepted (adapt) · Date: 2026-10-31

## Context

openetl's stage state machine (`open -> pass | na`, regress with reject reason, scope in/out) is validated by `core.Validate`:
the `rule` table (`k_pattern`, `to_value`, `side`, `reason_kind`) decides who may set a value and with which reason kind;
`builtin()` in `internal/core/validate.go` holds stage order, reopen-needs-reject, N/A-needs-reason and merge-cycle checks.

What the sources say:

* Airflow has **no declarative allowed-transition table.** `state.py` only lists values (`TaskInstanceState`, `DagRunState`,
  `TerminalTIState`, `IntermediateTIState`, `State.finished`) (A1, A13). `DagRun.set_state` documents a "State transition
  matrix" but that matrix lists *side effects* (`queued_at`, `start_date`, `end_date`); the only guard is
  `if state not in State.dag_states: raise ValueError(...)` (A2).
* The real guard lives in the Task Execution API, inside the transaction: lock, read `previous_state`, compare, else 409.
  `ti_run`: `.with_for_update(of=TI)`, then `elif previous_state != TaskInstanceState.QUEUED:` raises 409 with
  `{"reason": "invalid_state", "message": "TI was not in a state where it could be marked as running", "previous_state": ...}`.
  `ti_update_state` does the same for `RUNNING` (A4, lines ~211-275, ~632-645).
* A repeated request from the same actor is accepted rather than rejected: with state `RUNNING` and equal
  `(hostname, unixname, pid)` the log says "Duplicate start request received" and the call succeeds (A4, line ~254).
* The scheduler claims work with `with_row_locks(..., skip_locked=True)` (A5): that is a queue pattern, not a guard.
* AIP-134: "state fields must not be directly writable in update methods" (G5); AIP-136: use a custom `:verb` for
  transitions (G6). openetl already uses `:advanceStage`, `:regressStage`, `:setScope`.

openetl today: `applyTx` takes `SELECT id FROM entity ... FOR UPDATE`, loads current values, then validates `Old -> New`
inside the same transaction (`internal/store/apply.go:85-135`). That is the same discipline as Airflow's.

## Options

1. Keep the split: rule table for authorization/reason policy, code for structural invariants; guard under row lock.
2. Move everything, including stage order, into the rule table by adding `from_value` and `requires_prior` columns.
3. Copy Airflow: all transitions in code, drop the rule table.

## Decision

**Adapt: option 1.** The rule table stays the place for what an administrator may change (side, reason kind) because that
policy varies per deployment; Airflow has no equivalent and puts everything in code, which is a worse fit for "rules in a rule
table". Order and cycle invariants stay in code because they reference *other* keys of the entity, which a `(key, value)` row
cannot express; adding `from_value` would not fix that.

Adopt from Airflow:
* every transition is judged against the value read after the row lock, never against client-supplied "old" state (etag is
  the optimistic check, the lock is the real one; already true);
* a failed guard reports the *current* state so an agent can recover without a second read. Existing metadata is partial:
  `STAGE_ORDER_VIOLATION` has `blocking_stage`, `REASON_REQUIRED` has `required_kind`; `NO_RULE_FOR_VALUE` and
  `ETAG_MISMATCH` (`current_etag`) are fine; add `current` (the existing value) to `NO_RULE_FOR_VALUE` and `REASON_REQUIRED`
  when it is cheap;
* a replay of an already-applied identical transition is a no-op success, not a conflict (see 0004).

Add one test, no feature: a table-driven test that enumerates `(from, to, side, reason kind)` for the six stages against the
seed rule table and asserts the outcome. This turns the implicit matrix into a reviewed artifact, which is what Airflow's
docstring matrix tries to be but, being prose, cannot enforce.

## Consequences

* The split is documented: policy in `rule`, invariants in `builtin()`. `docs/security.md` already states the import channel
  skips order validation by design; keep that explicit.
* Risk to watch: `checkRules` lets `admin` bypass the side filter (`in.Actor.Role == RoleAdmin`). Keep, but cover it with a test.
* Not adopted: SKIP LOCKED (no queue), `DagRun`-style side-effect timestamps (events already carry `event_time/create_time`).

## Evidence

A1, A2, A4, A5, A13, G5, G6; `internal/core/validate.go:87-232`, `internal/store/apply.go`, `db/migrations/0001_init.sql` (rule seed).
