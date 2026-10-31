package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// PlannedChange is the new value of one key; nil or JSON null removes the key.
type PlannedChange struct {
	Key string
	New json.RawMessage
}

// ChangeRequest is the single write primitive input: one event on one entity.
type ChangeRequest struct {
	ProjectID  int64
	EntityType string // workflow|task|comment|attachment
	EntityID   int64  // 0 creates a new entity
	Actor      core.Actor
	ReasonKey  string
	Note       string
	RequestID  string // idempotency key
	Etag       string // expected etag; "" skips the check
	// ValidateOnly runs everything, then rolls back.
	ValidateOnly  bool
	EventTime     time.Time // zero = now
	CorrectsEvent int64
	// Fingerprint identifies the request payload (hash, caller-computed). With RequestID it makes a
	// reused request_id with a different payload fail instead of silently replaying the first result.
	Fingerprint string
	// Evidence are validated attachment names (Store.ResolveEvidence). Needed when an admin sets a
	// key the rule table reserves for the other side; Validate then records the override on the event.
	Evidence []string
	// Plan turns the current state into the key changes. It runs inside the transaction after the
	// entity row is locked, so it sees the latest committed values.
	Plan func(cur *Entity, env core.Env) ([]PlannedChange, error)
}

// Result of Apply.
type Result struct {
	EntityID int64 // 0 for a validate_only create
	EventID  int64 // 0 when nothing changed or validate_only
	Changes  []core.Change
	Replayed bool // an earlier request with the same request_id was returned
	NoOp     bool // no key changed, no event written
	After    *Entity
}

var errRetryReplay = errors.New("retry as replay")

// Apply runs one write in one transaction.
func (s *Store) Apply(ctx context.Context, req ChangeRequest) (*Result, error) {
	for attempt := 0; attempt < 4; attempt++ {
		res, err := s.apply(ctx, req)
		if errors.Is(err, errRetryReplay) || isDeadlock(err) {
			// a concurrent identical request_id won the race (second pass takes the replay path), or this
			// transaction was the deadlock victim of two merges locking each other's chains (second pass
			// sees the other merge and answers MERGE_CYCLE)
			continue
		}
		return res, internalErr(err)
	}
	return nil, internalErr(errors.New("idempotency retry exhausted"))
}

func (s *Store) apply(ctx context.Context, req ChangeRequest) (res *Result, err error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err = s.applyTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if req.ValidateOnly {
		return res, nil // deferred Rollback discards everything
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// applyTx is the body of Apply on a caller-owned transaction (used by import jobs to write many
// entities atomically). It never commits; with ValidateOnly the caller must roll back.
func (s *Store) applyTx(ctx context.Context, tx *sql.Tx, req ChangeRequest) (res *Result, err error) {
	// 1. lock the entity row
	if req.EntityID != 0 {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM entity WHERE id=? AND project_id=? AND type=? FOR UPDATE`,
			req.EntityID, req.ProjectID, req.EntityType).Scan(&id)
		if isNoRows(err) {
			return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "%s not found", req.EntityType)
		}
		if err != nil {
			return nil, err
		}
	}

	// 2. idempotency
	if req.RequestID != "" {
		var evID, entID int64
		var storedFP sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, entity_id, idem_fp FROM event WHERE project_id=? AND idempotency_key=?`,
			req.ProjectID, req.RequestID).Scan(&evID, &entID, &storedFP)
		if err == nil {
			if req.EntityID != 0 && entID != req.EntityID {
				return nil, core.NewError(core.StatusAlreadyExists, "REQUEST_ID_REUSED", "request_id was already used for a different request")
			}
			if req.Fingerprint != "" && storedFP.Valid && storedFP.String != req.Fingerprint {
				return nil, core.NewError(core.StatusAlreadyExists, "IDEMPOTENCY_KEY_REUSED", "request_id was already used with a different payload")
			}
			changes, err := loadEventChanges(ctx, tx, evID)
			if err != nil {
				return nil, err
			}
			ent, err := loadEntity(ctx, tx, req.ProjectID, req.EntityType, entID)
			if isNoRows(err) {
				// the key belongs to an entity of another type/project: a different request
				return nil, core.NewError(core.StatusAlreadyExists, "REQUEST_ID_REUSED", "request_id was already used for a different request")
			}
			if err != nil {
				return nil, err // a real failure must not be reported as the client's mistake
			}
			return &Result{EntityID: entID, EventID: evID, Changes: changes, Replayed: true, After: ent}, nil
		}
		if !isNoRows(err) {
			return nil, err
		}
	}

	now := s.now()
	cur := &Entity{ProjectID: req.ProjectID, Type: req.EntityType, Vals: map[string]json.RawMessage{}, Meta: map[string]KeyMeta{}}
	entityID := req.EntityID
	if entityID == 0 {
		r, err := tx.ExecContext(ctx, `INSERT INTO entity(project_id, type, create_time) VALUES(?,?,?)`, req.ProjectID, req.EntityType, now)
		if err != nil {
			return nil, err
		}
		entityID, _ = r.LastInsertId()
		cur.ID, cur.CreateTime = entityID, now
	} else {
		cur, err = loadEntity(ctx, tx, req.ProjectID, req.EntityType, entityID)
		if err != nil {
			return nil, err
		}
		// 3. optimistic concurrency
		if req.Etag != "" && req.Etag != cur.Etag() {
			return nil, core.NewError(core.StatusAborted, "ETAG_MISMATCH", "the %s changed since the supplied etag was read", req.EntityType).
				With("current_etag", cur.Etag())
		}
	}

	cfg, err := loadConfig(ctx, tx)
	if err != nil {
		return nil, err
	}
	env := &txEnv{ctx: ctx, q: tx, project: req.ProjectID}

	// 4. plan, fill old values, drop no-ops
	planned, err := req.Plan(cur, env)
	if err != nil {
		return nil, err
	}
	if req.EntityType == "task" {
		planned = dropStaleCascade(cur, planned)
	}
	seen := map[string]int{}
	var changes []core.Change
	for _, p := range planned {
		nv := normJSON(p.New)
		old := cur.Vals[p.Key]
		if i, ok := seen[p.Key]; ok { // last write of a key wins
			changes[i].New = nv
			continue
		}
		seen[p.Key] = len(changes)
		changes = append(changes, core.Change{Key: p.Key, Old: old, New: nv})
	}
	kept := changes[:0]
	for _, c := range changes {
		if !sameJSON(c.Old, c.New) {
			kept = append(kept, c)
		}
	}
	changes = kept
	if len(changes) == 0 {
		return &Result{EntityID: entityID, NoOp: true, After: cur}, nil
	}

	// 5. validation (rules + built-ins)
	overrides, err := core.Validate(core.Input{Cfg: cfg, Actor: req.Actor, EntityTyp: req.EntityType, EntityID: req.EntityID,
		ReasonKey: req.ReasonKey, Note: req.Note, Current: cur.Vals, Changes: changes, Env: env, Evidence: req.Evidence})
	if err != nil {
		return nil, err
	}
	if env.err != nil {
		return nil, env.err
	}
	// an override record rides on the event even when its value equals the previous one: it states
	// that THIS event was made for the other side (the no-op rule above does not apply to it)
	changes = append(changes, overrides...)

	// 6. write event + deltas
	eventTime := req.EventTime.UTC()
	if req.EventTime.IsZero() {
		eventTime = now
	} else if eventTime.After(now) {
		return nil, core.NewError(core.StatusInvalidArgument, "EVENT_TIME_IN_FUTURE", "event_time must not be later than the recording time")
	}
	var reasonKey, note, idem sql.NullString
	if req.ReasonKey != "" {
		reasonKey = sql.NullString{String: req.ReasonKey, Valid: true}
	}
	if req.Note != "" {
		note = sql.NullString{String: req.Note, Valid: true}
	}
	if req.RequestID != "" {
		idem = sql.NullString{String: req.RequestID, Valid: true}
	}
	var fp sql.NullString
	if req.RequestID != "" && req.Fingerprint != "" {
		fp = sql.NullString{String: req.Fingerprint, Valid: true}
	}
	var corrects sql.NullInt64
	if req.CorrectsEvent != 0 {
		corrects = sql.NullInt64{Int64: req.CorrectsEvent, Valid: true}
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO event(project_id, entity_id, actor, side, channel, reason_key, note, idempotency_key,
		corrects_event_id, event_time, create_time, idem_fp) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.ProjectID, entityID, req.Actor.Account, req.Actor.Side, req.Actor.Channel, reasonKey, note, idem, corrects, eventTime, now, fp)
	if err != nil {
		if isDup(err) && req.RequestID != "" {
			return nil, errRetryReplay
		}
		return nil, err
	}
	eventID, _ := r.LastInsertId()
	var latest []latestRow
	for _, c := range changes {
		var ov, nv sql.NullString
		if len(c.Old) > 0 {
			ov = sql.NullString{String: string(c.Old), Valid: true}
		}
		if len(c.New) > 0 {
			nv = sql.NullString{String: string(c.New), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delta(event_id, entity_id, k, old_v, new_v) VALUES(?,?,?,?,?)`,
			eventID, entityID, c.Key, ov, nv); err != nil {
			return nil, err
		}
		latest = append(latest, latestRow{EventID: eventID, EntityID: entityID, Key: c.Key, New: nv})
	}
	// entity_latest is a cache of delta: it moves in the same transaction (rolled back with it)
	if err := upsertLatest(ctx, tx, latest); err != nil {
		return nil, err
	}

	// 6b. a workflow delete/undelete takes its tasks along, in the same transaction
	for _, c := range changes {
		if req.EntityType == "workflow" && c.Key == "deleted" {
			if err := s.cascadeTasks(ctx, tx, req, entityID, eventID, eventTime, core.Bool(c.New)); err != nil {
				return nil, err
			}
		}
	}

	// 7. resulting state
	after := &Entity{ID: entityID, ProjectID: cur.ProjectID, Type: cur.Type, CreateTime: cur.CreateTime, Vals: map[string]json.RawMessage{},
		Meta: map[string]KeyMeta{}, LatestEvent: eventID, Creator: cur.Creator, CreatorSide: cur.CreatorSide, FirstEvent: cur.FirstEvent}
	for k, v := range cur.Vals {
		after.Vals[k] = v
	}
	for k, m := range cur.Meta {
		after.Meta[k] = m
	}
	if after.Creator == "" {
		after.Creator, after.CreatorSide, after.FirstEvent = req.Actor.Account, req.Actor.Side, eventTime
	}
	for _, c := range changes {
		if len(c.New) == 0 {
			delete(after.Vals, c.Key)
		} else {
			after.Vals[c.Key] = c.New
		}
		after.Meta[c.Key] = KeyMeta{EventID: eventID, EventTime: eventTime, CreateTime: now}
	}

	if req.ValidateOnly {
		out := &Result{EntityID: req.EntityID, Changes: changes, After: after}
		if req.EntityID != 0 {
			out.After.LatestEvent = cur.LatestEvent // nothing was recorded
		} else {
			out.After.ID, out.After.LatestEvent = 0, 0
		}
		return out, nil // caller rolls back
	}
	return &Result{EntityID: entityID, EventID: eventID, Changes: changes, After: after}, nil
}

func loadEventChanges(ctx context.Context, q execer, eventID int64) ([]core.Change, error) {
	rows, err := q.QueryContext(ctx, `SELECT k, old_v, new_v FROM delta WHERE event_id=? ORDER BY k`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Change
	for rows.Next() {
		var k string
		var ov, nv sql.NullString
		if err := rows.Scan(&k, &ov, &nv); err != nil {
			return nil, err
		}
		c := core.Change{Key: k}
		if ov.Valid {
			c.Old = json.RawMessage(ov.String)
		}
		if nv.Valid {
			c.New = json.RawMessage(nv.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func normJSON(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(t, &v) != nil {
		return t
	}
	b, _ := json.Marshal(v)
	return b
}

func sameJSON(a, b json.RawMessage) bool {
	return bytes.Equal(normJSON(a), normJSON(b))
}

// txEnv implements core.Env on a transaction.
type txEnv struct {
	ctx     context.Context
	q       execer
	project int64
	err     error
}

func (e *txEnv) latest(id int64, key string) (json.RawMessage, error) {
	var nv sql.NullString
	err := e.q.QueryRowContext(e.ctx, `SELECT v FROM entity_latest WHERE entity_id=? AND k=?`, id, key).Scan(&nv)
	if isNoRows(err) || (err == nil && !nv.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(nv.String), nil
}

// lock takes the row lock of a workflow whose merge state is about to be read. Merge validation
// reads other workflows, so without it two concurrent merges (A into B, B into A) both pass the
// cycle check and both commit. Writers always lock their own entity row first, so a locked chain
// cannot change under the check; two merges that lock each other end in a deadlock and one retries.
func (e *txEnv) lock(id int64) error {
	var x int64
	err := e.q.QueryRowContext(e.ctx, `SELECT id FROM entity WHERE id=? AND project_id=? FOR UPDATE`, id, e.project).Scan(&x)
	if isNoRows(err) {
		return nil
	}
	return err
}

func (e *txEnv) WorkflowExists(id int64) (bool, error) {
	if err := e.lock(id); err != nil {
		e.err = err
		return false, err
	}
	var n int
	if err := e.q.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM entity WHERE id=? AND project_id=? AND type='workflow'`, id, e.project).Scan(&n); err != nil {
		e.err = err
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	d, err := e.latest(id, "deleted")
	if err != nil {
		e.err = err
		return false, err
	}
	return !core.Bool(d), nil
}

func (e *txEnv) MergedInto(id int64) (int64, bool, error) {
	if err := e.lock(id); err != nil {
		e.err = err
		return 0, false, err
	}
	sc, err := e.latest(id, "scope")
	if err != nil {
		e.err = err
		return 0, false, err
	}
	if s, _ := core.Str(sc); s != "out" {
		return 0, false, nil
	}
	in, err := e.latest(id, "into")
	if err != nil {
		e.err = err
		return 0, false, err
	}
	var target int64
	if len(in) == 0 || json.Unmarshal(in, &target) != nil || target == 0 {
		return 0, false, nil
	}
	return target, true, nil
}
