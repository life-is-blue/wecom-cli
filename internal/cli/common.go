package cli

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ref resolves a user-supplied resource reference. kind is the collection
// segment ("workflows", "tasks"). The input may be a bare id (WF-000123 or
// wf-000123) or a full resource name (projects/p/workflows/wf-000123). It
// returns the project and the canonical lowercase id.
func (a *App) ref(kind, in string) (project, id string, err error) {
	in = strings.TrimSpace(in)
	if strings.Contains(in, "/") {
		segs := strings.Split(strings.Trim(in, "/"), "/")
		for i := 0; i+1 < len(segs); i++ {
			if segs[i] == kind {
				id = segs[i+1]
				if len(segs) >= 2 && segs[0] == "projects" {
					project = segs[1]
				}
				break
			}
		}
		if id == "" {
			return "", "", usagef("%q is not a valid %s resource name", in, kind)
		}
	} else {
		id = in
	}
	id = strings.ToLower(id)
	if !idRe.MatchString(id) {
		return "", "", usagef("invalid id %q", in)
	}
	if project != "" {
		if a.flagProject != "" && a.flagProject != project {
			return "", "", usagef("--project %q conflicts with project %q in %q", a.flagProject, project, in)
		}
		return project, id, nil
	}
	project, err = a.projectID()
	return project, id, err
}

// fullName returns projects/{p}/{kind}/{id} for the given reference.
func (a *App) fullName(kind, in string) (string, error) {
	p, id, err := a.ref(kind, in)
	if err != nil {
		return "", err
	}
	return "projects/" + p + "/" + kind + "/" + id, nil
}

// itemPath is /v1/projects/{p}/{kind}/{id}.
func (a *App) itemPath(kind, in string) (string, error) {
	p, id, err := a.ref(kind, in)
	if err != nil {
		return "", err
	}
	return "/v1/projects/" + p + "/" + kind + "/" + id, nil
}

func (a *App) collectionPath(kind string) (string, error) {
	p, err := a.projectID()
	if err != nil {
		return "", err
	}
	return "/v1/projects/" + p + "/" + kind, nil
}

// ---------------------------------------------------------------- write flags

type writeFlags struct {
	dryRun    bool
	etag      string
	requestID string
	reason    string
	note      string
}

func (w *writeFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVar(&w.dryRun, "dry-run", false, "validate only (validate_only); nothing is written")
	f.StringVar(&w.etag, "etag", "", "expected etag of the target (optimistic concurrency)")
	f.StringVar(&w.requestID, "request-id", "", "idempotency key, a UUID (default: generated)")
	f.StringVar(&w.reason, "reason", "", "reason_key from the project config")
	f.StringVar(&w.note, "note", "", "free-text note recorded with the change")
}

func (w *writeFlags) uuid() (uuid.UUID, error) {
	if w.requestID == "" {
		return uuid.New(), nil
	}
	id, err := uuid.Parse(w.requestID)
	if err != nil {
		return uuid.Nil, usagef("--request-id must be a UUID: %v", err)
	}
	return id, nil
}

func (w *writeFlags) options() (api.MutationOptions, error) {
	id, err := w.uuid()
	if err != nil {
		return api.MutationOptions{}, err
	}
	o := api.MutationOptions{RequestId: &id}
	if w.etag != "" {
		o.Etag = &w.etag
	}
	if w.reason != "" {
		o.Reason = &w.reason
	}
	if w.note != "" {
		o.Note = &w.note
	}
	if w.dryRun {
		t := true
		o.ValidateOnly = &t
	}
	return o, nil
}

// noBody rejects --etag/--reason/--note on calls whose request has nowhere to
// carry them (create). The flags exist for a uniform write surface.
func (w *writeFlags) noBody(what string) error {
	if w.etag != "" || w.reason != "" || w.note != "" {
		return usagef("--etag, --reason and --note are not supported by %s (the API has no field for them)", what)
	}
	return nil
}

// query returns request_id / validate_only (and etag when withEtag) as query
// parameters, for endpoints that take them there instead of in a body.
func (w *writeFlags) query(withEtag bool) (url.Values, error) {
	id, err := w.uuid()
	if err != nil {
		return nil, err
	}
	q := url.Values{"request_id": {id.String()}}
	if w.dryRun {
		q.Set("validate_only", "true")
	}
	if withEtag && w.etag != "" {
		q.Set("etag", w.etag)
	}
	return q, nil
}

// ---------------------------------------------------------------- helpers

func ptr[T any](v T) *T { return &v }

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseKV(items []string, flag string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(items))
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" {
			return nil, usagef("--%s expects key=value, got %q", flag, it)
		}
		m[k] = v
	}
	return m, nil
}

func parseAssignees(items []string) (map[string]string, error) {
	m, err := parseKV(items, "assignee")
	for k, v := range m {
		if !strings.HasPrefix(v, "accounts/") {
			m[k] = "accounts/" + v
		}
	}
	return m, err
}

// listFlags are shared by the list-style commands.
type listFlags struct {
	filter, orderBy, pageToken string
	pageSize                   int
	showDeleted                bool
}

func (l *listFlags) bind(cmd *cobra.Command, deleted bool) {
	f := cmd.Flags()
	f.StringVar(&l.filter, "filter", "", "AIP-160 filter expression")
	f.StringVar(&l.orderBy, "order-by", "", "e.g. 'create_time desc'")
	f.IntVar(&l.pageSize, "page-size", 0, "items per page (server default 50, max 1000)")
	f.StringVar(&l.pageToken, "page-token", "", "continue from a next_page_token")
	if deleted {
		f.BoolVar(&l.showDeleted, "show-deleted", false, "include soft-deleted items")
	}
}

func (l *listFlags) values(extraFilter string) url.Values {
	q := url.Values{}
	flt := l.filter
	if extraFilter != "" {
		if flt != "" {
			flt = "(" + flt + ") AND " + extraFilter
		} else {
			flt = extraFilter
		}
	}
	if flt != "" {
		q.Set("filter", flt)
	}
	if l.orderBy != "" {
		q.Set("order_by", l.orderBy)
	}
	if l.pageSize > 0 {
		q.Set("page_size", strconv.Itoa(l.pageSize))
	}
	if l.showDeleted {
		q.Set("show_deleted", "true")
	}
	return q
}

// listCall runs a GET list request with pagination support.
func (a *App) listCall(path string, l *listFlags, extraFilter string) error {
	q := l.values(extraFilter)
	return a.paginate(func(tok string) ([]byte, error) {
		if tok != "" {
			q.Set("page_token", tok)
		}
		return a.call("GET", path, q, nil)
	}, l.pageToken)
}
