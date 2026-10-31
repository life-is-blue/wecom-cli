package store

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// RenderTask maps folded state to the API Task.
func RenderTask(p Project, e *Entity) api.Task {
	id := strconv.FormatInt(e.ID, 10)
	t := api.Task{
		DisplayName: e.str("name"),
		Uid:         &id,
		Labels:      e.labels(),
		Annotations: e.annotations(),
		Assignees:   e.assignees(),
		Source:      e.source(),
		CreateTime:  ptr(e.CreateTime),
		DeleteTime:  e.deleteTime(),
	}
	if e.ID != 0 {
		t.Name = ptr(p.taskName(e.ID))
	}
	if e.LatestEvent != 0 {
		t.Etag = ptr(e.Etag())
	}
	if e.Creator != "" {
		t.Creator = ptr(accountName(e.Creator))
	}
	var ms []api.TaskMembership
	var wfIDs []int64
	mem := map[int64]string{}
	for k, v := range e.Vals {
		if rest, ok := strings.CutPrefix(k, "in."); ok {
			if n, err := strconv.ParseInt(rest, 10, 64); err == nil {
				wfIDs = append(wfIDs, n)
				mem[n] = core.Scalar(v)
			}
		}
	}
	sort.Slice(wfIDs, func(i, j int) bool { return wfIDs[i] < wfIDs[j] })
	for _, n := range wfIDs {
		m := api.TaskMembership{Workflow: p.workflowName(n)}
		if mem[n] == "primary" {
			m.Primary = ptr(true)
		}
		ms = append(ms, m)
	}
	if len(ms) > 0 {
		t.Workflows = &ms
	}
	return t
}

// PreviewTask renders a validate_only task result.
func (s *Store) PreviewTask(p Project, e *Entity) *api.Task {
	t := RenderTask(p, e)
	return &t
}

// GetTask reads one task (soft-deleted ones carry delete_time).
func (s *Store) GetTask(ctx context.Context, p Project, id int64) (*api.Task, error) {
	e, err := loadEntity(ctx, s.DB, p.ID, "task", id)
	if err != nil {
		return nil, internalErr(err)
	}
	t := RenderTask(p, e)
	return &t, nil
}

// ListTasks lists tasks of a project.
func (s *Store) ListTasks(ctx context.Context, p Project, o ListOpts) ([]api.Task, string, error) {
	lq := &listQuery{
		from:  "entity e",
		idCol: "e.id",
		where: []string{"e.project_id = ?", "e.type = 'task'"},
		args:  []any{p.ID},
		orders: map[string]orderSpec{
			"create_time":  {expr: "e.id", byID: true},
			"name":         {expr: "e.id", byID: true},
			"id":           {expr: "e.id", byID: true},
			"display_name": {expr: latestText("e", "name")},
		},
	}
	if !o.ShowDeleted {
		lq.where = append(lq.where, "COALESCE("+latestText("e", "deleted")+", 'false') <> 'true'")
	}
	res := func(field, op, v string) (string, []any, error) {
		switch {
		case field == "display_name":
			if op == ":" {
				return latestText("e", "name") + " LIKE ?", []any{"%" + core.EscapeLike(v) + "%"}, nil
			}
			q, err := cmpSQL(latestText("e", "name"), op)
			return q, []any{v}, err
		case strings.HasPrefix(field, "labels."):
			k := strings.ToLower(strings.TrimPrefix(field, "labels."))
			if !safeKey.MatchString(k) {
				return "", nil, badFilter("invalid label key %q", k)
			}
			q, err := cmpSQL(latestText("e", "attr."+k), op)
			return q, []any{strings.ToLower(v)}, err
		case strings.HasPrefix(field, "assignees."):
			k := strings.ToLower(strings.TrimPrefix(field, "assignees."))
			if !safeKey.MatchString(k) {
				return "", nil, badFilter("invalid assignee role %q", k)
			}
			q, err := cmpSQL(latestText("e", "assignee."+k), op)
			return q, []any{AccountID(v)}, err
		case field == "workflows.workflow":
			wf, ok := p.ParseWorkflowRef(v)
			if !ok || op != "=" {
				return "", nil, badFilter("workflows.workflow expects = and a workflow resource name")
			}
			return latestRaw("e", "in."+strconv.FormatInt(wf, 10)) + " IS NOT NULL", nil, nil
		case field == "create_time":
			t, err := parseTime(v)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("e.create_time", op)
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
	ents, err := loadEntities(ctx, s.DB, p.ID, "task", ids, time.Time{})
	if err != nil {
		return nil, "", internalErr(err)
	}
	out := make([]api.Task, 0, len(ids))
	for _, id := range ids {
		if e := ents[id]; e != nil {
			out = append(out, RenderTask(p, e))
		}
	}
	return out, next, nil
}
