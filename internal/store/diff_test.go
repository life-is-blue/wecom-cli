package store_test

// Differential test: random operation sequences are applied to the naive reference model
// (internal/store/refmodel) and to the real stack (HTTP server + store + MySQL). After every step
// both sides must agree on accept/reject and the error reason, on the state of the touched entity,
// on the number of recorded events and the keys they wrote; entity_latest must verify clean.
// Every few steps (and at the end) all entities and the SQL views are compared.
//
//	OPENETL_TEST_DSN=...            required (skipped otherwise); a private database is created on that server
//	SEED=42                         run only this seed
//	DIFF_SEEDS=200 DIFF_STEPS=2000  go bigger (default 20 seeds x 300 steps)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/server"
	"github.com/life-is-blue/openetl-cli/internal/store"
	"github.com/life-is-blue/openetl-cli/internal/store/refmodel"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func TestDifferential(t *testing.T) {
	st := freshStore(t)
	srv := httptest.NewServer(server.New(st, nil))
	defer srv.Close()

	seeds := make([]int64, 0)
	if s := os.Getenv("SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seeds = append(seeds, n)
	} else {
		for i := 1; i <= envInt("DIFF_SEEDS", 20); i++ {
			seeds = append(seeds, int64(i))
		}
	}
	steps := envInt("DIFF_STEPS", 300)
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, seed := range seeds {
		seed := seed
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h := newHarness(t, st, srv.URL, seed)
			h.run(steps)
		}()
	}
	wg.Wait()
	requireCleanLog(t, st)
	statMu.Lock()
	var keys []string
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%d ", k, stats[k])
	}
	statMu.Unlock()
	t.Logf("outcomes: %s", sb.String())
}

var (
	statMu sync.Mutex
	stats  = map[string]int{}
)

// ---------------------------------------------------------------- harness

type harness struct {
	t       testing.TB
	st      *store.Store
	url     string
	seed    int64
	project string
	proj    store.Project
	rnd     *rand.Rand
	m       *refmodel.Model
	tok     map[string]string // actor name -> token
	actors  []refmodel.Actor
	foreign int64 // a workflow of another project
	rid     map[int]int64
	mid     map[int64]int
	etags   map[int][]string // model id -> etags seen, oldest first
	last    map[int]int64    // model id -> latest event id seen
	att     map[int64]string // real workflow id -> an attachment of it (evidence)
	reqPool []string
	prev    []refmodel.Op // successful ops with request ids, for replays
	step    int
	cur     refmodel.Op
	failed  bool
}

func newHarness(t testing.TB, st *store.Store, url string, seed int64) *harness {
	h := &harness{t: t, st: st, url: url, seed: seed, rnd: rand.New(rand.NewSource(seed)), m: refmodel.New(),
		tok: map[string]string{}, rid: map[int]int64{}, mid: map[int64]int{}, etags: map[int][]string{}, last: map[int]int64{}, att: map[int64]string{}}
	h.project = fmt.Sprintf("df%d", seed)
	ctx := context.Background()
	h.actors = []refmodel.Actor{
		{Name: "d", Side: "delivery", Role: "editor"}, {Name: "c", Side: "customer", Role: "editor"},
		{Name: "v", Side: "delivery", Role: "viewer"}, {Name: "a", Side: "delivery", Role: "admin"},
		{Name: "x", Side: "customer", Role: "admin"},
	}
	for i := range h.actors {
		a := &h.actors[i]
		acct := fmt.Sprintf("%s%d", a.Name, seed)
		tok, err := st.CreateToken(ctx, h.project, acct, a.Side, a.Role, "human", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		h.tok[a.Name] = tok
	}
	other := h.project + "o"
	tok, err := st.CreateToken(ctx, other, fmt.Sprintf("o%d", seed), "delivery", "editor", "human", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := h.call(tok, "POST", "/v1/projects/"+other+"/workflows", map[string]any{"display_name": "foreign"})
	h.foreign = idOf(t, r.body["name"])
	m, _, err := st.MemberOf(ctx, h.project, fmt.Sprintf("d%d", seed))
	if err != nil {
		t.Fatal(err)
	}
	h.proj = store.Project{ID: m.ProjectID, Name: h.project}
	return h
}

type response struct {
	code int
	body map[string]any
	raw  string
}

func (r response) errKey() string {
	if r.code == 200 {
		return "OK"
	}
	e, _ := r.body["error"].(map[string]any)
	status, _ := e["status"].(string)
	reason := ""
	if d, _ := e["details"].([]any); len(d) > 0 {
		reason, _ = d[0].(map[string]any)["reason"].(string)
	}
	return status + "/" + reason
}

func (h *harness) call(token, method, path string, body any) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rs, err := http.DefaultClient.Do(req)
	if err != nil {
		h.fatalf("http: %v", err)
	}
	defer rs.Body.Close()
	raw, _ := io.ReadAll(rs.Body)
	out := response{code: rs.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (h *harness) fatalf(format string, a ...any) {
	h.t.Helper()
	h.failed = true
	h.t.Errorf("DIFFERENTIAL FAILURE seed=%d step=%d (rerun: SEED=%d DIFF_STEPS=%d)\nop=%+v\n%s", h.seed, h.step, h.seed, h.step, h.cur, fmt.Sprintf(format, a...))
	panic(failure{})
}

type failure struct{}

func idOf(t testing.TB, name any) int64 {
	s, _ := name.(string)
	i := strings.LastIndex(s, "-")
	n, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil {
		t.Fatalf("bad resource name %q", s)
	}
	return n
}

// ---------------------------------------------------------------- generator

func (h *harness) pick(p ...int) int { // weighted index
	sum := 0
	for _, w := range p {
		sum += w
	}
	n := h.rnd.Intn(sum)
	for i, w := range p {
		if n < w {
			return i
		}
		n -= w
	}
	return 0
}

func (h *harness) wfIDs() []int {
	var ids []int
	for id := range h.m.WFs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (h *harness) cmIDs() []int {
	var ids []int
	for id := range h.m.Comments {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (h *harness) tkIDs() []int {
	var ids []int
	for id := range h.m.Tasks {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// anyTask picks a task model id; now and then a missing one.
func (h *harness) anyTask() int {
	ids := h.tkIDs()
	if len(ids) == 0 || h.rnd.Intn(25) == 0 {
		return refmodel.Missing
	}
	return ids[h.rnd.Intn(len(ids))]
}

// pickEvidence sets op.Evidence for the calls that take evidence_attachments: admins use it a lot (an override
// of the other side needs it), everyone else rarely; an attachment only exists for a workflow that exists.
func (h *harness) pickEvidence(op *refmodel.Op) {
	if op.Actor.Role == "admin" {
		op.Evidence = h.pick(40, 50, 10)
	} else {
		op.Evidence = h.pick(88, 6, 6)
	}
	if op.Evidence == 1 && h.m.WFs[op.Target] == nil {
		op.Evidence = 0
	}
}

func (h *harness) anyWF() int {
	ids := h.wfIDs()
	if len(ids) == 0 || h.rnd.Intn(14) == 0 {
		return refmodel.Missing
	}
	return ids[h.rnd.Intn(len(ids))]
}

func (h *harness) liveWF() int {
	var live []int
	for _, id := range h.wfIDs() {
		if !h.m.WFs[id].IsDeleted() {
			live = append(live, id)
		}
	}
	if len(live) == 0 || h.rnd.Intn(12) == 0 {
		return h.anyWF()
	}
	return live[h.rnd.Intn(len(live))]
}

func (h *harness) reason(kinds ...string) string {
	if h.rnd.Intn(5) == 0 {
		return ""
	}
	if h.rnd.Intn(30) == 0 {
		return "no_such_reason"
	}
	var ks []string
	for _, k := range refmodel.ReasonKeys() {
		if len(kinds) == 0 {
			ks = append(ks, k)
			continue
		}
		for _, kd := range kinds {
			if refmodel.ReasonKind(k) == kd {
				ks = append(ks, k)
			}
		}
	}
	return ks[h.rnd.Intn(len(ks))]
}

func (h *harness) note() string {
	if h.rnd.Intn(3) == 0 {
		return ""
	}
	if h.rnd.Intn(8) == 0 {
		return "   " // blank: must count as no note
	}
	return "because " + strconv.Itoa(h.rnd.Intn(1000))
}

func (h *harness) stageFor(w int) string {
	if h.rnd.Intn(40) == 0 {
		return "nosuchstage"
	}
	if wf := h.m.WFs[w]; wf != nil && h.rnd.Intn(10) < 7 {
		if c := wf.Current(); c != "" {
			return c
		}
	}
	return refmodel.Stages[h.rnd.Intn(len(refmodel.Stages))]
}

func (h *harness) newReq() string {
	b := make([]byte, 16)
	h.rnd.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	u := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	h.reqPool = append(h.reqPool, u)
	return u
}

func (h *harness) gen() refmodel.Op {
	// replay of an earlier request, byte for byte
	if len(h.prev) > 0 && h.rnd.Intn(12) == 0 {
		op := h.prev[h.rnd.Intn(len(h.prev))]
		if h.rnd.Intn(3) == 0 {
			op.Validate = !op.Validate
		}
		return op
	}
	var op refmodel.Op
	op.Actor = h.actors[[]int{0, 1, 2, 3, 4}[h.pick(40, 25, 6, 15, 14)]]
	kinds := []string{"create", "advance", "regress", "setstages", "rename", "label", "scope", "delete", "undelete", "ccreate", "cresolve", "creopen", "cdelete", "tcreate", "tassign", "tdelete", "tundelete"}
	k := kinds[h.pick(9, 30, 10, 6, 3, 4, 13, 4, 3, 8, 4, 3, 3, 7, 5, 4, 4)]
	if len(h.m.WFs) < 3 {
		k = "create"
	}
	if (k == "cresolve" || k == "creopen" || k == "cdelete") && len(h.m.Comments) == 0 {
		k = "ccreate"
	}
	if (k == "tassign" || k == "tdelete" || k == "tundelete") && len(h.m.Tasks) == 0 {
		k = "tcreate"
	}
	op.Kind = k
	switch k {
	case "tcreate":
		op.Name = "task " + strconv.Itoa(h.rnd.Intn(100000))
		op.Target = h.liveWF()
		if h.rnd.Intn(4) == 0 {
			op.Reason, op.Note = h.reason(), h.note()
		}
	case "tassign", "tdelete", "tundelete":
		op.Task = h.anyTask()
		if k == "tassign" { // the other two carry no workflow: it must not differ in the request fingerprint
			op.Target = h.liveWF()
		}
		if h.rnd.Intn(4) == 0 {
			op.Reason, op.Note = h.reason(), h.note()
		}
	case "create":
		op.Name = "wf " + strconv.Itoa(h.rnd.Intn(100000))
		if h.rnd.Intn(3) == 0 {
			op.Label, op.LabelVal = "k"+strconv.Itoa(h.rnd.Intn(3)), "v"+strconv.Itoa(h.rnd.Intn(3))
		}
		if h.rnd.Intn(6) == 0 {
			h.randStages(&op, 1+h.rnd.Intn(3))
		}
		op.Reason, op.Note = h.reason(), h.note()
		if op.Reason != "" && h.rnd.Intn(2) == 0 {
			op.Reason = ""
		}
	case "advance":
		op.Target = h.liveWF()
		op.Stage = h.stageFor(op.Target)
		op.Reason, op.Note = h.reason(), h.note()
		h.pickEvidence(&op)
	case "regress":
		op.Target = h.liveWF()
		for i := 0; i <= h.rnd.Intn(3); i++ {
			if h.rnd.Intn(4) > 0 {
				if w := h.m.WFs[op.Target]; w != nil { // prefer passed stages
					var passed []string
					for _, s := range refmodel.Stages {
						if w.Stage[s] == "pass" {
							passed = append(passed, s)
						}
					}
					if len(passed) > 0 {
						op.Stages = append(op.Stages, passed[h.rnd.Intn(len(passed))])
						continue
					}
				}
			}
			op.Stages = append(op.Stages, refmodel.Stages[h.rnd.Intn(len(refmodel.Stages))])
		}
		if h.rnd.Intn(5) > 0 {
			op.Reason = h.reason("reject")
		} else {
			op.Reason = h.reason()
		}
		op.Note = h.note()
		h.pickEvidence(&op)
	case "setstages":
		op.Target = h.liveWF()
		h.randStages(&op, 1+h.rnd.Intn(3))
		if w := h.m.WFs[op.Target]; w != nil && h.rnd.Intn(2) == 0 { // aim at stages that are PASSED now
			for i := range op.Stages {
				if s := refmodel.Stages[h.rnd.Intn(len(refmodel.Stages))]; w.Stage[s] == "pass" {
					op.Stages[i] = s
				}
			}
			// reopen PASSED stages through PATCH stages (INV-31 must hold here as it does for :regressStage)
			if h.rnd.Intn(2) == 0 {
				for i, s := range op.Stages {
					if w.Stage[s] == "pass" {
						op.States[i] = "OPEN"
					}
				}
			}
		}
		switch h.rnd.Intn(3) {
		case 0:
			op.Reason = h.reason("not_applicable")
		case 1:
			op.Reason = h.reason("reject")
		default:
			op.Reason = h.reason()
		}
		op.Note = h.note()
		h.pickEvidence(&op)
	case "rename":
		op.Target = h.liveWF()
		op.Name = "renamed " + strconv.Itoa(h.rnd.Intn(5))
	case "label":
		op.Target = h.liveWF()
		op.Label = "k" + strconv.Itoa(h.rnd.Intn(3))
		if h.rnd.Intn(3) > 0 {
			op.LabelVal = "v" + strconv.Itoa(h.rnd.Intn(3))
		}
	case "scope":
		op.Target = h.anyWF()
		switch h.pick(25, 35, 25, 10, 3) {
		case 0:
			op.Scope = "IN_SCOPE"
			op.Reason = h.reason()
		case 1, 4:
			op.Scope, op.Reason = "OUT_OF_SCOPE", "merged"
			op.Into = h.anyWF()
			if h.rnd.Intn(15) == 0 {
				op.Into = 0
			}
			if h.rnd.Intn(5) == 0 {
				op.Into = op.Target // self merge
			}
		case 2:
			op.Scope, op.Reason = "OUT_OF_SCOPE", h.reason("discard")
			if op.Reason == "merged" {
				op.Reason = "obsolete"
			}
		default:
			op.Scope, op.Reason = "OUT_OF_SCOPE", h.reason()
			if op.Reason == "merged" {
				op.Reason = ""
			}
			if h.rnd.Intn(8) == 0 {
				op.Into = h.anyWF()
			}
		}
		op.Note = h.note()
		h.pickEvidence(&op)
	case "delete", "undelete":
		op.Target = h.anyWF()
		op.Reason, op.Note = h.reason(), h.note()
	case "ccreate":
		op.Target = h.liveWF()
		op.CKind = []string{"note", "question", "issue", "risk"}[h.pick(3, 2, 4, 4)]
		op.CBody = "body " + strconv.Itoa(h.rnd.Intn(1000))
		op.Future = h.rnd.Intn(40) == 0
		if h.rnd.Intn(4) == 0 {
			op.Reason, op.Note = h.reason(), h.note()
		}
	default:
		cs := h.cmIDs()
		op.Comment = cs[h.rnd.Intn(len(cs))]
		if h.rnd.Intn(30) == 0 {
			op.Comment = refmodel.Missing
		}
		op.Target = h.m.Comments[op.Comment].WFOrMissing()
		if h.rnd.Intn(12) == 0 {
			op.Target = h.anyWF() // wrong parent
		}
		if h.rnd.Intn(5) == 0 {
			op.Reason, op.Note = h.reason(), h.note()
		}
	}
	if h.rnd.Intn(10) == 0 {
		op.Validate = true
	}
	switch h.pick(70, 15, 10, 5) {
	case 1:
		op.Etag = "cur"
	case 2:
		op.Etag = "stale"
	case 3:
		op.Etag = "bogus"
	}
	if op.Kind == "create" || op.Kind == "ccreate" || op.Kind == "tcreate" {
		op.Etag = "" // not part of those calls
	}
	switch h.pick(55, 30, 15) {
	case 1:
		op.ReqID = h.newReq()
	case 2:
		if len(h.reqPool) > 0 {
			op.ReqID = h.reqPool[h.rnd.Intn(len(h.reqPool))]
		}
	}
	return op
}

func (h *harness) randStages(op *refmodel.Op, n int) {
	for i := 0; i < n; i++ {
		s := refmodel.Stages[h.rnd.Intn(len(refmodel.Stages))]
		if h.rnd.Intn(40) == 0 {
			s = "nosuchstage"
		}
		op.Stages = append(op.Stages, s)
		op.States = append(op.States, []string{"PASSED", "OPEN", "NOT_APPLICABLE"}[h.pick(6, 2, 3)])
	}
}

// ---------------------------------------------------------------- executing against the real stack

func (h *harness) wfName(model int) string {
	switch {
	case model == refmodel.Missing && h.rnd.Intn(2) == 0:
		return h.proj.WorkflowName(h.foreign) // exists, but in another project
	case model == refmodel.Missing:
		return h.proj.WorkflowName(1999999999)
	}
	return h.proj.WorkflowName(h.rid[model])
}

func (h *harness) etagFor(op refmodel.Op, entity int) string {
	hist := h.etags[entity]
	switch op.Etag {
	case "cur":
		if len(hist) > 0 {
			return hist[len(hist)-1]
		}
		return "ev-1"
	case "stale":
		if len(hist) > 1 {
			return hist[h.rnd.Intn(len(hist)-1)]
		}
		return "ev-1"
	case "bogus":
		return "ev-2000000000"
	}
	return ""
}

func (h *harness) send(op refmodel.Op) response {
	tok := h.tok[op.Actor.Name]
	base := "/v1/projects/" + h.project
	opts := func(m map[string]any, etagEntity int) map[string]any {
		if e := h.etagFor(op, etagEntity); e != "" {
			m["etag"] = e
		}
		if op.Reason != "" {
			m["reason"] = op.Reason
		}
		if op.Note != "" {
			m["note"] = op.Note
		}
		if op.ReqID != "" {
			m["request_id"] = op.ReqID
		}
		if op.Validate {
			m["validate_only"] = true
		}
		if ev := h.evidenceFor(op); ev != nil {
			m["evidence_attachments"] = ev
		}
		return m
	}
	query := func() string {
		v := []string{}
		if op.Reason != "" {
			v = append(v, "reason="+op.Reason)
		}
		if op.Note != "" {
			v = append(v, "note="+strings.ReplaceAll(op.Note, " ", "%20"))
		}
		if op.ReqID != "" {
			v = append(v, "request_id="+op.ReqID)
		}
		if op.Validate {
			v = append(v, "validate_only=true")
		}
		if e := h.etagFor(op, op.Target); e != "" && op.Kind == "delete" {
			v = append(v, "etag="+e)
		}
		if e := h.etagFor(op, op.Task); e != "" && op.Kind == "tdelete" {
			v = append(v, "etag="+e)
		}
		if len(v) == 0 {
			return ""
		}
		return "?" + strings.Join(v, "&")
	}
	stageList := func() []map[string]string {
		var l []map[string]string
		for i, s := range op.Stages {
			l = append(l, map[string]string{"stage": s, "state": op.States[i]})
		}
		return l
	}
	switch op.Kind {
	case "create":
		body := map[string]any{"display_name": op.Name}
		if op.Label != "" {
			body["labels"] = map[string]string{op.Label: op.LabelVal}
		}
		if len(op.Stages) > 0 {
			body["stages"] = stageList()
		}
		return h.call(tok, "POST", base+"/workflows"+query(), body)
	case "advance":
		return h.call(tok, "POST", h.wfPath(op)+":advanceStage", opts(map[string]any{"stage": op.Stage}, op.Target))
	case "regress":
		return h.call(tok, "POST", h.wfPath(op)+":regressStage", opts(map[string]any{"stages": op.Stages, "reason": op.Reason}, op.Target))
	case "scope":
		b := map[string]any{"scope": op.Scope}
		if op.Into != 0 {
			b["merged_into"] = h.wfName(op.Into)
		}
		return h.call(tok, "POST", h.wfPath(op)+":setScope", opts(b, op.Target))
	case "delete":
		return h.call(tok, "DELETE", h.wfPath(op)+query(), nil)
	case "undelete":
		return h.call(tok, "POST", h.wfPath(op)+":undelete", opts(map[string]any{}, op.Target))
	case "rename":
		return h.call(tok, "PATCH", h.wfPath(op)+"?update_mask=display_name",
			opts(map[string]any{"workflow": map[string]any{"display_name": op.Name}}, op.Target))
	case "label":
		w := map[string]any{}
		if op.LabelVal != "" {
			w["labels"] = map[string]string{op.Label: op.LabelVal}
		}
		return h.call(tok, "PATCH", h.wfPath(op)+"?update_mask=labels."+op.Label, opts(map[string]any{"workflow": w}, op.Target))
	case "setstages":
		return h.call(tok, "PATCH", h.wfPath(op)+"?update_mask=stages",
			opts(map[string]any{"workflow": map[string]any{"stages": stageList()}}, op.Target))
	case "tcreate":
		return h.call(tok, "POST", base+"/tasks"+query(), map[string]any{"display_name": op.Name, "workflow": h.wfName(op.Target)})
	case "tassign":
		return h.call(tok, "POST", h.tkPath(op)+":assign", opts(map[string]any{"workflow": h.wfName(op.Target)}, op.Task))
	case "tdelete":
		return h.call(tok, "DELETE", h.tkPath(op)+query(), nil)
	case "tundelete":
		return h.call(tok, "POST", h.tkPath(op)+":undelete", opts(map[string]any{}, op.Task))
	case "ccreate":
		b := map[string]any{"kind": strings.ToUpper(op.CKind), "body": op.CBody}
		if op.Future {
			b["event_time"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		}
		return h.call(tok, "POST", h.wfPath(op)+"/comments"+query(), b)
	case "cresolve", "creopen":
		verb := map[string]string{"cresolve": ":resolve", "creopen": ":reopen"}[op.Kind]
		return h.call(tok, "POST", h.cmPath(op)+verb, opts(map[string]any{}, op.Comment))
	case "cdelete":
		q := query()
		if e := h.etagFor(op, op.Comment); e != "" {
			if q == "" {
				q = "?etag=" + e
			} else {
				q += "&etag=" + e
			}
		}
		return h.call(tok, "DELETE", h.cmPath(op)+q, nil)
	}
	panic("op " + op.Kind)
}

func (h *harness) wfPath(op refmodel.Op) string { return "/v1/" + h.wfName(op.Target) }

func (h *harness) tkName(model int) string {
	if model == refmodel.Missing {
		return h.proj.TaskName(1999999997)
	}
	return h.proj.TaskName(h.rid[model])
}

func (h *harness) tkPath(op refmodel.Op) string { return "/v1/" + h.tkName(op.Task) }

// evidenceFor returns the evidence_attachments of the request: an attachment of the target workflow (made on
// first use, straight through the store: uploading is not what is under test) or one that does not exist.
func (h *harness) evidenceFor(op refmodel.Op) []string {
	switch op.Evidence {
	case 1:
		wf := h.rid[op.Target]
		if _, ok := h.att[wf]; !ok {
			res, err := h.st.Apply(context.Background(), store.ChangeRequest{ProjectID: h.proj.ID, EntityType: "attachment",
				Actor: core.Actor{Account: "x", Side: "system", Role: "editor", Channel: core.ChannelLegacy},
				Plan: func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
					return []store.PlannedChange{{Key: "on", New: core.JSON(wf)}, {Key: "filename", New: core.JSON("proof.txt")}}, nil
				}})
			if err != nil {
				h.fatalf("evidence attachment: %v", err)
			}
			h.att[wf] = store.FormatID(store.PrefixAttach, res.EntityID)
		}
		return []string{h.att[wf]}
	case 2:
		return []string{"at-999999"}
	}
	return nil
}

func (h *harness) cmPath(op refmodel.Op) string {
	id := int64(1999999998)
	if op.Comment != refmodel.Missing {
		id = h.rid[op.Comment]
	}
	return h.wfPath(op) + "/comments/" + store.FormatID(store.PrefixComment, id)
}

// ---------------------------------------------------------------- run + compare

func (h *harness) run(steps int) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(failure); !ok {
				panic(r)
			}
		}
	}()
	for h.step = 1; h.step <= steps; h.step++ {
		op := h.gen()
		h.cur = op
		want := h.m.Do(op)
		got := h.send(op)
		h.compare(op, want, got)
		if h.step%envInt("DIFF_FULL_EVERY", 25) == 0 {
			h.compareAll()
		}
	}
	h.step = steps
	h.compareAll()
}

func (h *harness) compare(op refmodel.Op, want refmodel.Result, got response) {
	statMu.Lock()
	k := got.errKey()
	if want.Replayed {
		k = "replayed"
	} else if want.NoOp {
		k = "noop"
	} else if op.Validate && got.code == 200 {
		k = "validated"
	}
	stats[k]++
	statMu.Unlock()
	if got.errKey() != want.Err.String() {
		h.fatalf("outcome differs: model %s, server %s\nserver body: %s", want.Err, got.errKey(), got.raw)
	}
	if want.Err == nil && want.Wrote && (op.Kind == "create" || op.Kind == "ccreate" || op.Kind == "tcreate") {
		real := idOf(h.t, got.body["name"])
		h.rid[want.Created] = real
		h.mid[real] = want.Created
	}
	if want.Err == nil && !want.Wrote && !want.Replayed && !want.NoOp && op.Validate && want.Preview != nil {
		h.comparePreview(want.Preview, got)
	}
	if want.Err == nil && want.Replayed && op.Kind != "ccreate" && op.Kind != "cresolve" && op.Kind != "creopen" && op.Kind != "cdelete" {
		// replay answers with the original entity
		if want.EntType == "workflow" && idOf(h.t, got.body["name"]) != h.rid[want.Entity] {
			h.fatalf("replay answered for another entity: %v", got.body["name"])
		}
	}
	if ent := want.Entity; ent != 0 {
		h.checkEvents(want.EntType, ent, want, op)
		if want.Wrote { // rejected/no-op/replayed requests are covered by the event count and compareAll
			h.compareEntity(want.EntType, ent)
		}
	}
	// a workflow delete/undelete took tasks along: one event each, state as the model says
	for _, tk := range want.Cascaded {
		h.checkEvents("task", tk, refmodel.Result{Wrote: true}, op)
		h.compareEntity("task", tk)
		h.verifyEntityLatest(h.rid[tk])
		statMu.Lock()
		stats["cascaded"]++
		statMu.Unlock()
	}
	if want.Wrote {
		if op.ReqID != "" {
			h.prev = append(h.prev, op)
		}
	}
	// entity_latest of the touched entity equals the recomputation from delta after every step; the
	// full VerifyLatest runs in compareAll
	if want.Entity != 0 && want.Wrote {
		h.verifyEntityLatest(h.rid[want.Entity])
	}
}

func (h *harness) verifyEntityLatest(id int64) {
	var n int
	err := h.st.DB.QueryRow(`SELECT COUNT(*) FROM (
		SELECT d.k, d.event_id, CAST(d.new_v AS CHAR) v FROM delta d
		  JOIN (SELECT k, MAX(event_id) m FROM delta WHERE entity_id=? GROUP BY k) x ON x.k=d.k AND x.m=d.event_id WHERE d.entity_id=?) r
		LEFT JOIN entity_latest l ON l.entity_id=? AND l.k=r.k
		WHERE l.k IS NULL OR l.event_id<>r.event_id OR NOT (CAST(l.v AS CHAR) <=> r.v)`, id, id, id).Scan(&n)
	var extra int
	h.st.DB.QueryRow(`SELECT COUNT(*) FROM entity_latest l WHERE l.entity_id=? AND NOT EXISTS (SELECT 1 FROM delta d WHERE d.entity_id=l.entity_id AND d.k=l.k)`, id).Scan(&extra)
	if err != nil || n+extra != 0 {
		h.fatalf("entity_latest of entity %d differs from delta (%d stale/missing, %d extra, err %v)", id, n, extra, err)
	}
}

func (h *harness) comparePreview(p *refmodel.WF, got response) {
	g := got.body
	if g["display_name"] != p.Name {
		h.fatalf("preview display_name %v want %q: %s", g["display_name"], p.Name, got.raw)
	}
	if h.scopeOf(p) != g["scope"] {
		h.fatalf("preview scope %v want %s", g["scope"], h.scopeOf(p))
	}
	if cs, _ := g["current_stage"].(string); cs != p.Current() {
		h.fatalf("preview current_stage %q want %q", cs, p.Current())
	}
	if _, has := g["delete_time"]; has != p.IsDeleted() {
		h.fatalf("preview delete_time presence %v want %v", has, p.IsDeleted())
	}
}

func (h *harness) scopeOf(w *refmodel.WF) string {
	if w.Out() {
		return "OUT_OF_SCOPE"
	}
	return "IN_SCOPE"
}

// checkEvents: the number of recorded events and (after a write) the keys of the newest event.
func (h *harness) checkEvents(typ string, ent int, want refmodel.Result, op refmodel.Op) {
	real := h.rid[ent]
	var n int
	var maxID int64
	if err := h.st.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id),0) FROM event WHERE entity_id=?`, real).Scan(&n, &maxID); err != nil {
		h.fatalf("count events: %v", err)
	}
	var ver int
	var log []map[string]string
	if typ == "workflow" {
		ver, log = h.m.WFs[ent].Ver, h.m.WFs[ent].Log
	} else if typ == "task" {
		ver, log = h.m.Tasks[ent].Ver, h.m.Tasks[ent].Log
	} else {
		ver, log = h.m.Comments[ent].Ver, h.m.Comments[ent].Log
	}
	if n != ver {
		h.fatalf("entity %d: %d events recorded, model expects %d (replay/no-op/validate_only must not write)", real, n, ver)
	}
	if want.Wrote {
		if maxID <= h.last[ent] {
			h.fatalf("entity %d: event id %d did not increase (previous %d)", real, maxID, h.last[ent])
		}
		h.last[ent] = maxID
		// keys and values of the newest event
		rows, err := h.st.DB.Query(`SELECT k, CAST(new_v AS CHAR) FROM delta WHERE event_id=?`, maxID)
		if err != nil {
			h.fatalf("deltas: %v", err)
		}
		got := map[string]string{}
		for rows.Next() {
			var k string
			var v *string
			rows.Scan(&k, &v)
			got[k] = ""
			if v != nil {
				var x any
				json.Unmarshal([]byte(*v), &x)
				switch t := x.(type) {
				case string:
					got[k] = t
				case float64:
					got[k] = strconv.FormatInt(int64(t), 10)
				case bool:
					got[k] = strconv.FormatBool(t)
				case []any: // evidence.<stage>: the list holds the one attachment of the workflow
					got[k] = "E"
				case map[string]any: // override.<key>: the side(s) the rule reserved (evidence is checked on its own)
					got[k], _ = t["side"].(string)
					statMu.Lock()
					stats["override-recorded"]++
					statMu.Unlock()
					if ev, _ := t["evidence"].([]any); len(ev) == 0 {
						h.fatalf("entity %d event %d: override record without evidence: %s", real, maxID, *v)
					}
				}
			}
		}
		rows.Close()
		exp := map[string]string{}
		for k, v := range log[len(log)-1] {
			if (k == "into" || k == "on" || k == "workflow") && v != "" {
				mv, _ := strconv.Atoi(v)
				v = strconv.FormatInt(h.rid[mv], 10)
			}
			if k == "cascade" && v == "ev" { // the id of the workflow's delete event, its newest event
				var evID int64
				h.st.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM event WHERE entity_id=?`, h.rid[op.Target]).Scan(&evID)
				v = strconv.FormatInt(evID, 10)
			}
			exp[k] = v
		}
		if fmt.Sprint(got) != fmt.Sprint(exp) {
			h.fatalf("entity %d event %d wrote %v, model expects %v", real, maxID, got, exp)
		}
	}
}

func (h *harness) getWF(ent int) response {
	return h.call(h.tok["d"], "GET", "/v1/"+h.proj.WorkflowName(h.rid[ent]), nil)
}

func (h *harness) compareEntity(typ string, ent int) {
	if typ == "workflow" {
		r := h.getWF(ent)
		if r.code != 200 {
			h.fatalf("GET workflow %d: %s", h.rid[ent], r.raw)
		}
		h.diffWF(h.m.WFs[ent], r.body)
		h.noteEtag(ent, r.body["etag"])
		return
	}
	if typ == "task" {
		t := h.m.Tasks[ent]
		r := h.call(h.tok["d"], "GET", "/v1/"+h.tkName(ent), nil)
		if r.code != 200 {
			h.fatalf("GET task %d: %s", h.rid[ent], r.raw)
		}
		if r.body["display_name"] != t.Name || r.body["workflow"] != h.proj.WorkflowName(h.rid[t.WF]) {
			h.fatalf("task %d: server %v, model name=%q workflow=%d", h.rid[ent], r.body, t.Name, h.rid[t.WF])
		}
		if _, has := r.body["delete_time"]; has != t.IsDeleted() {
			h.fatalf("task %d delete_time present %v, model deleted=%v", h.rid[ent], has, t.IsDeleted())
		}
		if r.body["workflows"] != nil {
			h.fatalf("task %d still carries a workflows list: %v", h.rid[ent], r.body)
		}
		h.noteEtag(ent, r.body["etag"])
		return
	}
	c := h.m.Comments[ent]
	r := h.call(h.tok["d"], "GET", "/v1/"+h.proj.WorkflowName(h.rid[c.WF])+"/comments/"+store.FormatID(store.PrefixComment, h.rid[ent]), nil)
	var found map[string]any
	if r.code == 200 {
		found = r.body
	} else if c.Deleted != "true" {
		h.fatalf("live comment %d not readable: %s", h.rid[ent], r.raw)
	}
	if found == nil {
		h.noteEtag(ent, nil)
		return
	}
	if c.Deleted == "true" {
		h.fatalf("deleted comment %d still listed", h.rid[ent])
	}
	if found["kind"] != strings.ToUpper(c.Kind) || found["body"] != c.Body || found["resolved"] != (c.Resolved == "true") {
		h.fatalf("comment %d: server %v, model kind=%s body=%q resolved=%v", h.rid[ent], found, c.Kind, c.Body, c.Resolved)
	}
	h.noteEtag(ent, found["etag"])
}

func (h *harness) noteEtag(ent int, etag any) {
	// the etag is derived from the latest event; a server-reported one must agree
	var maxID int64
	h.st.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM event WHERE entity_id=?`, h.rid[ent]).Scan(&maxID)
	want := store.EtagOf(maxID)
	if s, ok := etag.(string); etag != nil && (!ok || s != want) {
		h.fatalf("entity %d etag %v is not derived from its latest event %s", h.rid[ent], etag, want)
	}
	hist := h.etags[ent]
	if len(hist) == 0 || hist[len(hist)-1] != want {
		h.etags[ent] = append(hist, want)
	}
}

func (h *harness) diffWF(w *refmodel.WF, g map[string]any) {
	bad := func(what string, got, want any) {
		h.fatalf("workflow %d (model %d) %s: server %v, model %v\nserver: %v", h.rid[w.ID], w.ID, what, got, want, g)
	}
	if g["display_name"] != w.Name {
		bad("display_name", g["display_name"], w.Name)
	}
	labels := map[string]string{}
	if l, ok := g["labels"].(map[string]any); ok {
		for k, v := range l {
			labels[k] = v.(string)
		}
	}
	if fmt.Sprint(labels) != fmt.Sprint(w.Labels) {
		bad("labels", labels, w.Labels)
	}
	states := map[string]string{"PASSED": "pass", "OPEN": "open", "NOT_APPLICABLE": "na"}
	st, _ := g["stages"].([]any)
	if len(st) != len(refmodel.Stages) {
		bad("stage count", len(st), len(refmodel.Stages))
	}
	for i, x := range st {
		s := x.(map[string]any)
		if s["stage"] != refmodel.Stages[i] || states[s["state"].(string)] != w.Eff(refmodel.Stages[i]) {
			bad("stages", st, w.Stage)
		}
	}
	if g["scope"] != h.scopeOf(w) {
		bad("scope", g["scope"], h.scopeOf(w))
	}
	mi, _ := g["merged_into"].(string)
	wantMI := ""
	if w.Out() && w.Into != 0 {
		wantMI = h.proj.WorkflowName(h.rid[w.Into])
	}
	if mi != wantMI {
		bad("merged_into", mi, wantMI)
	}
	if _, has := g["delete_time"]; has != w.IsDeleted() {
		bad("delete_time present", has, w.IsDeleted())
	}
	if cs, _ := g["current_stage"].(string); cs != w.Current() {
		bad("current_stage", cs, w.Current())
	}
	if g["health"] != h.m.Health(w.ID) {
		bad("health", g["health"], h.m.Health(w.ID))
	}
}

// compareAll compares every entity, the list endpoints, project isolation and the SQL views.
func (h *harness) compareAll() {
	for _, id := range h.wfIDs() {
		if h.rid[id] == 0 {
			continue
		}
		h.compareEntity("workflow", id)
		for _, c := range h.m.LiveComments(id) {
			h.compareEntity("comment", c.ID)
		}
	}
	for _, show := range []bool{false, true} {
		path := "/v1/projects/" + h.project + "/workflows?page_size=1000"
		if show {
			path += "&show_deleted=true"
		}
		r := h.call(h.tok["d"], "GET", path, nil)
		var got []int64
		if l, ok := r.body["workflows"].([]any); ok {
			for _, x := range l {
				got = append(got, idOf(h.t, x.(map[string]any)["name"]))
			}
		}
		var want []int64
		for _, id := range h.wfIDs() {
			if h.rid[id] != 0 && (show || !h.m.WFs[id].IsDeleted()) {
				want = append(want, h.rid[id])
			}
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			h.fatalf("list workflows (show_deleted=%v): server %v, model %v (a foreign workflow %d must never appear)", show, got, want, h.foreign)
		}
	}
	h.compareTasks()
	// the SQL view
	r := h.call(h.tok["d"], "POST", "/v1/projects/"+h.project+":query", map[string]any{"sql": "SELECT id, scope, current_stage, health FROM workflows", "page_size": 1000})
	if r.code != 200 {
		h.fatalf("query: %s", r.raw)
	}
	type row struct{ scope, cur, health string }
	got := map[int64]row{}
	for _, x := range r.body["rows"].([]any) {
		c := x.([]any)
		cur := ""
		if c[2] != nil {
			cur = fmt.Sprint(c[2])
		}
		id, _ := strconv.ParseInt(fmt.Sprint(c[0]), 10, 64)
		if f, ok := c[0].(float64); ok {
			id = int64(f)
		}
		got[id] = row{fmt.Sprint(c[1]), cur, fmt.Sprint(c[3])}
	}
	want := map[int64]row{}
	for _, id := range h.wfIDs() {
		w := h.m.WFs[id]
		if h.rid[id] == 0 || w.IsDeleted() {
			continue
		}
		sc := "in"
		if w.Out() {
			sc = "out"
		}
		hl := map[string]string{"NORMAL": "ok", "AT_RISK": "at_risk", "BLOCKED": "blocked"}[h.m.Health(id)]
		want[h.rid[id]] = row{sc, w.Current(), hl}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		h.fatalf("workflows view differs:\nserver %v\nmodel  %v", got, want)
	}
	if p, err := logProblems(context.Background(), h.st.DB); err != nil || len(p) > 0 {
		h.fatalf("log integrity: %v %v", p, err)
	}
	if n, first, err := h.st.VerifyLatest(context.Background(), 3); err != nil || n != 0 {
		h.fatalf("VerifyLatest: %d mismatches %v err %v", n, first, err)
	}
}

// compareTasks compares every task, the task lists and the tasks view; a live task never sits under a deleted workflow.
func (h *harness) compareTasks() {
	for _, id := range h.tkIDs() {
		if t := h.m.Tasks[id]; !t.IsDeleted() && h.m.WFs[t.WF].IsDeleted() {
			h.fatalf("model: live task %d under deleted workflow %d", id, t.WF)
		}
		h.compareEntity("task", id)
	}
	for _, show := range []bool{false, true} {
		path := "/v1/projects/" + h.project + "/tasks?page_size=1000"
		if show {
			path += "&show_deleted=true"
		}
		r := h.call(h.tok["d"], "GET", path, nil)
		var got, want []int64
		if l, ok := r.body["tasks"].([]any); ok {
			for _, x := range l {
				got = append(got, idOf(h.t, x.(map[string]any)["name"]))
			}
		}
		for _, id := range h.tkIDs() {
			if show || !h.m.Tasks[id].IsDeleted() {
				want = append(want, h.rid[id])
			}
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if fmt.Sprint(got) != fmt.Sprint(want) {
			h.fatalf("list tasks (show_deleted=%v): server %v, model %v", show, got, want)
		}
	}
	r := h.call(h.tok["d"], "POST", "/v1/projects/"+h.project+":query", map[string]any{"sql": "SELECT id, workflow_id FROM tasks", "page_size": 1000})
	if r.code != 200 {
		h.fatalf("query tasks: %s", r.raw)
	}
	got := map[int64]int64{}
	for _, x := range r.body["rows"].([]any) {
		c := x.([]any)
		got[int64(c[0].(float64))] = int64(c[1].(float64))
	}
	want := map[int64]int64{}
	for _, t := range h.m.LiveTasks() {
		want[h.rid[t.ID]] = h.rid[t.WF]
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		h.fatalf("tasks view differs:\nserver %v\nmodel  %v", got, want)
	}
}
