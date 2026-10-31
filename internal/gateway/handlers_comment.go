package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

func (s *Server) routeComments(q *request, wf int64) error {
	m, n := q.r.Method, len(q.segs)
	switch n {
	case 5:
		if q.verb != "" {
			return notFound("route")
		}
		switch m {
		case http.MethodGet:
			return s.listComments(q, wf)
		case http.MethodPost:
			return s.createComment(q, wf)
		}
		return methodNotAllowed(q)
	case 6:
		id, ok := store.ParseID(store.PrefixComment, q.segs[5])
		if !ok {
			return notFound("comment")
		}
		switch q.verb {
		case "":
			switch m {
			case http.MethodGet:
				c, err := s.Store.GetComment(q.r.Context(), q.pr.project(), wf, id)
				if err != nil {
					return err
				}
				return s.respond(q, c)
			case http.MethodDelete:
				return s.mutateComment(q, wf, id, "deleted", true)
			}
		case "resolve":
			if m == http.MethodPost {
				return s.mutateComment(q, wf, id, "resolved", true)
			}
		case "reopen":
			if m == http.MethodPost {
				return s.mutateComment(q, wf, id, "resolved", false)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	}
	return notFound("route")
}

func (s *Server) listComments(q *request, wf int64) error {
	o, err := q.listOpts()
	if err != nil {
		return err
	}
	if ok, err := s.Store.WorkflowExists(q.r.Context(), q.pr.project(), wf); err != nil {
		return err
	} else if !ok {
		return notFound("workflow")
	}
	cs, next, err := s.Store.ListComments(q.r.Context(), q.pr.project(), wf, o)
	if err != nil {
		return err
	}
	resp := api.ListCommentsResponse{Comments: &cs}
	if next != "" {
		resp.NextPageToken = &next
	}
	return s.respond(q, resp)
}

func (s *Server) createComment(q *request, wf int64) error {
	var body api.Comment
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(nil, nil, nil, nil, nil)
	if err != nil {
		return err
	}
	switch body.Kind {
	case "NOTE", "QUESTION", "ISSUE", "RISK":
	default:
		return invalid("INVALID_KIND", "kind must be NOTE, QUESTION, ISSUE or RISK")
	}
	if strings.TrimSpace(body.Body) == "" || len(body.Body) > 20000 {
		return invalid("INVALID_BODY", "body must be 1-20000 characters")
	}
	if ok, err := s.Store.WorkflowExists(q.r.Context(), q.pr.project(), wf); err != nil {
		return err
	} else if !ok {
		return notFound("workflow")
	}
	var et time.Time
	if body.EventTime != nil {
		et = *body.EventTime
	}
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		ch := []store.PlannedChange{
			{Key: "on", New: core.JSON(wf)},
			{Key: "kind", New: strRaw(strings.ToLower(string(body.Kind)))},
			{Key: "body", New: strRaw(body.Body)},
		}
		if body.Attachments != nil && len(*body.Attachments) > 0 {
			ch = append(ch, store.PlannedChange{Key: "attachments", New: core.JSON(*body.Attachments)})
		}
		return ch, nil
	}
	res, err := s.apply(q, "comment", 0, o, plan, et)
	if err != nil {
		return err
	}
	return s.respond(q, s.Store.PreviewComment(q.pr.project(), res.After))
}

// mutateComment sets resolved / deleted on a comment of the workflow.
func (s *Server) mutateComment(q *request, wf, id int64, key string, val bool) error {
	var body api.MutationOptions
	if q.r.Method == http.MethodPost {
		if err := q.decodeBody(&body, true); err != nil {
			return err
		}
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		if core.Scalar(cur.Vals["on"]) != strconv.FormatInt(wf, 10) || core.Bool(cur.Vals["deleted"]) {
			return nil, notFound("comment")
		}
		if key == "resolved" && core.Bool(cur.Vals["resolved"]) == val {
			return nil, nil // already in the requested state
		}
		return []store.PlannedChange{{Key: key, New: core.JSON(val)}}, nil
	}
	res, err := s.apply(q, "comment", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.respond(q, s.Store.PreviewComment(q.pr.project(), res.After))
}

// ---------------------------------------------------------------- events

func (s *Server) routeEvents(q *request, wf int64) error {
	if q.r.Method != http.MethodGet || q.verb != "" {
		return methodNotAllowed(q)
	}
	switch len(q.segs) {
	case 5:
		o, err := q.listOpts()
		if err != nil {
			return err
		}
		if ok, err := s.Store.WorkflowExists(q.r.Context(), q.pr.project(), wf); err != nil {
			return err
		} else if !ok {
			// deleted workflows keep their audit trail readable
			if _, err := s.Store.GetWorkflow(q.r.Context(), q.pr.project(), wf); err != nil {
				return err
			}
		}
		evs, next, err := s.Store.ListEvents(q.r.Context(), q.pr.project(), wf, o)
		if err != nil {
			return err
		}
		resp := api.ListEventsResponse{Events: &evs}
		if next != "" {
			resp.NextPageToken = &next
		}
		return s.respond(q, resp)
	case 6:
		id, ok := store.ParseID(store.PrefixEvent, q.segs[5])
		if !ok {
			return notFound("event")
		}
		ev, err := s.Store.GetEvent(q.r.Context(), q.pr.project(), wf, id)
		if err != nil {
			return err
		}
		return s.respond(q, ev)
	}
	return notFound("route")
}

// ---------------------------------------------------------------- query

func (s *Server) query(q *request) error {
	var body api.QueryRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	if body.AsOf != nil {
		return invalid("AS_OF_UNSUPPORTED", "as_of is not supported by query in this version; use the events list for point-in-time history")
	}
	ps, tok := 0, ""
	if body.PageSize != nil {
		ps = *body.PageSize
	}
	if body.PageToken != nil {
		tok = *body.PageToken
	}
	resp, err := s.Store.Query(store.WithQueryAccount(q.r.Context(), q.pr.Account.UserName), q.pr.project(), body.Sql, ps, tok)
	if err != nil {
		return err
	}
	return s.respond(q, resp)
}
