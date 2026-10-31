package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// RenderComment maps folded state to the API Comment.
func RenderComment(p Project, e *Entity) api.Comment {
	id := strconv.FormatInt(e.ID, 10)
	wf, _ := strconv.ParseInt(core.Scalar(e.Vals["on"]), 10, 64)
	c := api.Comment{
		Kind:       api.CommentKind(strings.ToUpper(e.str("kind"))),
		Body:       e.str("body"),
		Uid:        &id,
		CreateTime: ptr(e.CreateTime),
		Resolved:   ptr(core.Bool(e.Vals["resolved"])),
	}
	if e.ID != 0 {
		c.Name = ptr(p.commentName(wf, e.ID))
	}
	if e.LatestEvent != 0 {
		c.Etag = ptr(e.Etag())
	}
	if e.Creator != "" {
		c.Creator = ptr(accountName(e.Creator))
		c.Side = ptr(api.Side(strings.ToUpper(e.CreatorSide)))
	}
	if !e.FirstEvent.IsZero() {
		c.EventTime = ptr(e.FirstEvent)
	}
	if core.Bool(e.Vals["resolved"]) {
		if m, ok := e.Meta["resolved"]; ok {
			c.ResolveTime = ptr(m.EventTime)
		}
	}
	if raw, ok := e.Vals["attachments"]; ok {
		var at []string
		if jsonUnmarshal(raw, &at) == nil && len(at) > 0 {
			c.Attachments = &at
		}
	}
	return c
}

// PreviewComment renders a validate_only comment result.
func (s *Store) PreviewComment(p Project, e *Entity) *api.Comment {
	c := RenderComment(p, e)
	return &c
}

// GetComment reads one live comment of a workflow.
func (s *Store) GetComment(ctx context.Context, p Project, wf, id int64) (*api.Comment, error) {
	e, err := loadEntity(ctx, s.DB, p.ID, "comment", id)
	if err != nil {
		return nil, internalErr(err)
	}
	if core.Scalar(e.Vals["on"]) != strconv.FormatInt(wf, 10) || e.deleted() {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "comment not found")
	}
	c := RenderComment(p, e)
	return &c, nil
}

// ListComments lists the live comments of a workflow.
func (s *Store) ListComments(ctx context.Context, p Project, wf int64, o ListOpts) ([]api.Comment, string, error) {
	lq := &listQuery{
		from:  "v_comment c",
		idCol: "c.id",
		where: []string{"c.project_id = ?", "c.workflow_id = ?", "NOT c.deleted"},
		args:  []any{p.ID, wf},
		orders: map[string]orderSpec{
			"create_time": {expr: "c.id", byID: true},
			"name":        {expr: "c.id", byID: true},
			"id":          {expr: "c.id", byID: true},
		},
	}
	first := func(col string) string {
		return "(SELECT ev." + col + " FROM event ev WHERE ev.entity_id = c.id ORDER BY ev.id LIMIT 1)"
	}
	res := func(field, op, v string) (string, []any, error) {
		switch field {
		case "kind":
			m, err := enumMap(v, map[string]string{"NOTE": "note", "QUESTION": "question", "ISSUE": "issue", "RISK": "risk"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("c.kind", op)
			return q, []any{m}, err
		case "resolved":
			b := strings.ToLower(v)
			if b != "true" && b != "false" {
				return "", nil, badFilter("resolved expects true or false")
			}
			q, err := cmpSQL("c.resolved", op)
			n := 0
			if b == "true" {
				n = 1
			}
			return q, []any{n}, err
		case "side":
			m, err := enumMap(v, map[string]string{"DELIVERY": "delivery", "CUSTOMER": "customer", "SYSTEM": "system"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL(first("side"), op)
			return q, []any{m}, err
		case "creator":
			q, err := cmpSQL(first("actor"), op)
			return q, []any{AccountID(v)}, err
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
	ents, err := loadEntities(ctx, s.DB, p.ID, "comment", ids, time.Time{})
	if err != nil {
		return nil, "", internalErr(err)
	}
	out := make([]api.Comment, 0, len(ids))
	for _, id := range ids {
		if e := ents[id]; e != nil {
			out = append(out, RenderComment(p, e))
		}
	}
	return out, next, nil
}
