package gateway

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

var (
	labelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	roleKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
)

func (s *Server) getMe(q *request) error {
	ctx := q.r.Context()
	ms, err := s.Store.Memberships(ctx, q.pr.Account.UserName)
	if err != nil {
		return err
	}
	acc := "accounts/" + q.pr.Account.UserName
	me := api.Me{Account: &acc, Kind: ptrOf(api.AccountKind(strings.ToUpper(q.pr.Account.Kind)))}
	if q.pr.Account.DisplayName != "" {
		me.DisplayName = &q.pr.Account.DisplayName
	}
	out := []api.Member{}
	for _, m := range ms {
		out = append(out, api.Member{
			Name:       ptrOf("projects/" + m.ProjectName + "/members/" + m.UserName),
			Account:    &acc,
			Role:       api.Role(strings.ToUpper(m.Role)),
			Side:       api.MemberSide(strings.ToUpper(m.Side)),
			CreateTime: ptrOf(m.CreateTime),
		})
	}
	me.Memberships = &out
	return s.respond(q, me)
}

func ptrOf[T any](v T) *T { return &v }

// ---------------------------------------------------------------- key mapping helpers

func stateToValue(st api.StageState) (string, error) {
	switch st {
	case "PASSED":
		return core.StatePass, nil
	case "OPEN":
		return core.StateOpen, nil
	case "NOT_APPLICABLE":
		return core.StateNA, nil
	}
	return "", invalid("INVALID_STAGE_STATE", "invalid stage state %q", st)
}

func strRaw(s string) json.RawMessage { return core.JSON(s) }

// mapChanges turns a string map into attr./ann./assignee. key changes. When replace is set,
// existing keys with the prefix that are missing from m are removed.
func mapChanges(prefix string, m map[string]string, cur *store.Entity, replace bool, keyRe *regexp.Regexp, norm func(string) string) ([]store.PlannedChange, error) {
	var out []store.PlannedChange
	seen := map[string]bool{}
	for k, v := range m {
		k = strings.ToLower(k)
		if !keyRe.MatchString(k) {
			return nil, invalid("INVALID_KEY", "invalid key %q", k)
		}
		if len(v) > 4000 {
			return nil, invalid("VALUE_TOO_LONG", "value of %q is too long", k)
		}
		seen[k] = true
		out = append(out, store.PlannedChange{Key: prefix + k, New: strRaw(norm(v))})
	}
	if replace && cur != nil {
		for k := range cur.Vals {
			if rest, ok := strings.CutPrefix(k, prefix); ok && !seen[rest] {
				out = append(out, store.PlannedChange{Key: k})
			}
		}
	}
	return out, nil
}

func lowerNorm(s string) string   { return strings.ToLower(strings.TrimSpace(s)) }
func plainNorm(s string) string   { return s }
func accountNorm(s string) string { return store.AccountID(s) }

func profileChanges(display *string, labels *api.Labels, ann *api.Annotations, asg *api.Assignees, src *api.Source, cur *store.Entity, replace bool) ([]store.PlannedChange, error) {
	var out []store.PlannedChange
	if display != nil {
		name := strings.TrimSpace(*display)
		if name == "" || len(name) > 255 {
			return nil, invalid("INVALID_DISPLAY_NAME", "display_name must be 1-255 characters")
		}
		out = append(out, store.PlannedChange{Key: "name", New: strRaw(name)})
	}
	if labels != nil {
		c, err := mapChanges("attr.", *labels, cur, replace, labelKeyRe, lowerNorm)
		if err != nil {
			return nil, err
		}
		out = append(out, c...)
	}
	if ann != nil {
		c, err := mapChanges("ann.", *ann, cur, replace, labelKeyRe, plainNorm)
		if err != nil {
			return nil, err
		}
		out = append(out, c...)
	}
	if asg != nil {
		c, err := mapChanges("assignee.", *asg, cur, replace, roleKeyRe, accountNorm)
		if err != nil {
			return nil, err
		}
		out = append(out, c...)
	}
	if src != nil {
		if src.System != nil {
			out = append(out, store.PlannedChange{Key: "source.system", New: strRaw(*src.System)})
		}
		if src.Reference != nil {
			out = append(out, store.PlannedChange{Key: "source.ref", New: strRaw(*src.Reference)})
		}
		if src.RowHash != nil {
			out = append(out, store.PlannedChange{Key: "source.hash", New: strRaw(*src.RowHash)})
		}
	}
	return out, nil
}

func stageChanges(stages []api.StageStatus) ([]store.PlannedChange, error) {
	var out []store.PlannedChange
	for _, st := range stages {
		v, err := stateToValue(st.State)
		if err != nil {
			return nil, err
		}
		out = append(out, store.PlannedChange{Key: "stage." + strings.ToLower(st.Stage), New: strRaw(v)})
	}
	return out, nil
}

// ---------------------------------------------------------------- workflows

func (s *Server) listWorkflows(q *request) error {
	o, err := q.listOpts()
	if err != nil {
		return err
	}
	ws, next, err := s.Store.ListWorkflows(q.r.Context(), q.pr.project(), o)
	if err != nil {
		return err
	}
	resp := api.ListWorkflowsResponse{Workflows: &ws}
	if next != "" {
		resp.NextPageToken = &next
	}
	return s.respond(q, resp)
}

func (s *Server) getWorkflow(q *request, id int64) error {
	w, err := s.Store.GetWorkflow(q.r.Context(), q.pr.project(), id)
	if err != nil {
		return err
	}
	return s.respond(q, w)
}

// workflowResult renders the outcome of a write: preview for validate_only, fresh read otherwise.
func (s *Server) workflowResult(q *request, res *store.Result, o mutOpts) error {
	if o.ValidateOnly {
		w, err := s.Store.PreviewWorkflow(q.r.Context(), q.pr.project(), res.After)
		if err != nil {
			return err
		}
		return s.respond(q, w)
	}
	return s.getWorkflow(q, res.EntityID)
}

func (s *Server) createWorkflow(q *request) error {
	var body api.Workflow
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	if q.query("workflow_id") != "" {
		return invalid("CLIENT_ID_UNSUPPORTED", "client-assigned workflow ids are not supported; ids are server-assigned (wf-NNNNNN)")
	}
	o, err := q.mutOpts(nil, nil, nil, nil, nil)
	if err != nil {
		return err
	}
	if body.DisplayName == nil || strings.TrimSpace(*body.DisplayName) == "" {
		return invalid("INVALID_DISPLAY_NAME", "display_name is required")
	}
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		ch, err := profileChanges(body.DisplayName, body.Labels, body.Annotations, body.Assignees, body.Source, nil, false)
		if err != nil {
			return nil, err
		}
		if body.Stages != nil {
			sc, err := stageChanges(*body.Stages)
			if err != nil {
				return nil, err
			}
			ch = append(ch, sc...)
		}
		return ch, nil
	}
	res, err := s.apply(q, "workflow", 0, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.workflowResult(q, res, o)
}

func (s *Server) updateWorkflow(q *request, id int64) error {
	var body api.UpdateWorkflowRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	paths, err := parseMask(q.query("update_mask"), []string{"display_name", "labels", "annotations", "assignees", "stages"})
	if err != nil {
		return err
	}
	w := body.Workflow
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		var out []store.PlannedChange
		for _, path := range paths {
			var ch []store.PlannedChange
			var err error
			top, sub, _ := strings.Cut(path, ".")
			switch top {
			case "display_name":
				ch, err = profileChanges(w.DisplayName, nil, nil, nil, nil, cur, false)
			case "labels":
				ch, err = nestedMapUpdate("attr.", sub, derefMap(w.Labels), cur, labelKeyRe, lowerNorm)
			case "annotations":
				ch, err = nestedMapUpdate("ann.", sub, derefMap(w.Annotations), cur, labelKeyRe, plainNorm)
			case "assignees":
				ch, err = nestedMapUpdate("assignee.", sub, derefMap(w.Assignees), cur, roleKeyRe, accountNorm)
			case "stages":
				var stages []api.StageStatus
				if w.Stages != nil {
					stages = *w.Stages
				}
				if sub != "" {
					var only []api.StageStatus
					for _, st := range stages {
						if strings.EqualFold(st.Stage, sub) {
							only = append(only, st)
						}
					}
					if len(only) == 0 {
						return nil, invalid("INVALID_UPDATE_MASK", "update_mask names stages.%s but the body has no such stage", sub)
					}
					stages = only
				}
				ch, err = stageChanges(stages)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, ch...)
		}
		return out, nil
	}
	res, err := s.apply(q, "workflow", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.workflowResult(q, res, o)
}

// nestedMapUpdate handles "labels" (replace the map) and "labels.k" (one key; absent in body = delete).
func nestedMapUpdate(prefix, sub string, mm map[string]string, cur *store.Entity, keyRe *regexp.Regexp, norm func(string) string) ([]store.PlannedChange, error) {
	if sub == "" {
		if mm == nil {
			mm = map[string]string{}
		}
		return mapChanges(prefix, mm, cur, true, keyRe, norm)
	}
	sub = strings.ToLower(sub)
	if v, ok := mm[sub]; ok {
		return mapChanges(prefix, map[string]string{sub: v}, cur, false, keyRe, norm)
	}
	if !keyRe.MatchString(sub) {
		return nil, invalid("INVALID_KEY", "invalid key %q", sub)
	}
	return []store.PlannedChange{{Key: prefix + sub}}, nil
}

func parseMask(mask string, allowed []string) ([]string, error) {
	if strings.TrimSpace(mask) == "" {
		return nil, invalid("INVALID_UPDATE_MASK", "update_mask is required")
	}
	var out []string
	for _, p := range strings.Split(mask, ",") {
		p = strings.TrimSpace(p)
		top, _, _ := strings.Cut(p, ".")
		ok := false
		for _, a := range allowed {
			if top == a {
				ok = true
			}
		}
		if !ok {
			hint := ""
			if p == "scope" || p == "merged_into" {
				hint = "; use :setScope"
			}
			return nil, invalid("INVALID_UPDATE_MASK", "field %q cannot be updated (updatable: %s)%s", p, strings.Join(allowed, ", "), hint)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) advanceStage(q *request, id int64) error {
	var body api.AdvanceStageRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	stage := strings.ToLower(strings.TrimSpace(body.Stage))
	if stage == "" {
		return invalid("INVALID_STAGE", "stage is required")
	}
	// evidence_attachments must be attachments of this workflow; they are recorded on the event
	// as evidence.<stage> (a list of attachment resource names) next to the stage change.
	var evidence []string
	if body.EvidenceAttachments != nil && len(*body.EvidenceAttachments) > 0 {
		if evidence, err = s.Store.ResolveEvidence(q.r.Context(), q.pr.project(), id, *body.EvidenceAttachments); err != nil {
			return err
		}
	}
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		ch := []store.PlannedChange{{Key: "stage." + stage, New: strRaw(core.StatePass)}}
		if len(evidence) > 0 && core.Scalar(cur.Vals["stage."+stage]) != core.StatePass {
			ch = append(ch, store.PlannedChange{Key: "evidence." + stage, New: core.JSON(evidence)})
		}
		return ch, nil
	}
	res, err := s.apply(q, "workflow", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.workflowResult(q, res, o)
}

func (s *Server) regressStage(q *request, id int64) error {
	var body api.RegressStageRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, &body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	if len(body.Stages) == 0 {
		return invalid("INVALID_STAGE", "stages must not be empty")
	}
	if o.Reason == "" {
		return core.NewError(core.StatusFailedPrecondition, "REASON_REQUIRED", "regressing a stage requires a reject reason").With("required_kind", core.KindReject)
	}
	plan := func(cur *store.Entity, _ core.Env) ([]store.PlannedChange, error) {
		var out []store.PlannedChange
		for _, st := range body.Stages {
			st = strings.ToLower(strings.TrimSpace(st))
			if v, _ := core.Str(cur.Vals["stage."+st]); v != core.StatePass {
				return nil, core.NewError(core.StatusFailedPrecondition, "NOT_REGRESSIBLE", "stage %q is not PASSED", st).With("stage", st)
			}
			out = append(out, store.PlannedChange{Key: "stage." + st, New: strRaw(core.StateOpen)})
		}
		return out, nil
	}
	res, err := s.apply(q, "workflow", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.workflowResult(q, res, o)
}

func (s *Server) setScope(q *request, id int64) error {
	var body api.SetScopeRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	p := q.pr.project()
	var plan func(*store.Entity, core.Env) ([]store.PlannedChange, error)
	switch body.Scope {
	case "IN_SCOPE":
		if body.MergedInto != nil && *body.MergedInto != "" {
			return invalid("INVALID_MERGED_INTO", "merged_into is only valid with OUT_OF_SCOPE")
		}
		plan = func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: "scope", New: strRaw("in")}, {Key: "into"}}, nil
		}
	case "OUT_OF_SCOPE":
		var into json.RawMessage
		if o.Reason == "merged" {
			if body.MergedInto == nil || *body.MergedInto == "" {
				return invalid("MERGE_TARGET_REQUIRED", "reason merged requires merged_into")
			}
			target, ok := p.ParseWorkflowRef(*body.MergedInto)
			if !ok {
				return invalid("INVALID_MERGED_INTO", "merged_into %q is not a workflow resource name", *body.MergedInto)
			}
			into = json.RawMessage(strconv.FormatInt(target, 10))
		} else if body.MergedInto != nil && *body.MergedInto != "" {
			return invalid("INVALID_MERGED_INTO", "merged_into requires reason merged")
		}
		plan = func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: "scope", New: strRaw("out")}, {Key: "into", New: into}}, nil
		}
	default:
		return invalid("INVALID_SCOPE", "scope must be IN_SCOPE or OUT_OF_SCOPE")
	}
	res, err := s.apply(q, "workflow", id, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.workflowResult(q, res, o)
}

// setDeleted soft-deletes or restores a workflow or task.
func (s *Server) setDeleted(q *request, id int64, typ string, del bool) error {
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
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		return []store.PlannedChange{{Key: "deleted", New: core.JSON(del)}}, nil
	}
	res, err := s.apply(q, typ, id, o, plan, timeZero)
	if err != nil {
		return err
	}
	if typ == "workflow" {
		return s.workflowResult(q, res, o)
	}
	return s.respond(q, s.Store.PreviewTask(q.pr.project(), res.After))
}

func derefMap[M ~map[string]string](p *M) map[string]string {
	if p == nil {
		return nil
	}
	return map[string]string(*p)
}
