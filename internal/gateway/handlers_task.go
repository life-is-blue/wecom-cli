package gateway

import (
	"strconv"
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

// requireWorkflow checks that the referenced workflow exists and is not deleted.
func (s *Server) requireWorkflow(q *request, ref string) (int64, error) {
	p := q.pr.project()
	id, ok := p.ParseWorkflowRef(ref)
	if !ok {
		return 0, invalid("INVALID_WORKFLOW", "%q is not a workflow resource name", ref)
	}
	exists, err := s.Store.WorkflowExists(q.r.Context(), p, id)
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
	type member struct {
		wf      int64
		primary bool
	}
	var members []member
	primaries := 0
	if body.Workflows != nil {
		for _, m := range *body.Workflows {
			id, err := s.requireWorkflow(q, m.Workflow)
			if err != nil {
				return err
			}
			pr := m.Primary != nil && *m.Primary
			if pr {
				primaries++
			}
			members = append(members, member{id, pr})
		}
	}
	if primaries > 1 {
		return invalid("MULTIPLE_PRIMARY", "a task has at most one primary workflow")
	}
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		ch, err := profileChanges(&body.DisplayName, body.Labels, body.Annotations, body.Assignees, body.Source, nil, false)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			v := "shared"
			if m.primary {
				v = "primary"
			}
			ch = append(ch, store.PlannedChange{Key: "in." + strconv.FormatInt(m.wf, 10), New: strRaw(v)})
		}
		return ch, nil
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
	var wf int64
	if body.Membership == "NONE" {
		var ok bool
		if wf, ok = q.pr.project().ParseWorkflowRef(body.Workflow); !ok {
			return invalid("INVALID_WORKFLOW", "%q is not a workflow resource name", body.Workflow)
		}
	} else {
		if wf, err = s.requireWorkflow(q, body.Workflow); err != nil {
			return err
		}
	}
	key := "in." + strconv.FormatInt(wf, 10)
	var plan func(*store.Entity, core.Env) ([]store.PlannedChange, error)
	switch body.Membership {
	case "PRIMARY":
		plan = func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
			out := []store.PlannedChange{{Key: key, New: strRaw("primary")}}
			for k, v := range cur.Vals { // at most one primary: demote the others
				if strings.HasPrefix(k, "in.") && k != key && core.Scalar(v) == "primary" {
					out = append(out, store.PlannedChange{Key: k, New: strRaw("shared")})
				}
			}
			return out, nil
		}
	case "SHARED":
		plan = func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: key, New: strRaw("shared")}}, nil
		}
	case "NONE":
		plan = func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: key}}, nil
		}
	default:
		return invalid("INVALID_MEMBERSHIP", "membership must be PRIMARY, SHARED or NONE")
	}
	res, err := s.apply(q, "task", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.taskResult(q, res)
}
