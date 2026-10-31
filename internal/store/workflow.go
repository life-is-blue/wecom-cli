package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

func ptr[T any](v T) *T { return &v }

func nonEmptyMap(m map[string]string) *map[string]string {
	if len(m) == 0 {
		return nil
	}
	return &m
}

// source builds the API Source from source.* keys.
func (e *Entity) source() *api.Source {
	sys, ref, hash := e.str("source.system"), e.str("source.ref"), e.str("source.hash")
	if sys == "" && ref == "" && hash == "" {
		return nil
	}
	s := &api.Source{}
	if sys != "" {
		s.System = &sys
	}
	if ref != "" {
		s.Reference = &ref
	}
	if hash != "" {
		s.RowHash = &hash
	}
	return s
}

func (e *Entity) labels() *api.Labels {
	m := e.prefixed("attr.")
	if len(m) == 0 {
		return nil
	}
	l := api.Labels(m)
	return &l
}

func (e *Entity) annotations() *api.Annotations {
	m := e.prefixed("ann.")
	if len(m) == 0 {
		return nil
	}
	a := api.Annotations(m)
	return &a
}

func (e *Entity) assignees() *api.Assignees {
	m := e.prefixed("assignee.")
	if len(m) == 0 {
		return nil
	}
	for k, v := range m {
		m[k] = accountName(v)
	}
	a := api.Assignees(m)
	return &a
}

// RenderWorkflow maps folded state to the API resource. health is ok|at_risk|blocked; currentStage "" = done.
func RenderWorkflow(p Project, cfg core.Config, e *Entity, currentStage, health string) api.Workflow {
	id := strconv.FormatInt(e.ID, 10)
	w := api.Workflow{
		DisplayName: ptrStr(e.str("name")),
		Uid:         &id,
		Labels:      e.labels(),
		Annotations: e.annotations(),
		Assignees:   e.assignees(),
		Source:      e.source(),
		CreateTime:  ptr(e.CreateTime),
		DeleteTime:  e.deleteTime(),
	}
	if e.ID != 0 {
		w.Name = ptr(p.workflowName(e.ID))
	}
	if e.LatestEvent != 0 {
		w.Etag = ptr(e.Etag())
	}
	if e.Creator != "" {
		w.Creator = ptr(accountName(e.Creator))
	}
	var stages []api.StageStatus
	for _, s := range cfg.Stages {
		st := api.StageState("OPEN")
		switch e.str("stage." + s.Key) {
		case core.StatePass:
			st = "PASSED"
		case core.StateNA:
			st = "NOT_APPLICABLE"
		}
		stages = append(stages, api.StageStatus{Stage: s.Key, State: st})
	}
	w.Stages = &stages
	if e.str("scope") == "out" {
		w.Scope = ptr(api.Scope("OUT_OF_SCOPE"))
		if raw, ok := e.Vals["into"]; ok {
			if n, err := strconv.ParseInt(core.Scalar(raw), 10, 64); err == nil && n > 0 {
				w.MergedInto = ptr(p.workflowName(n))
			}
		}
	} else {
		w.Scope = ptr(api.Scope("IN_SCOPE"))
	}
	switch health {
	case "blocked":
		w.Health = ptr(api.Health("BLOCKED"))
	case "at_risk":
		w.Health = ptr(api.Health("AT_RISK"))
	default:
		w.Health = ptr(api.Health("NORMAL"))
	}
	w.CurrentStage = ptr(currentStage)
	return w
}

// CurrentStageOf computes current_stage from folded values (used for validate_only previews;
// persisted reads take it from the v_current_stage view).
func CurrentStageOf(cfg core.Config, e *Entity) string {
	for _, s := range cfg.Stages {
		if st := e.str("stage." + s.Key); st != core.StatePass && st != core.StateNA {
			return s.Key
		}
	}
	return ""
}

// PreviewWorkflow renders a validate_only result: current_stage computed from values, health from the views.
func (s *Store) PreviewWorkflow(ctx context.Context, p Project, e *Entity) (*api.Workflow, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	health := ""
	if e.ID != 0 {
		d, err := loadDerived(ctx, s.DB, []int64{e.ID})
		if err != nil {
			return nil, err
		}
		health = d[e.ID].Health
	}
	w := RenderWorkflow(p, cfg, e, CurrentStageOf(cfg, e), health)
	return &w, nil
}

// GetWorkflow reads one workflow (soft-deleted ones are returned with delete_time).
func (s *Store) GetWorkflow(ctx context.Context, p Project, id int64) (*api.Workflow, error) {
	e, err := loadEntity(ctx, s.DB, p.ID, "workflow", id)
	if err != nil {
		return nil, internalErr(err)
	}
	ws, err := s.renderWorkflows(ctx, p, map[int64]*Entity{id: e}, []int64{id})
	if err != nil {
		return nil, internalErr(err)
	}
	return &ws[0], nil
}

// WorkflowExists reports a live workflow in the project (for comment/task parents).
func (s *Store) WorkflowExists(ctx context.Context, p Project, id int64) (bool, error) {
	e, err := loadEntity(ctx, s.DB, p.ID, "workflow", id)
	if err != nil {
		var ce *core.Error
		if asCore(err, &ce) && ce.Status == core.StatusNotFound {
			return false, nil
		}
		return false, internalErr(err)
	}
	return !e.deleted(), nil
}

// WorkflowKnown reports whether the workflow exists in the project, deleted or not.
func (s *Store) WorkflowKnown(ctx context.Context, p Project, id int64) (bool, error) {
	_, err := loadEntity(ctx, s.DB, p.ID, "workflow", id)
	var ce *core.Error
	if asCore(err, &ce) && ce.Status == core.StatusNotFound {
		return false, nil
	}
	return err == nil, internalErr(err)
}

func (s *Store) renderWorkflows(ctx context.Context, p Project, ents map[int64]*Entity, order []int64) ([]api.Workflow, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	der, err := loadDerived(ctx, s.DB, order)
	if err != nil {
		return nil, err
	}
	out := make([]api.Workflow, 0, len(order))
	for _, id := range order {
		e := ents[id]
		if e == nil {
			continue
		}
		out = append(out, RenderWorkflow(p, cfg, e, der[id].CurrentStage, der[id].Health))
	}
	return out, nil
}

// ListWorkflows lists workflows of a project.
func (s *Store) ListWorkflows(ctx context.Context, p Project, o ListOpts) ([]api.Workflow, string, error) {
	lq := &listQuery{
		from:  "entity e",
		idCol: "e.id",
		where: []string{"e.project_id = ?", "e.type = 'workflow'"},
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
	var needCS, needOI bool
	res := func(field, op, v string) (string, []any, error) {
		switch {
		case field == "display_name":
			if op == ":" {
				return latestText("e", "name") + " LIKE ?", []any{"%" + core.EscapeLike(v) + "%"}, nil
			}
			q, err := cmpSQL(latestText("e", "name"), op)
			return q, []any{v}, err
		case field == "health":
			needOI = true
			m, err := enumMap(v, map[string]string{"NORMAL": "ok", "AT_RISK": "at_risk", "BLOCKED": "blocked"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("(CASE WHEN COALESCE(oi.open_issues,0) > 0 THEN 'blocked' WHEN COALESCE(oi.open_risks,0) > 0 THEN 'at_risk' ELSE 'ok' END)", op)
			return q, []any{m}, err
		case field == "scope":
			m, err := enumMap(v, map[string]string{"IN_SCOPE": "in", "OUT_OF_SCOPE": "out"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("COALESCE("+latestText("e", "scope")+", 'in')", op)
			return q, []any{m}, err
		case field == "current_stage":
			needCS = true
			q, err := cmpSQL("COALESCE(cs.current_stage, '')", op)
			return q, []any{strings.ToLower(v)}, err
		case strings.HasPrefix(field, "stages."):
			k := strings.ToLower(strings.TrimPrefix(field, "stages."))
			if !safeKey.MatchString(k) {
				return "", nil, badFilter("invalid stage %q", k)
			}
			m, err := enumMap(v, map[string]string{"PASSED": "pass", "OPEN": "open", "NOT_APPLICABLE": "na"}, field)
			if err != nil {
				return "", nil, err
			}
			q, err := cmpSQL("COALESCE("+latestText("e", "stage."+k)+", 'open')", op)
			return q, []any{m}, err
		case strings.HasPrefix(field, "labels."):
			k := strings.ToLower(strings.TrimPrefix(field, "labels."))
			if !safeKey.MatchString(k) {
				return "", nil, badFilter("invalid label key %q", k)
			}
			if op == ":" {
				return latestText("e", "attr."+k) + " LIKE ?", []any{"%" + core.EscapeLike(strings.ToLower(v)) + "%"}, nil
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
	if needCS {
		lq.from += " LEFT JOIN v_current_stage cs ON cs.workflow_id = e.id"
	}
	if needOI {
		lq.from += " LEFT JOIN v_open_items oi ON oi.workflow_id = e.id"
	}
	ids, next, err := lq.run(ctx, s.DB, o)
	if err != nil {
		return nil, "", internalErr(err)
	}
	ents, err := loadEntities(ctx, s.DB, p.ID, "workflow", ids, time.Time{})
	if err != nil {
		return nil, "", internalErr(err)
	}
	ws, err := s.renderWorkflows(ctx, p, ents, ids)
	if err != nil {
		return nil, "", internalErr(err)
	}
	return ws, next, nil
}

func ptrStr(s string) *string { return &s }
