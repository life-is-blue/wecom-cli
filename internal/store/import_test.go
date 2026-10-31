package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

type rs struct {
	ref, name, hash, group, owner, stage string
	code                                 string
}

func mkRow(system string, r rs) json.RawMessage {
	src := map[string]any{"system": system}
	if r.ref != "" {
		src["reference"] = r.ref
	}
	if r.hash != "" {
		src["row_hash"] = r.hash
	}
	data := map[string]any{}
	if r.name != "" {
		data["display_name"] = r.name
	}
	if r.code != "" {
		data["name"] = r.code
	}
	if r.group != "" {
		data["labels"] = map[string]string{"group": r.group}
	}
	if r.owner != "" {
		data["assignees"] = map[string]string{"dev_owner": r.owner}
	}
	if r.stage != "" {
		data["stages"] = []map[string]string{{"stage": "development", "state": r.stage}}
	}
	b, _ := json.Marshal(map[string]any{"source": src, "data": data})
	return b
}

func rows(system string, rr ...rs) []json.RawMessage {
	var out []json.RawMessage
	for _, r := range rr {
		out = append(out, mkRow(system, r))
	}
	return out
}

func (e *env) stage(t *testing.T, src string, rr []json.RawMessage) (int64, *ImportJobResult) {
	t.Helper()
	job, err := e.st.StageImport(e.ctx, e.p, e.actor("delivery", "editor"), StageInput{Source: src, Rows: rr})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(*job.Uid, 10, 64)
	return id, job
}

func (e *env) apply(t *testing.T, id int64, res map[int]string) *ImportJobResult {
	t.Helper()
	job, err := e.st.ApplyImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{Resolutions: res})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (e *env) diff(t *testing.T, id int64) map[int]DiffRow {
	t.Helper()
	rows, _, err := e.st.DiffImport(e.ctx, e.p, id, nil, "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]DiffRow{}
	for _, r := range rows {
		k := r.RowNo
		if r.Action == core.ActionMissing {
			k = -int(r.Match)
		}
		out[k] = r
	}
	return out
}

func (e *env) ent(t *testing.T, id int64) *Entity {
	t.Helper()
	en, err := loadEntity(e.ctx, e.st.DB, e.p.ID, "workflow", id)
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// byRef finds the live workflow id imported with the given reference.
func (e *env) byRef(t *testing.T, system, ref string) int64 {
	t.Helper()
	ex, err := loadImportExisting(e.ctx, e.st.DB, e.p.ID, "workflow")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ex {
		s, _ := core.Str(x.Vals["source.system"])
		r, _ := core.Str(x.Vals["source.ref"])
		if s == system && r == ref && !x.Deleted {
			return x.ID
		}
	}
	t.Fatalf("no workflow for %s/%s", system, ref)
	return 0
}

func (e *env) countEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE project_id=?`, e.p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) liveWorkflows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.st.DB.QueryRow(`SELECT COUNT(*) FROM workflows WHERE project_id=?`, e.p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) platformSet(t *testing.T, id int64, kv ...string) {
	t.Helper()
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: e.actor("delivery", "editor"), Plan: set(kv...)}); err != nil {
		t.Fatal(err)
	}
}

func sum(j *ImportJobResult, k string) int { return (*j.Summary)[k] }

func TestImportDiffClassifications(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	// a platform-native workflow
	plat := e.createWF(t, "Plat")
	e.platformSet(t, plat, "attr.group", "g1")
	other := e.createWF(t, "Other source")
	e.platformSet(t, other, "source.system", "other_sys", "source.ref", "o1", "source.hash", "x")

	id1, j1 := e.stage(t, sys, rows(sys,
		rs{ref: "a", name: "A", hash: "ha", group: "g1", owner: "bob", stage: "OPEN"},
		rs{ref: "b", name: "B", hash: "hb", stage: "OPEN"},
		rs{ref: "c", name: "C", hash: "hc", owner: "bob"},
		rs{ref: "d", name: "D", hash: "hd"},
		rs{ref: "e", name: "E", hash: "he"},
	))
	if sum(j1, "NEW") != 5 || sum(j1, "INVALID") != 0 || *j1.State != "STAGED" {
		t.Fatalf("stage summary %v %v", *j1.Summary, *j1.State)
	}
	a1 := e.apply(t, id1, nil)
	if *a1.State != "APPLIED" || sum(a1, "CREATED") != 5 {
		t.Fatalf("apply1 %v %v", *a1.State, *a1.Summary)
	}
	wfA, wfB, wfC, wfD, wfE := e.byRef(t, sys, "a"), e.byRef(t, sys, "b"), e.byRef(t, sys, "c"), e.byRef(t, sys, "d"), e.byRef(t, sys, "e")

	// platform edits after the first import
	e.platformSet(t, wfB, "stage.development", "pass")
	e.platformSet(t, wfC, "assignee.dev_owner", "carol")
	e.platformSet(t, wfD, "deleted", "")
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: wfD, Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "deleted", New: core.JSON(true)}}, nil
		}}); err != nil {
		t.Fatal(err)
	}

	bad := json.RawMessage(`{"source":{"system":"legacy","reference":"bad1"},"data":{"labels":{"x":"y"}}}`)
	badStage := json.RawMessage(`{"source":{"system":"legacy","reference":"bad2"},"data":{"display_name":"Q","stages":[{"stage":"nope","state":"OPEN"}]}}`)
	garbage := json.RawMessage(`{"source":"oops"}`)
	batch2 := rows(sys,
		rs{ref: "a", name: "A", hash: "ha", group: "g1", owner: "bob", stage: "OPEN"},   // 1 unchanged
		rs{ref: "b", name: "B2", hash: "hb2", stage: "NOT_APPLICABLE"},                  // 2 conflict (stage edited in platform)
		rs{ref: "c", name: "C2", hash: "hc2", owner: "bob"},                             // 3 changed, platform assignee wins
		rs{ref: "d", name: "D2", hash: "hd2"},                                           // 4 conflict (deleted in platform)
		rs{name: "Plat", group: "g1", hash: "hp0"},                                      // 5 suspect
		rs{name: "Fresh", hash: "hf"},                                                   // 6 new (no id, no name match)
		rs{ref: "z", name: "Z", hash: "hz"},                                             // 7 new
		rs{code: e.p.WorkflowName(other), ref: "pc", name: "Other renamed", hash: "hp"}, // 8 changed by platform code
		rs{ref: "z", name: "Z again", hash: "hz2"},                                      // 9 duplicate ref -> invalid
	)
	batch2 = append(batch2, bad, badStage, garbage)
	id2, j2 := e.stage(t, sys, batch2)
	want := map[string]int{"NEW": 2, "CHANGED": 2, "UNCHANGED": 1, "CONFLICT": 2, "SUSPECT": 1, "MISSING": 1, "INVALID": 4}
	for k, v := range want {
		if sum(j2, k) != v {
			t.Errorf("summary %s = %d, want %d (%v)", k, sum(j2, k), v, *j2.Summary)
		}
	}
	if len(j2.RowErrors) != 4 || j2.RowErrors[0].RowNo != 9 {
		t.Fatalf("row errors %+v", j2.RowErrors)
	}
	d := e.diff(t, id2)
	expect := map[int]string{1: "unchanged", 2: "conflict", 3: "changed", 4: "conflict", 5: "suspect", 6: "new", 7: "new", 8: "changed", -int(wfE): "missing"}
	for no, act := range expect {
		if d[no].Action != act {
			t.Errorf("row %d: %q, want %q", no, d[no].Action, act)
		}
	}
	if len(d) != len(expect) {
		t.Errorf("diff has %d lines, want %d", len(d), len(expect))
	}
	if d[5].Match != plat || d[8].Match != other {
		t.Errorf("matches: %d %d", d[5].Match, d[8].Match)
	}
	// the other source's workflow is not "missing"; conflict lists the guarded key
	for _, r := range d {
		if r.Action == core.ActionMissing && r.Match == other {
			t.Error("workflow of another source flagged missing")
		}
	}
	var gotKinds []string
	for _, c := range d[2].Changes {
		gotKinds = append(gotKinds, c.Key+":"+c.Kind)
	}
	if !strings.Contains(strings.Join(gotKinds, ","), "stage.development:CONFLICT") {
		t.Errorf("row 2 changes %v", gotKinds)
	}
	for _, c := range d[3].Changes {
		if strings.HasPrefix(c.Key, "assignee.") {
			t.Errorf("row 3 must not touch assignees: %v", d[3].Changes)
		}
	}

	// filter + paging
	pg, next, err := e.st.DiffImport(e.ctx, e.p, id2, map[string]bool{"new": true, "conflict": true}, "", 2)
	if err != nil || len(pg) != 2 || next == "" {
		t.Fatalf("page: %v %q %v", pg, next, err)
	}
	// the token is bound to its job and filter, and cannot be forged from an offset
	_, _, err = e.st.DiffImport(e.ctx, e.p, id2, map[string]bool{"new": true}, next, 2)
	coreErr(t, err, core.StatusInvalidArgument, "INVALID_PAGE_TOKEN")
	_, _, err = e.st.DiffImport(e.ctx, e.p, id2, map[string]bool{"new": true, "conflict": true}, "2", 2)
	coreErr(t, err, core.StatusInvalidArgument, "INVALID_PAGE_TOKEN")
	pg, next, _ = e.st.DiffImport(e.ctx, e.p, id2, map[string]bool{"new": true, "conflict": true}, next, 10)
	if len(pg) != 2 || next != "" {
		t.Fatalf("page2: %v %q", pg, next)
	}

	// staging changed nothing
	if n := e.liveWorkflows(t); n != 2+5-1 {
		t.Fatalf("live workflows after staging: %d", n)
	}

	// apply without resolutions
	evBefore := e.countEvents(t)
	a2 := e.apply(t, id2, nil)
	if sum(a2, "CREATED") != 2 || sum(a2, "UPDATED") != 2 || sum(a2, "SKIPPED") != 3 {
		t.Fatalf("apply2 summary %v", *a2.Summary)
	}
	if got := e.countEvents(t); got != evBefore+4 {
		t.Fatalf("apply wrote %d events, want 4", got-evBefore)
	}
	// source-owned updated, platform-owned untouched
	c := e.ent(t, wfC)
	if c.str("name") != "C2" || c.str("source.hash") != "hc2" || c.str("assignee.dev_owner") != "carol" {
		t.Fatalf("wfC: %v", c.Vals)
	}
	if b := e.ent(t, wfB); b.str("name") != "B" || b.str("stage.development") != "pass" || b.str("source.hash") != "hb" {
		t.Fatalf("wfB must be untouched (conflict skipped): %v", b.Vals)
	}
	if dd := e.ent(t, wfD); dd.str("name") != "D" || !dd.deleted() {
		t.Fatalf("wfD: %v", dd.Vals)
	}
	if o := e.ent(t, other); o.str("name") != "Other renamed" || o.str("source.ref") != "pc" {
		t.Fatalf("other: %v", o.Vals)
	}
	if pl := e.ent(t, plat); pl.str("source.ref") != "" || pl.str("name") != "Plat" {
		t.Fatalf("suspect must be skipped: %v", pl.Vals)
	}
	if e.ent(t, wfE).deleted() || e.ent(t, wfA).str("name") != "A" {
		t.Fatal("missing / unchanged rows must stay as they are")
	}
	// events are attributable to the batch
	var key, note, channel string
	if err := e.st.DB.QueryRow(`SELECT idempotency_key, note, channel FROM event WHERE entity_id=? ORDER BY id DESC LIMIT 1`, wfC).Scan(&key, &note, &channel); err != nil {
		t.Fatal(err)
	}
	if key != fmt.Sprintf("imp-%d-3", id2) || channel != "import" || !strings.Contains(note, fmt.Sprintf("import job %d row 3", id2)) {
		t.Fatalf("event attribution: %q %q %q", key, note, channel)
	}

	// idempotent: second apply is a no-op
	evBefore = e.countEvents(t)
	a2b := e.apply(t, id2, nil)
	if *a2b.State != "APPLIED" || e.countEvents(t) != evBefore || e.liveWorkflows(t) != 2+5-1+2 {
		t.Fatal("second apply must change nothing")
	}

	// resolutions: re-stage the skipped rows and resolve them explicitly
	susp := e.createWF(t, "Susp")
	e.platformSet(t, susp, "attr.group", "g1")
	id3, j3 := e.stage(t, sys, rows(sys,
		rs{ref: "b", name: "B2", hash: "hb2", stage: "NOT_APPLICABLE"},
		rs{name: "Susp", group: "g1", hash: "hs"},
		rs{name: "Susp", group: "g1", hash: "hs2"},
	))
	if sum(j3, "CONFLICT") != 1 || sum(j3, "SUSPECT") != 2 {
		t.Fatalf("batch3 %v", *j3.Summary)
	}
	if _, err := e.st.ApplyImport(e.ctx, e.p, e.actor("delivery", "admin"), id3, ImportApplyOpts{Resolutions: map[int]string{1: "link"}}); err == nil {
		t.Fatal("link is not a conflict resolution")
	} else {
		coreErr(t, err, core.StatusInvalidArgument, "INVALID_RESOLUTION")
	}
	e.apply(t, id3, map[int]string{1: "merge", 2: "link", 3: "create"})
	if b := e.ent(t, wfB); b.str("name") != "B2" || b.str("source.hash") != "hb2" || b.str("stage.development") != "pass" {
		t.Fatalf("merge must update source fields only: %v", b.Vals)
	}
	if s := e.ent(t, susp); s.str("source.ref") != "" && s.str("source.hash") != "hs" {
		t.Fatalf("link: %v", s.Vals)
	} else if s.str("source.system") != sys || s.str("source.hash") != "hs" {
		t.Fatalf("link: %v", s.Vals)
	}
	if e.liveWorkflows(t) != 2+5-1+2+1+1 {
		t.Fatalf("created by resolution: %d", e.liveWorkflows(t))
	}
}

func TestImportRollback(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	pre := e.createWF(t, "Existing")
	e.platformSet(t, pre, "source.system", sys, "source.ref", "p1", "source.hash", "h0")
	id, _ := e.stage(t, sys, rows(sys,
		rs{ref: "p1", name: "Existing renamed", hash: "h1"},
		rs{ref: "n1", name: "New 1", hash: "h2"},
		rs{ref: "n2", name: "New 2", hash: "h3"},
	))
	e.apply(t, id, nil)
	if e.liveWorkflows(t) != 3 || e.ent(t, pre).str("name") != "Existing renamed" {
		t.Fatal("apply did not take effect")
	}
	e.verifyLatest(t)
	evBefore := e.countEvents(t)
	rb, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if *rb.State != "ROLLED_BACK" {
		t.Fatal(*rb.State)
	}
	e.verifyLatest(t)
	if e.liveWorkflows(t) != 1 {
		t.Fatalf("live workflows after rollback: %d", e.liveWorkflows(t))
	}
	if p := e.ent(t, pre); p.str("name") != "Existing" || p.str("source.hash") != "h0" {
		t.Fatalf("updated entity not restored: %v", p.Vals)
	}
	if got := e.countEvents(t); got != evBefore+3 {
		t.Fatalf("rollback must append 3 correcting events, got %d", got-evBefore)
	}
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE project_id=? AND corrects_event_id IS NOT NULL AND channel='import'`, e.p.ID).Scan(&n)
	if n != 3 {
		t.Fatalf("correcting events: %d", n)
	}
	// idempotent
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{}); err != nil || e.countEvents(t) != evBefore+3 {
		t.Fatalf("second rollback: %v", err)
	}
	// a rolled back job cannot be applied again
	if _, err := e.st.ApplyImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{}); err == nil {
		t.Fatal("apply after rollback must fail")
	} else {
		coreErr(t, err, core.StatusFailedPrecondition, "IMPORT_JOB_STATE")
	}
	// a staged job cannot be rolled back
	id2, _ := e.stage(t, sys, rows(sys, rs{ref: "n9", name: "N9", hash: "h9"}))
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id2, ImportApplyOpts{}); err == nil {
		t.Fatal("rollback of staged job must fail")
	}
}

func TestImportRollbackRefusedAfterLaterChange(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	id, _ := e.stage(t, sys, rows(sys, rs{ref: "n1", name: "New 1", hash: "h1"}, rs{ref: "n2", name: "New 2", hash: "h2"}, rs{ref: "n3", name: "New 3", hash: "h3"}))
	e.apply(t, id, nil)
	touched := e.byRef(t, sys, "n2")
	e.platformSet(t, touched, "attr.priority", "p1")
	before := e.countEvents(t)
	_, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{})
	if err == nil {
		t.Fatal("rollback must be refused")
	}
	coreErr(t, err, core.StatusFailedPrecondition, "ROLLBACK_BLOCKED")
	if !strings.Contains(err.Error(), e.p.WorkflowName(touched)) {
		t.Fatalf("refusal must list the entity: %v", err)
	}
	if e.countEvents(t) != before || e.liveWorkflows(t) != 3 {
		t.Fatal("refused rollback must change nothing")
	}
	job, _ := e.st.GetImport(e.ctx, e.p, id)
	if *job.State != "APPLIED" {
		t.Fatal(*job.State)
	}
}

func TestImportProjectIsolationAndRequestID(t *testing.T) {
	e := newEnv(t)
	e2 := newEnv(t)
	rr := rows("legacy", rs{ref: "a", name: "A", hash: "h"})
	j1, err := e.st.StageImport(e.ctx, e.p, e.actor("delivery", "editor"), StageInput{Source: "legacy", Rows: rr, RequestID: "11111111-1111-1111-1111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := e.st.StageImport(e.ctx, e.p, e.actor("delivery", "editor"), StageInput{Source: "legacy", Rows: rr, RequestID: "11111111-1111-1111-1111-111111111111"})
	if err != nil || *j1.Name != *j2.Name {
		t.Fatalf("request_id replay: %v %v", err, j2)
	}
	id, _ := strconv.ParseInt(*j1.Uid, 10, 64)
	if _, err := e2.st.GetImport(e2.ctx, e2.p, id); err == nil {
		t.Fatal("job leaked across projects")
	} else {
		coreErr(t, err, core.StatusNotFound, "NOT_FOUND")
	}
	if _, err := e2.st.ApplyImport(e2.ctx, e2.p, e2.actor("delivery", "admin"), id, ImportApplyOpts{}); err == nil {
		t.Fatal("apply leaked across projects")
	}
	if _, err := e.st.StageImport(e.ctx, e.p, e.actor("delivery", "editor"), StageInput{Source: "", Rows: rr}); err == nil {
		t.Fatal("empty source must be refused")
	}
	// validate_only applies nothing
	v, err := e.st.ApplyImport(e.ctx, e.p, e.actor("delivery", "admin"), id, ImportApplyOpts{ValidateOnly: true})
	if err != nil || *v.State != "STAGED" || sum(v, "CREATED") != 1 || e.liveWorkflows(t) != 0 {
		t.Fatalf("validate_only: %v %v", err, v)
	}
}

func TestImportScale3000(t *testing.T) {
	e := newEnv(t)
	const sys = "legacy"
	const n = 3000
	mk := func(suffix string) []json.RawMessage {
		var rr []json.RawMessage
		for i := 0; i < n; i++ {
			rr = append(rr, mkRow(sys, rs{ref: fmt.Sprintf("pipe_%04d", i), name: fmt.Sprintf("pipeline_%04d%s", i, suffix),
				hash: fmt.Sprintf("h%d%s", i, suffix), group: "g" + strconv.Itoa(i%10), owner: "bob", stage: "OPEN"}))
		}
		return rr
	}
	t0 := time.Now()
	id, j := e.stage(t, sys, mk(""))
	tStage := time.Since(t0)
	if sum(j, "NEW") != n {
		t.Fatalf("summary %v", *j.Summary)
	}
	t0 = time.Now()
	a := e.apply(t, id, nil)
	tApply := time.Since(t0)
	if sum(a, "CREATED") != n || e.liveWorkflows(t) != n {
		t.Fatalf("apply: %v live=%d", *a.Summary, e.liveWorkflows(t))
	}
	t.Logf("stage %v, apply %d new workflows %v", tStage, n, tApply)
	if tApply > 60*time.Second || tStage > 60*time.Second {
		t.Fatalf("too slow: stage %v apply %v", tStage, tApply)
	}
	// re-import of the same data: everything unchanged
	t0 = time.Now()
	id2, j2 := e.stage(t, sys, mk(""))
	if sum(j2, "UNCHANGED") != n {
		t.Fatalf("re-import summary %v", *j2.Summary)
	}
	t.Logf("re-stage (diff against %d workflows) %v", n, time.Since(t0))
	// every row changed
	id3, j3 := e.stage(t, sys, mk("x"))
	if sum(j3, "CHANGED") != n {
		t.Fatalf("changed summary %v", *j3.Summary)
	}
	_ = id2
	t0 = time.Now()
	a3 := e.apply(t, id3, nil)
	tUpd := time.Since(t0)
	t.Logf("apply %d updates %v", n, tUpd)
	if sum(a3, "UPDATED") != n || tUpd > 120*time.Second {
		t.Fatalf("update apply: %v in %v", *a3.Summary, tUpd)
	}
	t0 = time.Now()
	if _, err := e.st.RollbackImport(e.ctx, e.p, e.actor("delivery", "admin"), id3, ImportApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	t.Logf("rollback %d updates %v", n, time.Since(t0))
	if e.ent(t, e.byRef(t, sys, "pipe_0007")).str("name") != "pipeline_0007" {
		t.Fatal("rollback did not restore names")
	}
}
