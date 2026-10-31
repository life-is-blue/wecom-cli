// Package server is the HTTP layer: custom router (for ":verb" suffixes), bearer auth,
// role gates and AIP-193 errors. All writes go through store.Apply.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/storage"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

const errDomain = "openetl"

// Server implements http.Handler.
type Server struct {
	Store *store.Store
	Log   *log.Logger
	// Storage is the blob backend of attachments; when nil it is created on first use from the
	// OPENETL_STORAGE* environment (default: fs under ./data/blobs).
	Storage  storage.Storage
	storOnce sync.Once
	storErr  error
	quota    uploadQuota

	// AccessLog, when set, receives one JSON line per request (see metrics.go).
	AccessLog io.Writer
	logMu     sync.Mutex
	metrics   *Metrics
	mxOnce    sync.Once
}

// New creates the server handler.
func New(st *store.Store, lg *log.Logger) *Server {
	if lg == nil {
		lg = log.Default()
	}
	return &Server{Store: st, Log: lg}
}

// Principal is the authenticated caller, with the membership of the addressed project (if any).
type Principal struct {
	Account store.Account
	Member  *store.Membership
	Channel string
}

func (p *Principal) actor() core.Actor {
	return core.Actor{Account: p.Account.UserName, Side: p.Member.Side, Role: p.Member.Role, Channel: p.Channel}
}

func (p *Principal) project() store.Project {
	return store.Project{ID: p.Member.ProjectID, Name: p.Member.ProjectName}
}

// ---------------------------------------------------------------- errors

func httpStatus(status string) int {
	switch status {
	case core.StatusInvalidArgument, core.StatusFailedPrecondition:
		return http.StatusBadRequest
	case core.StatusUnauthenticated:
		return http.StatusUnauthorized
	case core.StatusPermissionDenied:
		return http.StatusForbidden
	case core.StatusNotFound:
		return http.StatusNotFound
	case core.StatusAlreadyExists, core.StatusAborted:
		return http.StatusConflict
	case core.StatusResourceExhausted:
		return http.StatusTooManyRequests
	case core.StatusUnimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Log.Printf("write response: %v", err)
	}
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	var ce *core.Error
	if !errors.As(err, &ce) {
		s.Log.Printf("internal error: %v", err)
		ce = core.NewError(core.StatusInternal, "INTERNAL", "internal error")
	} else if ce.Status == core.StatusInternal {
		s.Log.Printf("internal error: %s", ce.Message)
		ce = core.NewError(core.StatusInternal, "INTERNAL", "internal error")
	}
	code := httpStatus(ce.Status)
	if rec, ok := w.(*statusRecorder); ok {
		rec.reason = ce.Reason
	}
	var out api.Error
	out.Error.Code = code
	out.Error.Status = api.ErrorErrorStatus(ce.Status)
	out.Error.Message = ce.Message
	info := api.ErrorInfo{Type: "ErrorInfo", Reason: ce.Reason, Domain: errDomain}
	if len(ce.Metadata) > 0 {
		md := ce.Metadata
		info.Metadata = &md
	}
	out.Error.Details = &[]api.ErrorInfo{info}
	if ra := ce.Metadata["retry_after_seconds"]; ra != "" && ce.Status == core.StatusResourceExhausted {
		w.Header().Set("Retry-After", ra)
	}
	s.writeJSON(w, code, out)
}

func invalid(reason, format string, args ...any) *core.Error {
	return core.NewError(core.StatusInvalidArgument, reason, format, args...)
}

func notFound(what string) *core.Error {
	return core.NewError(core.StatusNotFound, "NOT_FOUND", "%s not found", what)
}

// ---------------------------------------------------------------- routing

type request struct {
	w       http.ResponseWriter
	r       *http.Request
	segs    []string // path segments after /v1
	verb    string
	project string
	pr      *Principal

	rawBody bytes.Buffer // body bytes read by decodeBody, for the idempotency fingerprint
}

// fingerprint identifies the request payload: method, path, query and body, without the fields that
// may legitimately differ on a retry (request_id, validate_only, etag).
func (q *request) fingerprint() string {
	skip := map[string]bool{"request_id": true, "validate_only": true, "etag": true}
	h := sha256.New()
	fmt.Fprintf(h, "%s %s", q.r.Method, q.r.URL.Path)
	qv := q.r.URL.Query()
	keys := make([]string, 0, len(qv))
	for k := range qv {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%q", k, qv[k])
	}
	var body map[string]any
	if q.rawBody.Len() > 0 && json.Unmarshal(q.rawBody.Bytes(), &body) == nil {
		for k := range skip {
			delete(body, k)
		}
		b, _ := json.Marshal(body) // map keys are sorted
		h.Write([]byte{0})
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func splitPath(path string) (segs []string, verb string, ok bool) {
	rest, found := strings.CutPrefix(path, "/v1/")
	if !found {
		return nil, "", false
	}
	segs = strings.Split(strings.Trim(rest, "/"), "/")
	last := segs[len(segs)-1]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		segs[len(segs)-1], verb = last[:i], last[i+1:]
	}
	for _, sg := range segs {
		if sg == "" {
			return nil, "", false
		}
	}
	return segs, verb, true
}

// serveHTTP authenticates and dispatches; ServeHTTP (metrics.go) wraps it with metrics and the access log.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Printf("panic: %v\n%s", rec, debug.Stack())
			s.writeErr(w, core.NewError(core.StatusInternal, "INTERNAL", "internal error"))
		}
	}()
	if r.URL.Path == "/healthz" {
		s.writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, storage.BlobPrefix) {
		s.serveBlob(w, r) // authenticated by its URL signature, not by bearer token
		return
	}
	segs, verb, ok := splitPath(r.URL.Path)
	if !ok {
		s.writeErr(w, notFound("route"))
		return
	}
	account, err := s.authenticate(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	noteAccount(r, account.UserName)
	req := &request{w: w, r: r, segs: segs, verb: verb, pr: &Principal{Account: *account, Channel: channelOf(r)}}
	if segs[0] == "projects" && len(segs) >= 2 {
		req.project = segs[1]
	}
	if err := s.route(req); err != nil {
		s.writeErr(w, err)
	}
}

func channelOf(r *http.Request) string {
	if strings.EqualFold(r.Header.Get("X-Openetl-Channel"), "cli") {
		return core.ChannelCLI
	}
	return core.ChannelAPI
}

func (s *Server) authenticate(r *http.Request) (*store.Account, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(tok) == "" {
		return nil, core.NewError(core.StatusUnauthenticated, "UNAUTHENTICATED", "missing bearer token")
	}
	acc, ok, err := s.Store.AuthenticateToken(r.Context(), strings.TrimSpace(tok))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, core.NewError(core.StatusUnauthenticated, "UNAUTHENTICATED", "invalid, expired or revoked token")
	}
	return acc, nil
}

func (s *Server) route(q *request) error {
	m := q.r.Method
	n := len(q.segs)
	switch q.segs[0] {
	case "me":
		if n == 1 && m == http.MethodGet && q.verb == "" {
			return s.getMe(q)
		}
		return notFound("route")
	case "accounts":
		return s.routeAccounts(q)
	case "projects":
	default:
		return notFound("route")
	}
	if n < 2 {
		return notFound("route")
	}

	member, ok, err := s.Store.MemberOf(q.r.Context(), q.project, q.pr.Account.UserName)
	if err != nil {
		return err
	}
	if !ok {
		return core.NewError(core.StatusPermissionDenied, "NOT_A_MEMBER", "not a member of project %q", q.project)
	}
	q.pr.Member = member
	write := m == http.MethodPost || m == http.MethodPatch || m == http.MethodPut || m == http.MethodDelete
	if n == 2 && q.verb == "query" {
		write = false // read-only SQL over views
	}
	if write && member.Role == core.RoleViewer {
		return core.NewError(core.StatusPermissionDenied, "ROLE_INSUFFICIENT", "viewers have read-only access")
	}

	if n == 2 {
		if q.verb == "query" && m == http.MethodPost {
			return s.query(q)
		}
		return notFound("route")
	}
	switch q.segs[2] {
	case "workflows":
		return s.routeWorkflows(q)
	case "tasks":
		return s.routeTasks(q)
	case "importJobs":
		return s.routeImportJobs(q)
	case "members", "config":
		// Stages, reasons and rules are not secret and every member needs them (e.g. to find a valid reason
		// key); settings can reference secrets, so non-admins get everything except settings.
		if q.segs[2] == "config" && len(q.segs) == 3 && q.r.Method == http.MethodGet && member.Role != core.RoleAdmin {
			c, err := s.Store.GetConfig(q.r.Context(), q.pr.project())
			if err != nil {
				return err
			}
			c.Settings = nil
			return s.respond(q, c)
		}
		if member.Role != core.RoleAdmin {
			return core.NewError(core.StatusPermissionDenied, "ROLE_INSUFFICIENT", "this call requires the admin role")
		}
		return s.routeAdmin(q)
	}
	return notFound("route")
}

func methodNotAllowed(q *request) error {
	return core.NewError(core.StatusInvalidArgument, "METHOD_NOT_ALLOWED", "method %s is not supported on %s", q.r.Method, q.r.URL.Path)
}

func (s *Server) routeWorkflows(q *request) error {
	m, n := q.r.Method, len(q.segs)
	switch n {
	case 3:
		if q.verb != "" {
			return notFound("route")
		}
		switch m {
		case http.MethodGet:
			return s.listWorkflows(q)
		case http.MethodPost:
			return s.createWorkflow(q)
		}
		return methodNotAllowed(q)
	case 4:
		id, ok := store.ParseID(store.PrefixWorkflow, q.segs[3])
		if !ok {
			return notFound("workflow")
		}
		switch q.verb {
		case "":
			switch m {
			case http.MethodGet:
				return s.getWorkflow(q, id)
			case http.MethodPatch:
				return s.updateWorkflow(q, id)
			case http.MethodDelete:
				return s.setDeleted(q, id, "workflow", true)
			}
		case "undelete":
			if m == http.MethodPost {
				return s.setDeleted(q, id, "workflow", false)
			}
		case "advanceStage":
			if m == http.MethodPost {
				return s.advanceStage(q, id)
			}
		case "regressStage":
			if m == http.MethodPost {
				return s.regressStage(q, id)
			}
		case "setScope":
			if m == http.MethodPost {
				return s.setScope(q, id)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	}
	// nested collections
	wf, ok := store.ParseID(store.PrefixWorkflow, q.segs[3])
	if !ok {
		return notFound("workflow")
	}
	switch q.segs[4] {
	case "comments":
		return s.routeComments(q, wf)
	case "events":
		return s.routeEvents(q, wf)
	case "attachments":
		return s.routeAttachments(q, wf)
	}
	return notFound("route")
}

func (s *Server) routeTasks(q *request) error {
	m, n := q.r.Method, len(q.segs)
	switch n {
	case 3:
		if q.verb != "" {
			return notFound("route")
		}
		switch m {
		case http.MethodGet:
			return s.listTasks(q)
		case http.MethodPost:
			return s.createTask(q)
		}
		return methodNotAllowed(q)
	case 4:
		id, ok := store.ParseID(store.PrefixTask, q.segs[3])
		if !ok {
			return notFound("task")
		}
		switch q.verb {
		case "":
			switch m {
			case http.MethodGet:
				return s.getTask(q, id)
			case http.MethodPatch:
				return s.updateTask(q, id)
			case http.MethodDelete:
				return s.setDeleted(q, id, "task", true)
			}
		case "undelete":
			if m == http.MethodPost {
				return s.setDeleted(q, id, "task", false)
			}
		case "assign":
			if m == http.MethodPost {
				return s.assignTask(q, id)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	}
	return notFound("route")
}

// ---------------------------------------------------------------- request helpers

const maxBody = 4 << 20

// decodeBody reads a JSON body into v; an empty body is allowed when optional.
func (q *request) decodeBody(v any, optional bool) error {
	body := http.MaxBytesReader(q.w, q.r.Body, maxBody)
	q.rawBody.Reset()
	dec := json.NewDecoder(io.TeeReader(body, &q.rawBody))
	err := dec.Decode(v)
	if err != nil {
		if errors.Is(err, io.EOF) && optional {
			return nil
		}
		return invalid("INVALID_BODY", "invalid JSON body: %v", err)
	}
	return nil
}

func (q *request) query(name string) string { return q.r.URL.Query().Get(name) }

func (q *request) queryBool(name string) (bool, error) {
	v := q.query(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, invalid("INVALID_ARGUMENT", "%s must be true or false", name)
	}
	return b, nil
}

func (q *request) listOpts() (store.ListOpts, error) {
	o := store.ListOpts{Filter: q.query("filter"), OrderBy: q.query("order_by"), PageToken: q.query("page_token")}
	if v := q.query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return o, invalid("INVALID_PAGE_SIZE", "page_size must be a non-negative integer")
		}
		o.PageSize = n
	}
	var err error
	if o.ShowDeleted, err = q.queryBool("show_deleted"); err != nil {
		return o, err
	}
	if v := q.query("as_of"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return o, invalid("INVALID_AS_OF", "as_of must be an RFC 3339 timestamp")
		}
		o.AsOf = t.UTC()
	}
	return o, nil
}

// mutOpts are the options shared by all mutating calls (body fields win over query parameters).
type mutOpts struct {
	Etag, Reason, Note, RequestID string
	ValidateOnly                  bool
	Evidence                      []string // resolved attachment names, see evidence()
}

// evidence validates the evidence_attachments of a request against the workflow and keeps the
// canonical names in the options. Called right before apply, after the handler's other argument checks.
func (s *Server) evidence(q *request, o *mutOpts, wf int64, refs *[]string) error {
	if refs == nil || len(*refs) == 0 {
		return nil
	}
	names, err := s.Store.ResolveEvidence(q.r.Context(), q.pr.project(), wf, *refs)
	o.Evidence = names
	return err
}

func (q *request) mutOpts(etag, reason, note *string, requestID *string, validateOnly *bool) (mutOpts, error) {
	o := mutOpts{Etag: q.query("etag"), Reason: q.query("reason"), Note: q.query("note"), RequestID: q.query("request_id")}
	var err error
	if o.ValidateOnly, err = q.queryBool("validate_only"); err != nil {
		return o, err
	}
	if etag != nil && *etag != "" {
		o.Etag = *etag
	}
	if reason != nil && *reason != "" {
		o.Reason = *reason
	}
	if note != nil && *note != "" {
		o.Note = *note
	}
	if requestID != nil && *requestID != "" {
		o.RequestID = *requestID
	}
	if validateOnly != nil && *validateOnly {
		o.ValidateOnly = true
	}
	if len(o.RequestID) > 64 {
		return o, invalid("INVALID_REQUEST_ID", "request_id must be at most 64 characters")
	}
	return o, nil
}

// apply runs the write primitive for the current principal.
func (s *Server) apply(q *request, typ string, id int64, o mutOpts, plan func(*store.Entity, core.Env) ([]store.PlannedChange, error), eventTime time.Time) (*store.Result, error) {
	res, err := s.Store.Apply(q.r.Context(), store.ChangeRequest{
		ProjectID: q.pr.Member.ProjectID, EntityType: typ, EntityID: id, Actor: q.pr.actor(),
		ReasonKey: o.Reason, Note: o.Note, RequestID: o.RequestID, Etag: o.Etag, ValidateOnly: o.ValidateOnly, Fingerprint: q.fingerprint(), Evidence: o.Evidence,
		EventTime: eventTime, Plan: plan,
	})
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Reason == "ETAG_MISMATCH" && id != 0 {
			name := q.pr.project().WorkflowName(id)
			if typ == "task" {
				name = q.pr.project().TaskName(id)
			}
			ce.With("name", name)
		}
		return nil, err
	}
	return res, nil
}

func (s *Server) respond(q *request, v any) error {
	s.writeJSON(q.w, http.StatusOK, v)
	return nil
}

var timeZero time.Time

func uuidPtr(u *openapi_types.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}
