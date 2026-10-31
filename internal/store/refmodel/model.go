// Package refmodel is a deliberately naive, in-memory reference implementation of the openetl
// entity/event/rule semantics, written from docs/m0-naming-spec.md and the seed rule table
// (db/migrations/0001_init.sql). It shares no code with internal/core or internal/store: no JSON,
// no SQL, no rule engine, just maps and if-statements. The differential test (diff_test.go) drives
// the same random operations through this model and through the real HTTP server + MySQL and
// compares outcomes and resulting state.
//
// Only tests import this package.
package refmodel

import (
	"fmt"
	"sort"
	"strings"
)

// Err is an expected failure: AIP-193 status plus ErrorInfo reason. nil means success.
type Err struct{ Status, Reason string }

func (e *Err) String() string {
	if e == nil {
		return "OK"
	}
	return e.Status + "/" + e.Reason
}

func e(status, reason string) *Err { return &Err{status, reason} }

// Actor is a project member as the model sees it.
type Actor struct{ Name, Side, Role string }

// Stages in order (seed stage_def).
var Stages = []string{"development", "deployment", "verify", "accept", "dual_run", "cutover"}

func stageSeq(s string) int {
	for i, x := range Stages {
		if x == s {
			return i + 1
		}
	}
	return 0
}

type reasonDef struct {
	kind string
	note bool // note required
}

// Seed reason table (all active).
var reasons = map[string]reasonDef{
	"customer_env_issue": {"gate_override", false}, "customer_source_data": {"gate_override", false},
	"customer_takeover": {"discard", false}, "duplicate": {"discard", false},
	"known_acceptable_diff": {"gate_override", true}, "lookup_static_verified": {"gate_override", true},
	"merged": {"discard", false}, "na_code_outside_repo": {"not_applicable", true},
	"na_continuous_stream": {"not_applicable", true}, "na_event_or_queue": {"not_applicable", true},
	"na_external_scheduler": {"not_applicable", true}, "na_lookup_passive": {"not_applicable", true},
	"na_manual_trigger": {"not_applicable", true}, "na_other": {"not_applicable", true},
	"no_legacy_output": {"gate_override", false}, "no_source": {"discard", false},
	"obsolete": {"discard", false}, "other_discard": {"discard", true}, "other_override": {"gate_override", true},
	"rej_business_logic": {"reject", false}, "rej_other": {"reject", true}, "rej_row_count": {"reject", false},
	"rej_value_or_schema": {"reject", false}, "rej_verification_method": {"reject", false},
	"rule_not_applicable": {"gate_override", true}, "source_retired": {"discard", false},
}

// ReasonKeys returns all seed reason keys, sorted (for generators).
func ReasonKeys() []string {
	var ks []string
	for k := range reasons {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ReasonKind returns the kind of a seed reason ("" if unknown).
func ReasonKind(k string) string { return reasons[k].kind }

// WF is one workflow. Every field is stored the way the log would store it, so that "did anything
// change" is decided on stored values: "" means "never written".
type WF struct {
	ID      int
	Name    string
	Labels  map[string]string
	Stage   map[string]string // stage -> pass|open|na ("" absent == open)
	Scope   string            // "", in, out
	Into    int               // merge target (model id), 0 none
	Deleted string            // "", true, false
	Ver     int               // number of events
	Log     []map[string]string
}

func (w *WF) clone() *WF {
	c := *w
	c.Labels = map[string]string{}
	for k, v := range w.Labels {
		c.Labels[k] = v
	}
	c.Stage = map[string]string{}
	for k, v := range w.Stage {
		c.Stage[k] = v
	}
	return &c
}

// Eff returns the effective stage state.
func (w *WF) Eff(stage string) string {
	if s := w.Stage[stage]; s != "" {
		return s
	}
	return "open"
}

// Out reports whether the workflow is out of scope.
func (w *WF) Out() bool { return w.Scope == "out" }

// IsDeleted reports soft deletion.
func (w *WF) IsDeleted() bool { return w.Deleted == "true" }

// Current is current_stage ("" when all stages are passed/NA).
func (w *WF) Current() string {
	for _, s := range Stages {
		if x := w.Eff(s); x != "pass" && x != "na" {
			return s
		}
	}
	return ""
}

// Comment is one comment.
type Comment struct {
	ID       int
	WF       int
	Kind     string
	Body     string
	Resolved string // "", true, false
	Deleted  string // "", true, false
	Ver      int
	Log      []map[string]string
}

// Model is the whole project.
type Model struct {
	WFs      map[int]*WF
	Comments map[int]*Comment
	reqs     map[string]reqRec
	next     int
}

type reqRec struct {
	typ string // workflow|comment
	id  int
	fp  string
}

// fingerprint is the payload of a request: everything except who sent it and the fields that may
// differ on a legitimate retry (request id, validate_only, etag).
func fingerprint(op Op) string {
	op.Actor, op.ReqID, op.Validate, op.Etag = Actor{}, "", false, ""
	return fmt.Sprintf("%+v", op)
}

// New returns an empty model.
func New() *Model {
	return &Model{WFs: map[int]*WF{}, Comments: map[int]*Comment{}, reqs: map[string]reqRec{}, next: 1}
}

// Sentinel ids for entities that do not exist in this project (never created, or in another project).
const (
	Missing = -1
)

// Op is one randomly generated request.
type Op struct {
	Kind    string // create|advance|regress|scope|delete|undelete|rename|label|setstages|ccreate|cresolve|creopen|cdelete
	Actor   Actor
	Target  int // workflow model id (or Missing)
	Comment int // comment model id (or Missing)

	Name     string
	Label    string
	LabelVal string // "" with Kind label = delete the label
	Stage    string
	Stages   []string // regress list / setstages names
	States   []string // setstages states: PASSED|OPEN|NOT_APPLICABLE
	Scope    string   // IN_SCOPE|OUT_OF_SCOPE
	Into     int      // merge target model id, 0 none, Missing = foreign/nonexistent
	CKind    string   // note|question|issue|risk
	CBody    string

	Reason   string
	Note     string
	Etag     string // "", cur, stale, bogus
	ReqID    string
	Validate bool
	Future   bool // comment event_time in the future
}

// Result of Do.
type Result struct {
	Err      *Err
	NoOp     bool // success, nothing recorded
	Replayed bool
	Created  int // model id of a created entity
	Entity   int // affected entity id (wf or comment), 0 none
	EntType  string
	Preview  *WF // validate_only: state the workflow would have
	Wrote    bool
	Event    map[string]string // keys written (key -> new value, "" = removed)
}

type chg struct{ key, old, new string }

// Do applies op to the model and returns what the server must answer.
func (m *Model) Do(op Op) Result {
	if op.Actor.Role == "viewer" {
		return Result{Err: e("PERMISSION_DENIED", "ROLE_INSUFFICIENT")}
	}
	switch op.Kind {
	case "create":
		return m.doCreate(op)
	case "advance":
		return m.doWF(op, func(w *WF) ([]chg, *Err) {
			return m.stageChgs(w, [][2]string{{op.Stage, "pass"}}), nil
		})
	case "regress":
		if op.Reason == "" {
			return Result{Err: e("FAILED_PRECONDITION", "REASON_REQUIRED")}
		}
		return m.doWF(op, func(w *WF) ([]chg, *Err) {
			var pairs [][2]string
			for _, s := range op.Stages {
				if w.Stage[s] != "pass" {
					return nil, e("FAILED_PRECONDITION", "NOT_REGRESSIBLE")
				}
				pairs = append(pairs, [2]string{s, "open"})
			}
			return m.stageChgs(w, pairs), nil
		})
	case "setstages":
		return m.doWF(op, func(w *WF) ([]chg, *Err) {
			var pairs [][2]string
			for i, s := range op.Stages {
				pairs = append(pairs, [2]string{s, map[string]string{"PASSED": "pass", "OPEN": "open", "NOT_APPLICABLE": "na"}[op.States[i]]})
			}
			return m.stageChgs(w, pairs), nil
		})
	case "rename":
		return m.doWF(op, func(w *WF) ([]chg, *Err) { return diff(w.Name, op.Name, "name"), nil })
	case "label":
		return m.doWF(op, func(w *WF) ([]chg, *Err) {
			old, has := w.Labels[op.Label]
			if op.LabelVal == "" { // delete
				if !has {
					return nil, nil
				}
				return []chg{{"attr." + op.Label, old, ""}}, nil
			}
			return diff(old, op.LabelVal, "attr."+op.Label), nil
		})
	case "delete", "undelete":
		return m.doWF(op, func(w *WF) ([]chg, *Err) {
			return diff(w.Deleted, map[string]string{"delete": "true", "undelete": "false"}[op.Kind], "deleted"), nil
		})
	case "scope":
		return m.doScope(op)
	case "ccreate":
		return m.doCommentCreate(op)
	case "cresolve", "creopen", "cdelete":
		return m.doCommentMut(op)
	}
	panic("unknown op " + op.Kind)
}

func diff(old, new, key string) []chg {
	if old == new {
		return nil
	}
	return []chg{{key, old, new}}
}

func (m *Model) stageChgs(w *WF, pairs [][2]string) []chg {
	var out []chg
	seen := map[string]int{}
	for _, p := range pairs {
		if i, ok := seen[p[0]]; ok { // the last value for a key wins
			out[i].new = p[1]
			continue
		}
		seen[p[0]] = len(out)
		out = append(out, chg{"stage." + p[0], w.Stage[p[0]], p[1]})
	}
	kept := out[:0]
	for _, c := range out {
		if c.old != c.new {
			kept = append(kept, c)
		}
	}
	return kept
}

// ---------------------------------------------------------------- generic write pipeline

// pipeline is the order in which a write is judged. entity existence -> replay -> etag -> plan ->
// no-op -> validation -> commit. apply mutates the entity (workflow or comment) on commit.
type target struct {
	typ  string
	id   int // 0 = create
	wf   *WF
	cm   *Comment
	ver  func() int
	logf func(map[string]string)
}

func (m *Model) pipeline(op Op, t target, plan func() ([]chg, *Err), validate func([]chg) *Err, commit func([]chg)) Result {
	res := Result{EntType: t.typ}
	if t.id != 0 {
		res.Entity = t.id
	}
	// replay
	if op.ReqID != "" {
		if rec, ok := m.reqs[op.ReqID]; ok {
			if t.id != 0 && rec.id != t.id {
				res.Err = e("ALREADY_EXISTS", "REQUEST_ID_REUSED") // another entity
				return res
			}
			if rec.fp != fingerprint(op) {
				res.Err = e("ALREADY_EXISTS", "IDEMPOTENCY_KEY_REUSED") // same key, different payload
				return res
			}
			if rec.typ != t.typ {
				res.Err = e("ALREADY_EXISTS", "REQUEST_ID_REUSED")
				return res
			}
			res.Replayed, res.Entity = true, rec.id
			return res
		}
	}
	if t.id != 0 && op.Etag != "" {
		if op.Etag != "cur" {
			res.Err = e("ABORTED", "ETAG_MISMATCH")
			return res
		}
	}
	changes, perr := plan()
	if perr != nil {
		res.Err = perr
		return res
	}
	if len(changes) == 0 {
		res.NoOp = true
		return res
	}
	if err := validate(changes); err != nil {
		res.Err = err
		return res
	}
	ev := map[string]string{}
	for _, c := range changes {
		ev[c.key] = c.new
	}
	res.Event = ev
	if op.Validate {
		return res // caller builds the preview
	}
	commit(changes)
	res.Wrote = true
	return res
}

// checkReason: reason existence and note requirement, applied to every event that names a reason.
func checkReason(reason, note string) *Err {
	if reason == "" {
		return nil
	}
	r, ok := reasons[reason]
	if !ok {
		return e("INVALID_ARGUMENT", "UNKNOWN_REASON")
	}
	if r.note && strings.TrimSpace(note) == "" {
		return e("FAILED_PRECONDITION", "REASON_REQUIRED")
	}
	return nil
}

func kindOf(reason string) string { return reasons[reason].kind }

// ---------------------------------------------------------------- workflows

func (m *Model) doCreate(op Op) Result {
	id := m.next
	w := &WF{ID: id, Name: op.Name, Labels: map[string]string{}, Stage: map[string]string{}}
	var pairs [][2]string
	for i, s := range op.Stages {
		pairs = append(pairs, [2]string{s, map[string]string{"PASSED": "pass", "OPEN": "open", "NOT_APPLICABLE": "na"}[op.States[i]]})
	}
	t := target{typ: "workflow"}
	res := m.pipeline(op, t,
		func() ([]chg, *Err) {
			ch := []chg{{"name", "", op.Name}}
			if op.Label != "" {
				ch = append(ch, chg{"attr." + op.Label, "", op.LabelVal})
			}
			return append(ch, m.stageChgs(w, pairs)...), nil
		},
		func(ch []chg) *Err { return m.validateWF(op, w, ch) },
		func(ch []chg) {
			m.next++
			m.WFs[id] = w
			applyWF(w, ch)
			w.Ver = 1
			w.Log = append(w.Log, logOf(ch))
			if op.ReqID != "" {
				m.reqs[op.ReqID] = reqRec{"workflow", id, fingerprint(op)}
			}
		})
	if res.Err == nil && !res.Replayed {
		res.Entity = id
		if res.Wrote {
			res.Created = id
		}
		if op.Validate {
			p := w.clone()
			applyWF(p, nil)
			// the preview has the changes applied; rebuild them from the event
			for k, v := range res.Event {
				setKey(p, k, v)
			}
			res.Preview = p
			res.Entity = 0
		}
	}
	return res
}

func (m *Model) doWF(op Op, plan func(w *WF) ([]chg, *Err)) Result {
	w := m.WFs[op.Target]
	if w == nil {
		return Result{Err: e("NOT_FOUND", "NOT_FOUND")}
	}
	res := m.pipeline(op, target{typ: "workflow", id: w.ID},
		func() ([]chg, *Err) { return plan(w) },
		func(ch []chg) *Err { return m.validateWF(op, w, ch) },
		func(ch []chg) {
			applyWF(w, ch)
			w.Ver++
			w.Log = append(w.Log, logOf(ch))
			if op.ReqID != "" {
				m.reqs[op.ReqID] = reqRec{"workflow", w.ID, fingerprint(op)}
			}
		})
	if res.Err == nil && op.Validate && res.Event != nil {
		p := w.clone()
		for k, v := range res.Event {
			setKey(p, k, v)
		}
		res.Preview = p
	}
	return res
}

func logOf(ch []chg) map[string]string {
	l := map[string]string{}
	for _, c := range ch {
		l[c.key] = c.new
	}
	return l
}

func applyWF(w *WF, ch []chg) {
	for _, c := range ch {
		setKey(w, c.key, c.new)
	}
}

// setKey writes one stored key (value "" = key removed). Into is carried as the decimal model id.
func setKey(w *WF, k, v string) {
	switch {
	case k == "name":
		w.Name = v
	case strings.HasPrefix(k, "attr."):
		if v == "" {
			delete(w.Labels, strings.TrimPrefix(k, "attr."))
		} else {
			w.Labels[strings.TrimPrefix(k, "attr.")] = v
		}
	case strings.HasPrefix(k, "stage."):
		w.Stage[strings.TrimPrefix(k, "stage.")] = v
	case k == "scope":
		w.Scope = v
	case k == "into":
		w.Into = 0
		if v != "" {
			fmt.Sscanf(v, "%d", &w.Into)
		}
	case k == "deleted":
		w.Deleted = v
	}
}

// doScope: setScope. Handler-level argument checks come first, then the pipeline.
func (m *Model) doScope(op Op) Result {
	mergedReq := op.Reason == "merged"
	switch op.Scope {
	case "IN_SCOPE":
		if op.Into != 0 {
			return Result{Err: e("INVALID_ARGUMENT", "INVALID_MERGED_INTO")}
		}
	case "OUT_OF_SCOPE":
		if mergedReq && op.Into == 0 {
			return Result{Err: e("INVALID_ARGUMENT", "MERGE_TARGET_REQUIRED")}
		}
		if !mergedReq && op.Into != 0 {
			return Result{Err: e("INVALID_ARGUMENT", "INVALID_MERGED_INTO")}
		}
	}
	return m.doWF(op, func(w *WF) ([]chg, *Err) {
		var ch []chg
		if op.Scope == "IN_SCOPE" {
			ch = append(diff(w.Scope, "in", "scope"), diff(itoa(w.Into), "", "into")...)
		} else {
			into := ""
			if op.Into != 0 {
				into = itoa(op.Into)
			}
			ch = append(diff(w.Scope, "out", "scope"), diff(itoa(w.Into), into, "into")...)
		}
		return ch, nil
	})
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

// ---------------------------------------------------------------- the rules

// mayPass: who may set stage <s> to pass (seed rules; admins may do anything).
func mayPass(s string, a Actor) bool {
	if a.Role == "admin" {
		return true
	}
	if s == "accept" {
		return a.Side == "customer"
	}
	return a.Side == "delivery"
}

// mayReopen: who may set stage <s> back to open (with a reject reason).
func mayReopen(s string, a Actor) bool {
	if a.Role == "admin" {
		return true
	}
	switch s {
	case "accept":
		return a.Side == "customer"
	case "verify":
		return true // delivery or customer
	}
	return a.Side == "delivery"
}

func (m *Model) live(id int) bool {
	w := m.WFs[id]
	return w != nil && !w.IsDeleted()
}

// validateWF judges the changes of one event in order. Within one change the structural checks come
// before the rule-table checks; the first failing change wins.
func (m *Model) validateWF(op Op, w *WF, changes []chg) *Err {
	// spec: a soft-deleted workflow accepts nothing but the change that restores it
	if w.IsDeleted() {
		restores := false
		for _, c := range changes {
			if c.key == "deleted" && c.new != "true" {
				restores = true
			}
		}
		if !restores {
			return e("FAILED_PRECONDITION", "ENTITY_DELETED")
		}
	}
	if err := checkReason(op.Reason, op.Note); err != nil {
		return err
	}
	after := w.clone()
	for _, c := range changes {
		setKey(after, c.key, c.new)
	}
	rk := kindOf(op.Reason)
	for _, c := range changes {
		switch {
		case strings.HasPrefix(c.key, "stage."):
			s := strings.TrimPrefix(c.key, "stage.")
			if stageSeq(s) == 0 {
				return e("INVALID_ARGUMENT", "UNKNOWN_STAGE")
			}
			switch c.new {
			case "pass":
				for _, earlier := range Stages[:stageSeq(s)-1] {
					if x := after.Eff(earlier); x != "pass" && x != "na" {
						return e("FAILED_PRECONDITION", "STAGE_ORDER_VIOLATION")
					}
				}
				if !mayPass(s, op.Actor) {
					return e("PERMISSION_DENIED", "SIDE_NOT_ALLOWED")
				}
			case "open":
				if c.old == "pass" && rk != "reject" {
					return e("FAILED_PRECONDITION", "REASON_REQUIRED")
				}
				if !mayReopen(s, op.Actor) {
					return e("PERMISSION_DENIED", "SIDE_NOT_ALLOWED")
				}
				if rk != "reject" {
					return e("FAILED_PRECONDITION", "REASON_REQUIRED")
				}
			case "na":
				if c.old == "pass" { // a PASSED stage only leaves PASSED through a reject reason (regress) and
					// N/A needs another kind of reason, so PASSED -> N/A is impossible in one step
					return e("FAILED_PRECONDITION", "REASON_REQUIRED")
				}
				if rk != "not_applicable" {
					return e("FAILED_PRECONDITION", "REASON_REQUIRED")
				}
			}
		case c.key == "scope":
			if c.new == "out" {
				if rk != "discard" {
					return e("FAILED_PRECONDITION", "REASON_REQUIRED")
				}
				if op.Reason == "merged" {
					if err := m.mergeCheck(w.ID, after.Into); err != nil {
						return err
					}
				}
			}
		case c.key == "into":
			if c.new != "" {
				if err := m.mergeCheck(w.ID, after.Into); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// mergeCheck: target must be a live workflow of this project, not the workflow itself, and the
// chain of merges starting at the target must not lead back to the workflow.
func (m *Model) mergeCheck(self, target int) *Err {
	if target == 0 {
		return e("INVALID_ARGUMENT", "MERGE_TARGET_REQUIRED")
	}
	if self != 0 && target == self {
		return e("FAILED_PRECONDITION", "MERGE_CYCLE")
	}
	if !m.live(target) {
		return e("INVALID_ARGUMENT", "MERGE_TARGET_INVALID")
	}
	cur := target
	for i := 0; i < 1000; i++ {
		w := m.WFs[cur]
		if w == nil || !w.Out() || w.Into == 0 {
			return nil
		}
		if w.Into == self {
			return e("FAILED_PRECONDITION", "MERGE_CYCLE")
		}
		cur = w.Into
	}
	return e("FAILED_PRECONDITION", "MERGE_CYCLE")
}

// ---------------------------------------------------------------- comments

func (m *Model) doCommentCreate(op Op) Result {
	w := m.WFs[op.Target]
	if w == nil || w.IsDeleted() {
		return Result{Err: e("NOT_FOUND", "NOT_FOUND")}
	}
	id := m.next
	c := &Comment{ID: id, WF: w.ID}
	res := m.pipeline(op, target{typ: "comment"},
		func() ([]chg, *Err) {
			return []chg{{"on", "", itoa(w.ID)}, {"kind", "", op.CKind}, {"body", "", op.CBody}}, nil
		},
		func(ch []chg) *Err {
			if err := checkReason(op.Reason, op.Note); err != nil {
				return err
			}
			if op.Future {
				return e("INVALID_ARGUMENT", "EVENT_TIME_IN_FUTURE")
			}
			return nil
		},
		func(ch []chg) {
			m.next++
			c.Kind, c.Body, c.Ver = op.CKind, op.CBody, 1
			c.Log = append(c.Log, logOf(ch))
			m.Comments[id] = c
			if op.ReqID != "" {
				m.reqs[op.ReqID] = reqRec{"comment", id, fingerprint(op)}
			}
		})
	if res.Err == nil && !res.Replayed {
		res.Entity = id
		if res.Wrote {
			res.Created = id
		}
		if op.Validate {
			res.Entity = 0
		}
	}
	return res
}

func (m *Model) doCommentMut(op Op) Result {
	c := m.Comments[op.Comment]
	if c == nil {
		return Result{Err: e("NOT_FOUND", "NOT_FOUND")}
	}
	key, val := "resolved", "true"
	switch op.Kind {
	case "creopen":
		val = "false"
	case "cdelete":
		key = "deleted"
	}
	res := m.pipeline(op, target{typ: "comment", id: c.ID},
		func() ([]chg, *Err) {
			if c.WF != op.Target || c.Deleted == "true" {
				return nil, e("NOT_FOUND", "NOT_FOUND")
			}
			if key == "resolved" {
				if (c.Resolved == "true") == (val == "true") {
					return nil, nil // already in the requested state
				}
				return []chg{{"resolved", c.Resolved, val}}, nil
			}
			return []chg{{"deleted", c.Deleted, "true"}}, nil
		},
		func(ch []chg) *Err {
			if !m.live(c.WF) { // spec: a comment of a deleted workflow is read-only
				return e("FAILED_PRECONDITION", "ENTITY_DELETED")
			}
			return checkReason(op.Reason, op.Note)
		},
		func(ch []chg) {
			if key == "resolved" {
				c.Resolved = val
			} else {
				c.Deleted = val
			}
			c.Ver++
			c.Log = append(c.Log, logOf(ch))
			if op.ReqID != "" {
				m.reqs[op.ReqID] = reqRec{"comment", c.ID, fingerprint(op)}
			}
		})
	return res
}

// ---------------------------------------------------------------- derived views

// Health is the derived health of a workflow: an unresolved issue blocks, else an unresolved risk
// puts it at risk, else normal. Deleted comments do not count.
func (m *Model) Health(wf int) string {
	issues, risks := 0, 0
	for _, c := range m.Comments {
		if c.WF == wf && c.Resolved != "true" && c.Deleted != "true" {
			switch c.Kind {
			case "issue":
				issues++
			case "risk":
				risks++
			}
		}
	}
	switch {
	case issues > 0:
		return "BLOCKED"
	case risks > 0:
		return "AT_RISK"
	}
	return "NORMAL"
}

// LiveComments returns the non-deleted comments of a workflow by id.
func (m *Model) LiveComments(wf int) []*Comment {
	var out []*Comment
	for _, c := range m.Comments {
		if c.WF == wf && c.Deleted != "true" {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WFOrMissing returns the workflow a comment belongs to (nil-safe: Missing for a nil comment).
func (c *Comment) WFOrMissing() int {
	if c == nil {
		return Missing
	}
	return c.WF
}
