package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// eventScope restricts events to a workflow and the comments/attachments attached to it.
const eventScope = `ev.project_id = ? AND (ev.entity_id = ? OR ev.entity_id IN
	(SELECT l.entity_id FROM entity_latest l WHERE l.k = 'on' AND JSON_UNQUOTE(l.v) = ?))`

// ListEvents lists audit events of a workflow. o.AsOf bounds by recording time (create_time).
func (s *Store) ListEvents(ctx context.Context, p Project, wf int64, o ListOpts) ([]api.Event, string, error) {
	lq := &listQuery{
		from:  "event ev",
		idCol: "ev.id",
		where: []string{eventScope},
		args:  []any{p.ID, wf, strconv.FormatInt(wf, 10)},
		orders: map[string]orderSpec{
			"create_time": {expr: "ev.id", byID: true},
			"name":        {expr: "ev.id", byID: true},
			"id":          {expr: "ev.id", byID: true},
			"event_time":  {expr: "ev.event_time"},
		},
	}
	if !o.AsOf.IsZero() {
		lq.where = append(lq.where, "ev.create_time <= ?")
		lq.args = append(lq.args, o.AsOf.UTC())
	}
	res := func(field, op, v string) (string, []any, error) {
		switch field {
		case "actor":
			q, err := cmpSQL("ev.actor", op)
			return q, []any{AccountID(v)}, err
		case "side":
			m, err := enumMap(v, map[string]string{"DELIVERY": "delivery", "CUSTOMER": "customer", "SYSTEM": "system"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("ev.side", op)
			return q, []any{m}, err
		case "channel":
			q, err := cmpSQL("ev.channel", op)
			return q, []any{strings.ToLower(v)}, err
		case "reason":
			q, err := cmpSQL("ev.reason_key", op)
			return q, []any{v}, err
		case "event_time", "create_time":
			t, err := parseTime(v)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("ev."+field, op)
			return q, []any{t}, err
		}
		return "", nil, badFilter("unknown filter field %q", field)
	}
	fsql, fargs, err := compileFilter(o.Filter, res)
	if err != nil {
		return nil, "", err
	}
	if fsql != "" {
		lq.where = append(lq.where, fsql)
		lq.args = append(lq.args, fargs...)
	}
	ids, next, err := lq.run(ctx, s.DB, o)
	if err != nil {
		return nil, "", internalErr(err)
	}
	evs, err := s.renderEvents(ctx, p, wf, ids)
	if err != nil {
		return nil, "", internalErr(err)
	}
	return evs, next, nil
}

// GetEvent reads one event belonging to the workflow's scope.
func (s *Store) GetEvent(ctx context.Context, p Project, wf, id int64) (*api.Event, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event ev WHERE ev.id = ? AND `+eventScope,
		id, p.ID, wf, strconv.FormatInt(wf, 10)).Scan(&n)
	if err != nil {
		return nil, internalErr(err)
	}
	if n == 0 {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "event not found")
	}
	evs, err := s.renderEvents(ctx, p, wf, []int64{id})
	if err != nil || len(evs) == 0 {
		return nil, internalErr(err)
	}
	return &evs[0], nil
}

func (s *Store) renderEvents(ctx context.Context, p Project, wf int64, ids []int64) ([]api.Event, error) {
	if len(ids) == 0 {
		return []api.Event{}, nil
	}
	ph := placeholders(len(ids))
	rows, err := s.DB.QueryContext(ctx, `SELECT ev.id, ev.entity_id, en.type, ev.actor, ev.side, ev.channel, ev.reason_key, ev.note,
		ev.idempotency_key, ev.corrects_event_id, ev.event_time, ev.create_time FROM event ev JOIN entity en ON en.id = ev.entity_id
		WHERE ev.id IN (`+ph+`)`, idArgs(ids)...)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*api.Event{}
	for rows.Next() {
		var id, entID int64
		var typ, actor, side, channel string
		var reason, note, idem sql.NullString
		var corrects sql.NullInt64
		e := api.Event{}
		if err := rows.Scan(&id, &entID, &typ, &actor, &side, &channel, &reason, &note, &idem, &corrects, &e.EventTime, &e.CreateTime); err != nil {
			rows.Close()
			return nil, err
		}
		e.Name = ptr(p.eventName(wf, id))
		e.Uid = ptr(strconv.FormatInt(id, 10))
		switch typ {
		case "workflow":
			e.Target = ptr(p.workflowName(entID))
		case "task":
			e.Target = ptr(p.taskName(entID))
		case "comment":
			e.Target = ptr(p.commentName(wf, entID))
		default:
			e.Target = ptr(p.workflowName(wf) + "/attachments/" + FormatID(PrefixAttach, entID))
		}
		e.Actor = ptr(accountName(actor))
		e.Side = ptr(api.Side(strings.ToUpper(side)))
		e.Channel = ptr(api.EventChannel(strings.ToUpper(channel)))
		if reason.Valid {
			e.Reason = &reason.String
		}
		if note.Valid {
			e.Note = &note.String
		}
		if idem.Valid {
			e.RequestId = &idem.String
		}
		if corrects.Valid {
			e.Corrects = ptr(p.eventName(wf, corrects.Int64))
		}
		byID[id] = &e
	}
	rows.Close()
	drows, err := s.DB.QueryContext(ctx, `SELECT event_id, k, old_v, new_v FROM delta WHERE event_id IN (`+ph+`) ORDER BY event_id, k`, idArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	type chg = struct {
		Key      *string       `json:"key,omitempty"`
		NewValue *api.AnyValue `json:"new_value,omitempty"`
		OldValue *api.AnyValue `json:"old_value,omitempty"`
	}
	changes := map[int64][]chg{}
	for drows.Next() {
		var id int64
		var k string
		var ov, nv sql.NullString
		if err := drows.Scan(&id, &k, &ov, &nv); err != nil {
			return nil, err
		}
		c := chg{Key: ptr(k)}
		c.OldValue = anyVal(ov)
		c.NewValue = anyVal(nv)
		changes[id] = append(changes[id], c)
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}
	out := make([]api.Event, 0, len(ids))
	for _, id := range ids {
		e := byID[id]
		if e == nil {
			continue
		}
		cs := changes[id]
		e.Changes = &cs
		out = append(out, *e)
	}
	return out, nil
}

func anyVal(n sql.NullString) *api.AnyValue {
	var v api.AnyValue
	if !n.Valid {
		return &v
	}
	_ = json.Unmarshal([]byte(n.String), &v)
	return &v
}
