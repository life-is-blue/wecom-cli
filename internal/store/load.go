package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// KeyMeta says which event last set a key.
type KeyMeta struct {
	EventID    int64
	EventTime  time.Time
	CreateTime time.Time
}

// Entity is the folded current state of one entity: latest non-null value per key plus metadata.
type Entity struct {
	ID          int64
	ProjectID   int64
	Type        string
	CreateTime  time.Time
	Vals        map[string]json.RawMessage
	Meta        map[string]KeyMeta
	LatestEvent int64 // 0 when no event
	Creator     string
	CreatorSide string
	FirstEvent  time.Time // event_time of the first event
}

// Etag is the opaque concurrency token derived from the latest event id.
func (e *Entity) Etag() string { return EtagOf(e.LatestEvent) }

// EtagOf formats an event id as an etag / event resource id.
func EtagOf(eventID int64) string { return fmt.Sprintf("ev-%d", eventID) }

// loadEntities folds the state of the given entity ids (only those in projectID with type typ).
// asOf, when non-zero, ignores events recorded after it (create_time).
func loadEntities(ctx context.Context, q execer, projectID int64, typ string, ids []int64, asOf time.Time) (map[int64]*Entity, error) {
	out := map[int64]*Entity{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := placeholders(len(ids))
	args := append([]any{projectID, typ}, idArgs(ids)...)
	rows, err := q.QueryContext(ctx, `SELECT id, create_time FROM entity WHERE project_id=? AND type=? AND id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		e := &Entity{ProjectID: projectID, Type: typ, Vals: map[string]json.RawMessage{}, Meta: map[string]KeyMeta{}}
		if err := rows.Scan(&e.ID, &e.CreateTime); err != nil {
			rows.Close()
			return nil, err
		}
		out[e.ID] = e
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}

	asOfCond, asOfArgs := "", []any{}
	if !asOf.IsZero() {
		asOfCond = " AND ev.create_time <= ?"
		asOfArgs = []any{asOf}
	}
	// latest value per (entity,key): the entity_latest cache, or, for time travel, delta itself
	var q1 string
	if asOf.IsZero() {
		q1 = `SELECT l.entity_id, l.k, l.v, l.event_id, ev.event_time, ev.create_time
			FROM entity_latest l JOIN event ev ON ev.id = l.event_id
			WHERE l.entity_id IN (` + ph + `)`
		args = idArgs(ids)
	} else {
		q1 = `SELECT d.entity_id, d.k, d.new_v, d.event_id, ev.event_time, ev.create_time
			FROM delta d JOIN event ev ON ev.id = d.event_id
			WHERE d.entity_id IN (` + ph + `)` + asOfCond + `
			AND d.event_id = (SELECT MAX(d2.event_id) FROM delta d2 JOIN event ev2 ON ev2.id = d2.event_id
				WHERE d2.entity_id = d.entity_id AND d2.k = d.k` + strings.ReplaceAll(asOfCond, "ev.", "ev2.") + `)`
		args = append(idArgs(ids), asOfArgs...)
		args = append(args, asOfArgs...)
	}
	rows, err = q.QueryContext(ctx, q1, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, evID int64
		var k string
		var nv sql.NullString
		var et, ct time.Time
		if err := rows.Scan(&id, &k, &nv, &evID, &et, &ct); err != nil {
			rows.Close()
			return nil, err
		}
		e := out[id]
		if e == nil {
			continue
		}
		if nv.Valid && nv.String != "" && nv.String != "null" {
			e.Vals[k] = json.RawMessage(nv.String)
		}
		e.Meta[k] = KeyMeta{EventID: evID, EventTime: et, CreateTime: ct}
	}
	rows.Close()

	// first/last event
	q2 := `SELECT ev.entity_id, MAX(ev.id), MIN(ev.id) FROM event ev WHERE ev.entity_id IN (` + ph + `)` + asOfCond + ` GROUP BY ev.entity_id`
	rows, err = q.QueryContext(ctx, q2, append(idArgs(ids), asOfArgs...)...)
	if err != nil {
		return nil, err
	}
	firsts := map[int64]int64{}
	for rows.Next() {
		var id, mx, mn int64
		if err := rows.Scan(&id, &mx, &mn); err != nil {
			rows.Close()
			return nil, err
		}
		if e := out[id]; e != nil {
			e.LatestEvent = mx
			firsts[id] = mn
		}
	}
	rows.Close()
	if len(firsts) > 0 {
		var fids []int64
		for _, v := range firsts {
			fids = append(fids, v)
		}
		rows, err = q.QueryContext(ctx, `SELECT entity_id, actor, side, event_time FROM event WHERE id IN (`+placeholders(len(fids))+`)`, idArgs(fids)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var actor, side string
			var et time.Time
			if err := rows.Scan(&id, &actor, &side, &et); err != nil {
				rows.Close()
				return nil, err
			}
			if e := out[id]; e != nil {
				e.Creator, e.CreatorSide, e.FirstEvent = actor, side, et
			}
		}
		rows.Close()
	}
	if !asOf.IsZero() {
		// entities not yet existing at as_of have no events
		for id, e := range out {
			if e.LatestEvent == 0 {
				delete(out, id)
			}
		}
	}
	return out, nil
}

func loadEntity(ctx context.Context, q execer, projectID int64, typ string, id int64) (*Entity, error) {
	m, err := loadEntities(ctx, q, projectID, typ, []int64{id}, time.Time{})
	if err != nil {
		return nil, err
	}
	e := m[id]
	if e == nil {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "%s not found", typ)
	}
	return e, nil
}

// ---- small helpers over Entity values

func (e *Entity) str(k string) string { s, _ := core.Str(e.Vals[k]); return s }

func (e *Entity) deleted() bool { return core.Bool(e.Vals["deleted"]) }

func (e *Entity) deleteTime() *time.Time {
	if !e.deleted() {
		return nil
	}
	t := e.Meta["deleted"].CreateTime
	return &t
}

// prefixed returns the string values of keys with the prefix, keyed by the suffix.
func (e *Entity) prefixed(prefix string) map[string]string {
	m := map[string]string{}
	for k, v := range e.Vals {
		if strings.HasPrefix(k, prefix) {
			m[strings.TrimPrefix(k, prefix)] = core.Scalar(v)
		}
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// derived workflow state from the views.
type wfDerived struct {
	CurrentStage string
	Health       string // ok|at_risk|blocked
}

func loadDerived(ctx context.Context, q execer, ids []int64) (map[int64]wfDerived, error) {
	out := map[int64]wfDerived{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := placeholders(len(ids))
	rows, err := q.QueryContext(ctx, `SELECT workflow_id, current_stage FROM v_current_stage WHERE workflow_id IN (`+ph+`)`, idArgs(ids)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var cs sql.NullString
		if err := rows.Scan(&id, &cs); err != nil {
			rows.Close()
			return nil, err
		}
		d := out[id]
		d.CurrentStage = cs.String
		out[id] = d
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT workflow_id, open_issues, open_risks FROM v_open_items WHERE workflow_id IN (`+ph+`)`, idArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var oi, or sql.NullInt64
		if err := rows.Scan(&id, &oi, &or); err != nil {
			return nil, err
		}
		d := out[id]
		switch {
		case oi.Int64 > 0:
			d.Health = "blocked"
		case or.Int64 > 0:
			d.Health = "at_risk"
		}
		out[id] = d
	}
	return out, rows.Err()
}

var errNoRows = sql.ErrNoRows

func isNoRows(err error) bool { return errors.Is(err, errNoRows) }
