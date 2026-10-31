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

// routeImportJobs serves /v1/projects/{p}/importJobs[/{id}][:diff|:apply|:rollback].
func (s *Server) routeImportJobs(q *request) error {
	m, n := q.r.Method, len(q.segs)
	switch n {
	case 3:
		if q.verb == "" && m == http.MethodPost {
			return s.createImportJob(q)
		}
		if q.verb != "" {
			return notFound("route")
		}
		return methodNotAllowed(q)
	case 4:
		id, err := strconv.ParseInt(q.segs[3], 10, 64)
		if err != nil || id <= 0 {
			return notFound("import job")
		}
		switch q.verb {
		case "":
			if m == http.MethodGet {
				return s.getImportJob(q, id)
			}
		case "diff":
			if m == http.MethodGet {
				return s.diffImportJob(q, id)
			}
		case "apply":
			if m == http.MethodPost {
				return s.applyImportJob(q, id, false)
			}
		case "rollback":
			if m == http.MethodPost {
				return s.applyImportJob(q, id, true)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	}
	return notFound("route")
}

func requireRole(q *request, roles ...string) error {
	for _, r := range roles {
		if q.pr.Member.Role == r {
			return nil
		}
	}
	return core.NewError(core.StatusPermissionDenied, "ROLE_INSUFFICIENT", "this call requires the %s role", roles[0])
}

func (s *Server) createImportJob(q *request) error {
	var body struct {
		Source    string            `json:"source"`
		Rows      []json.RawMessage `json:"rows"`
		RequestID string            `json:"request_id"`
	}
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(nil, nil, nil, &body.RequestID, nil)
	if err != nil {
		return err
	}
	job, err := s.Store.StageImport(q.r.Context(), q.pr.project(), q.pr.actor(), store.StageInput{
		Source: body.Source, Rows: body.Rows, RequestID: o.RequestID,
	})
	if err != nil {
		return err
	}
	return s.respond(q, job)
}

func (s *Server) getImportJob(q *request, id int64) error {
	job, err := s.Store.GetImport(q.r.Context(), q.pr.project(), id)
	if err != nil {
		return err
	}
	return s.respond(q, job)
}

var actionFilterRe = regexp.MustCompile(`^action\s*(?:=|:)\s*"?([A-Za-z_]+)"?$`)
var orSplitRe = regexp.MustCompile(`(?i)\s+OR\s+`)

func parseActionFilter(f string) (map[string]bool, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, nil
	}
	out := map[string]bool{}
	for _, part := range orSplitRe.Split(f, -1) {
		m := actionFilterRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, invalid("INVALID_FILTER", "filter supports only: action = NEW|CHANGED|UNCHANGED|MISSING|CONFLICT|SUSPECT (joined with OR)")
		}
		a := strings.ToLower(m[1])
		switch a {
		case core.ActionNew, core.ActionChanged, core.ActionUnchanged, core.ActionMissing, core.ActionConflict, core.ActionSuspect:
			out[a] = true
		default:
			return nil, invalid("INVALID_FILTER", "unknown action %q", m[1])
		}
	}
	return out, nil
}

// apiKey maps a storage key to its API name.
func apiKey(k string) string {
	switch {
	case k == "name":
		return "display_name"
	case k == "source.ref":
		return "source.reference"
	case k == "source.hash":
		return "source.row_hash"
	case strings.HasPrefix(k, "attr."):
		return "labels." + strings.TrimPrefix(k, "attr.")
	case strings.HasPrefix(k, "ann."):
		return "annotations." + strings.TrimPrefix(k, "ann.")
	case strings.HasPrefix(k, "assignee."):
		return "assignees." + strings.TrimPrefix(k, "assignee.")
	case strings.HasPrefix(k, "stage."):
		return "stages." + strings.TrimPrefix(k, "stage.")
	}
	return k
}

type diffChange struct {
	Key      string          `json:"key"`
	OldValue json.RawMessage `json:"old_value,omitempty"`
	NewValue json.RawMessage `json:"new_value,omitempty"`
	Kind     string          `json:"kind,omitempty"` // source | fill | conflict (extension)
}

type diffLine struct {
	RowNo   int          `json:"row_no"`
	Source  api.Source   `json:"source"`
	Action  string       `json:"action"`
	Match   string       `json:"match,omitempty"`
	Changes []diffChange `json:"changes,omitempty"`
}

func (s *Server) diffImportJob(q *request, id int64) error {
	if err := requireRole(q, core.RoleEditor, core.RoleAdmin); err != nil {
		return err
	}
	actions, err := parseActionFilter(q.query("filter"))
	if err != nil {
		return err
	}
	size, offset := 100, 0
	if v := q.query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("INVALID_PAGE_SIZE", "page_size must be a non-negative integer")
		}
		if n > 0 {
			size = min(n, 1000)
		}
	}
	if v := q.query("page_token"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("INVALID_PAGE_TOKEN", "invalid page_token")
		}
		offset = n
	}
	p := q.pr.project()
	rows, next, err := s.Store.DiffImport(q.r.Context(), p, id, actions, offset, size)
	if err != nil {
		return err
	}
	out := struct {
		Rows          []diffLine `json:"rows"`
		NextPageToken string     `json:"next_page_token,omitempty"`
	}{Rows: []diffLine{}}
	for _, r := range rows {
		l := diffLine{RowNo: r.RowNo, Action: strings.ToUpper(r.Action)}
		if r.System != "" {
			l.Source.System = &r.System
		}
		if r.Ref != "" {
			l.Source.Reference = &r.Ref
		}
		if r.Hash != "" {
			l.Source.RowHash = &r.Hash
		}
		if r.Match != 0 {
			l.Match = p.WorkflowName(r.Match)
		}
		for _, c := range r.Changes {
			dc := diffChange{Key: apiKey(c.Key), OldValue: c.Old, NewValue: c.New, Kind: c.Kind}
			if strings.HasPrefix(c.Key, "assignee.") {
				dc.OldValue, dc.NewValue = accountValue(c.Old), accountValue(c.New)
			}
			l.Changes = append(l.Changes, dc)
		}
		out.Rows = append(out.Rows, l)
	}
	if next > 0 {
		out.NextPageToken = strconv.Itoa(next)
	}
	return s.respond(q, out)
}

func accountValue(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	if v, ok := core.Str(raw); ok && v != "" {
		return core.JSON("accounts/" + v)
	}
	return raw
}

// applyImportJob serves :apply and :rollback (admin only) and returns an Operation.
func (s *Server) applyImportJob(q *request, id int64, rollback bool) error {
	if err := requireRole(q, core.RoleAdmin); err != nil {
		return err
	}
	var body struct {
		api.MutationOptions
		Resolutions []struct {
			RowNo      int    `json:"row_no"`
			Resolution string `json:"resolution"`
		} `json:"resolutions"`
	}
	if err := q.decodeBody(&body, true); err != nil {
		return err
	}
	o, err := q.mutOpts(body.Etag, body.Reason, body.Note, uuidPtr(body.RequestId), body.ValidateOnly)
	if err != nil {
		return err
	}
	opts := store.ImportApplyOpts{Reason: o.Reason, Note: o.Note, ValidateOnly: o.ValidateOnly, Resolutions: map[int]string{}}
	for _, r := range body.Resolutions {
		if _, dup := opts.Resolutions[r.RowNo]; dup {
			return invalid("INVALID_RESOLUTION", "row %d has more than one resolution", r.RowNo)
		}
		opts.Resolutions[r.RowNo] = strings.ToLower(r.Resolution)
	}
	p := q.pr.project()
	var job *store.ImportJobResult
	if rollback {
		job, err = s.Store.RollbackImport(q.r.Context(), p, q.pr.actor(), id, opts)
	} else {
		job, err = s.Store.ApplyImport(q.r.Context(), p, q.pr.actor(), id, opts)
	}
	if err != nil {
		return err
	}
	return s.respond(q, struct {
		Name     string                 `json:"name"`
		Done     bool                   `json:"done"`
		Response *store.ImportJobResult `json:"response"`
	}{Name: *job.Name, Done: true, Response: job})
}
