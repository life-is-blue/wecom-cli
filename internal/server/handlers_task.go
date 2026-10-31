package server

import (
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

func (s *Server) listTasks(q *request) error {
	o, err := q.listOpts()
	if err != nil {
		return err
	}
	ts, next, err := s.Store.ListTasks(q.r.Context(), q.pr.project(), o)
	if err != nil {
		return err
	}
	resp := api.ListTasksResponse{Tasks: &ts}
	if next != "" {
		resp.NextPageToken = &next
	}
	return s.respond(q, resp)
}

func (s *Server) getTask(q *request, id int64) error {
	t, err := s.Store.GetTask(q.r.Context(), q.pr.project(), id)
	if err != nil {
		return err
	}
	return s.respond(q, t)
}

func (s *Server) taskResult(q *request, res *store.Result) error {
	return s.respond(q, s.Store.PreviewTask(q.pr.project(), res.After))
}

// requireWorkflow checks that the referenced workflow exists (a deleted one is refused later, by
// core.Validate, with ENTITY_DELETED).
func (s *Server) requireWorkflow(q *request, ref string) (int64, error) {
	p := q.pr.project()
	id, ok := p.ParseWorkflowRef(ref)
	if !ok {
		return 0, invalid("INVALID_WORKFLOW", "%q is not a workflow resource name", ref)
	}
	exists, err := s.Store.WorkflowKnown(q.r.Context(), p, id)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, invalid("WORKFLOW_NOT_FOUND", "workflow %q does not exist", ref)
	}
	return id, nil
}

func (s *Server) createTask(q *request) error {
	var body api.Task
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	if q.query("task_id") != "" {
		return invalid("CLIENT_ID_UNSUPPORTED", "client-assigned task ids are not supported; ids are server-assigned (tk-NNNNNN)")
	}
	o, err := q.mutOpts(nil, nil, nil, nil, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body.DisplayName) == "" {
		return invalid("INVALID_DISPLAY_NAME", "display_name is required")
	}
	wf, err := s.requireWorkflow(q, body.Workflow)
	if err != nil {
		return err
	}
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		ch, err := profileChanges(&body.DisplayName, body.Labels, body.Annotations, body.Assignees, body.Source, nil, false)
		if err != nil {
			return nil, err
		}
		return append(ch, store.PlannedChange{Key: "workflow", New: core.JSON(wf)}), nil
	}
	res, err := s.apply(q, "task", 0, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.taskResult(q, res)
}

func (s *Server) updateTask(q *request, id int64) error {
	var body api.UpdateTaskRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	paths, err := parseMask(q.query("update_mask"), []string{"display_name", "labels", "annotations", "assignees"})
	if err != nil {
		return err
	}
	t := body.Task
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		var out []store.PlannedChange
		for _, path := range paths {
			var ch []store.PlannedChange
			var err error
			top, sub, _ := strings.Cut(path, ".")
			switch top {
			case "display_name":
				ch, err = profileChanges(&t.DisplayName, nil, nil, nil, nil, cur, false)
			case "labels":
				ch, err = nestedMapUpdate("attr.", sub, derefMap(t.Labels), cur, labelKeyRe, lowerNorm)
			case "annotations":
				ch, err = nestedMapUpdate("ann.", sub, derefMap(t.Annotations), cur, labelKeyRe, plainNorm)
			case "assignees":
				ch, err = nestedMapUpdate("assignee.", sub, derefMap(t.Assignees), cur, roleKeyRe, accountNorm)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, ch...)
		}
		return out, nil
	}
	res, err := s.apply(q, "task", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.taskResult(q, res)
}

func (s *Server) assignTask(q *request, id int64) error {
	var body api.AssignTaskRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	wf, err := s.requireWorkflow(q, body.Workflow)
	if err != nil {
		return err
	}
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		return []store.PlannedChange{{Key: "workflow", New: core.JSON(wf)}}, nil
	}
	res, err := s.apply(q, "task", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.taskResult(q, res)
}
