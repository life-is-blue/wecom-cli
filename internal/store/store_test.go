package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

var seq atomic.Int64

type env struct {
	st   *Store
	p    Project
	ctx  context.Context
	acct string
}

// newEnv opens OPENETL_TEST_DSN (skip when unset), migrates, and creates an isolated project.
func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	st, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("t%d-%d", time.Now().UnixNano()%1e9, seq.Add(1))
	if _, err := st.CreateToken(ctx, name, "alice", "delivery", "editor", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	m, ok, err := st.MemberOf(ctx, name, "alice")
	if err != nil || !ok {
		t.Fatalf("member: %v %v", ok, err)
	}
	return &env{st: st, p: Project{ID: m.ProjectID, Name: name}, ctx: ctx, acct: "alice"}
}

func (e *env) actor(side, role string) core.Actor {
	return core.Actor{Account: e.acct, Side: side, Role: role, Channel: core.ChannelAPI}
}

func set(kv ...string) func(*Entity, core.Env) ([]PlannedChange, error) {
	return func(*Entity, core.Env) ([]PlannedChange, error) {
		var out []PlannedChange
		for i := 0; i < len(kv); i += 2 {
			out = append(out, PlannedChange{Key: kv[i], New: core.JSON(kv[i+1])})
		}
		return out, nil
	}
}

func (e *env) createWF(t *testing.T, name string) int64 {
	t.Helper()
	res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", Actor: e.actor("delivery", "editor"), Plan: set("name", name)})
	if err != nil {
		t.Fatal(err)
	}
	return res.EntityID
}

func coreErr(t *testing.T, err error, status, reason string) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want %s/%s, got %v", status, reason, err)
	}
	if ce.Status != status || ce.Reason != reason {
		t.Fatalf("want %s/%s, got %s/%s: %s", status, reason, ce.Status, ce.Reason, ce.Message)
	}
}

func TestApplyCreateAndEtag(t *testing.T) {
	e := newEnv(t)
	res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", Actor: e.actor("delivery", "editor"), Plan: set("name", "daily_loader", "attr.domain", "dwh")})
	if err != nil {
		t.Fatal(err)
	}
	if res.EntityID == 0 || res.EventID == 0 || len(res.Changes) != 2 {
		t.Fatalf("bad result %+v", res)
	}
	etag := res.After.Etag()
	if etag != EtagOf(res.EventID) {
		t.Fatalf("etag %s", etag)
	}
	// matching etag succeeds
	r2, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: res.EntityID, Etag: etag,
		Actor: e.actor("delivery", "editor"), Plan: set("attr.domain", "ods")})
	if err != nil {
		t.Fatal(err)
	}
	// stale etag is rejected, with the current etag in metadata
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: res.EntityID, Etag: etag,
		Actor: e.actor("delivery", "editor"), Plan: set("attr.domain", "x")})
	coreErr(t, err, core.StatusAborted, "ETAG_MISMATCH")
	var ce *core.Error
	errors.As(err, &ce)
	if ce.Metadata["current_etag"] != EtagOf(r2.EventID) {
		t.Fatalf("current_etag %v", ce.Metadata)
	}
	// old value is read by the server
	if string(r2.Changes[0].Old) != `"dwh"` || string(r2.Changes[0].New) != `"ods"` {
		t.Fatalf("changes %+v", r2.Changes)
	}
	// unknown entity
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: 999999999, Actor: e.actor("delivery", "editor"), Plan: set("name", "x")})
	coreErr(t, err, core.StatusNotFound, "NOT_FOUND")
}

func TestApplyIdempotency(t *testing.T) {
	e := newEnv(t)
	req := ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "7d1f3c52-2f0e-4c53-9d1a-6a9c0f3a1b11",
		Actor: e.actor("delivery", "editor"), Plan: set("name", "idem")}
	r1, err := e.st.Apply(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.st.Apply(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Replayed || r2.EntityID != r1.EntityID || r2.EventID != r1.EventID {
		t.Fatalf("expected replay of %+v, got %+v", r1, r2)
	}
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM entity WHERE project_id=? AND type='workflow'`, e.p.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("replay created %d workflows", n)
	}
	// a replay with a stale etag still returns the stored result
	req2 := ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: r1.EntityID, RequestID: "aaaaaaaa-0000-4000-8000-000000000001",
		Actor: e.actor("delivery", "editor"), Plan: set("attr.a", "1")}
	a, err := e.st.Apply(e.ctx, req2)
	if err != nil {
		t.Fatal(err)
	}
	req2.Etag = "ev-1"
	b, err := e.st.Apply(e.ctx, req2)
	if err != nil || !b.Replayed || b.EventID != a.EventID {
		t.Fatalf("replay with stale etag: %+v %v", b, err)
	}
	// reuse on another entity is refused
	other := e.createWF(t, "other")
	req2.EntityID, req2.Etag = other, ""
	_, err = e.st.Apply(e.ctx, req2)
	coreErr(t, err, core.StatusAlreadyExists, "REQUEST_ID_REUSED")
}

func TestApplyIdempotencyConcurrent(t *testing.T) {
	e := newEnv(t)
	req := ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "cccccccc-0000-4000-8000-00000000000c",
		Actor: e.actor("delivery", "editor"), Plan: set("name", "race")}
	done := make(chan *Result, 4)
	for i := 0; i < 4; i++ {
		go func() {
			r, err := e.st.Apply(e.ctx, req)
			if err != nil {
				t.Error(err)
				done <- nil
				return
			}
			done <- r
		}()
	}
	ids := map[int64]bool{}
	for i := 0; i < 4; i++ {
		if r := <-done; r != nil {
			ids[r.EntityID] = true
		}
	}
	if len(ids) != 1 {
		t.Fatalf("concurrent identical requests produced %d entities", len(ids))
	}
}

func advance(e *env, id int64, stage, side string) error {
	_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: e.actor(side, "editor"), Plan: set("stage."+stage, "pass")})
	return err
}

func TestStageOrderAndSides(t *testing.T) {
	e := newEnv(t)
	id := e.createWF(t, "w")
	coreErr(t, advance(e, id, "deployment", "delivery"), core.StatusFailedPrecondition, "STAGE_ORDER_VIOLATION")
	if err := advance(e, id, "development", "delivery"); err != nil {
		t.Fatal(err)
	}
	if err := advance(e, id, "deployment", "delivery"); err != nil {
		t.Fatal(err)
	}
	if err := advance(e, id, "verify", "delivery"); err != nil {
		t.Fatal(err)
	}
	coreErr(t, advance(e, id, "accept", "delivery"), core.StatusPermissionDenied, "SIDE_NOT_ALLOWED")
	if err := advance(e, id, "accept", "customer"); err != nil {
		t.Fatal(err)
	}
	w, err := e.st.GetWorkflow(e.ctx, e.p, id)
	if err != nil {
		t.Fatal(err)
	}
	if *w.CurrentStage != "dual_run" || *w.Health != "NORMAL" {
		t.Fatalf("current_stage=%q health=%q", *w.CurrentStage, *w.Health)
	}
}

func TestRegressNeedsReason(t *testing.T) {
	e := newEnv(t)
	id := e.createWF(t, "w")
	if err := advance(e, id, "development", "delivery"); err != nil {
		t.Fatal(err)
	}
	open := func(reason string) error {
		_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, ReasonKey: reason,
			Actor: e.actor("delivery", "editor"), Plan: set("stage.development", "open")})
		return err
	}
	coreErr(t, open(""), core.StatusFailedPrecondition, "REASON_REQUIRED")
	coreErr(t, open("obsolete"), core.StatusFailedPrecondition, "REASON_REQUIRED")
	coreErr(t, open("nope"), core.StatusInvalidArgument, "UNKNOWN_REASON")
	if err := open("rej_row_count"); err != nil {
		t.Fatal(err)
	}
	w, _ := e.st.GetWorkflow(e.ctx, e.p, id)
	if *w.CurrentStage != "development" {
		t.Fatalf("current_stage=%q", *w.CurrentStage)
	}
}

func TestScopeAndMergeCycle(t *testing.T) {
	e := newEnv(t)
	a, b := e.createWF(t, "a"), e.createWF(t, "b")
	out := func(id int64, reason string, into int64) error {
		_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, ReasonKey: reason, Actor: e.actor("delivery", "editor"),
			Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
				ch := []PlannedChange{{Key: "scope", New: core.JSON("out")}}
				if into != 0 {
					ch = append(ch, PlannedChange{Key: "into", New: core.JSON(into)})
				}
				return ch, nil
			}})
		return err
	}
	coreErr(t, out(a, "", 0), core.StatusFailedPrecondition, "REASON_REQUIRED")
	coreErr(t, out(a, "rej_row_count", 0), core.StatusFailedPrecondition, "REASON_REQUIRED")
	coreErr(t, out(a, "merged", 0), core.StatusInvalidArgument, "MERGE_TARGET_REQUIRED")
	coreErr(t, out(a, "merged", a), core.StatusFailedPrecondition, "MERGE_CYCLE")
	coreErr(t, out(a, "merged", 987654321), core.StatusInvalidArgument, "MERGE_TARGET_INVALID")
	if err := out(a, "merged", b); err != nil { // a -> b
		t.Fatal(err)
	}
	coreErr(t, out(b, "merged", a), core.StatusFailedPrecondition, "MERGE_CYCLE") // b -> a would close the cycle
	if err := out(b, "obsolete", 0); err != nil {
		t.Fatal(err)
	}
	w, _ := e.st.GetWorkflow(e.ctx, e.p, a)
	if *w.Scope != "OUT_OF_SCOPE" || w.MergedInto == nil || *w.MergedInto != e.p.WorkflowName(b) {
		t.Fatalf("scope=%v merged_into=%v", *w.Scope, w.MergedInto)
	}
}

func TestValidateOnlyWritesNothing(t *testing.T) {
	e := newEnv(t)
	count := func() (ents, evs, deltas int) {
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM entity WHERE project_id=?`, e.p.ID).Scan(&ents)
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE project_id=?`, e.p.ID).Scan(&evs)
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM delta d JOIN event v ON v.id=d.event_id WHERE v.project_id=?`, e.p.ID).Scan(&deltas)
		return
	}
	res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", ValidateOnly: true, RequestID: "dddddddd-0000-4000-8000-00000000000d",
		Actor: e.actor("delivery", "editor"), Plan: set("name", "ghost")})
	if err != nil || res.EntityID != 0 || len(res.Changes) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if a, b, c := count(); a+b+c != 0 {
		t.Fatalf("validate_only wrote rows: %d %d %d", a, b, c)
	}
	// the same request_id is still free afterwards
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "dddddddd-0000-4000-8000-00000000000d",
		Actor: e.actor("delivery", "editor"), Plan: set("name", "real")}); err != nil {
		t.Fatal(err)
	}
	id := e.createWF(t, "w2")
	_, evs0, d0 := count()
	// a rule violation is still reported with validate_only
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, ValidateOnly: true,
		Actor: e.actor("delivery", "editor"), Plan: set("stage.verify", "pass")})
	coreErr(t, err, core.StatusFailedPrecondition, "STAGE_ORDER_VIOLATION")
	r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, ValidateOnly: true,
		Actor: e.actor("delivery", "editor"), Plan: set("stage.development", "pass")})
	if err != nil || r.EventID != 0 || len(r.Changes) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, evs1, d1 := count(); evs1 != evs0 || d1 != d0 {
		t.Fatalf("validate_only wrote events/deltas")
	}
	w, _ := e.st.GetWorkflow(e.ctx, e.p, id)
	if *w.CurrentStage != "development" {
		t.Fatal("stage changed by validate_only")
	}
}

func TestNoOpWritesNothing(t *testing.T) {
	e := newEnv(t)
	id := e.createWF(t, "same")
	r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: e.actor("delivery", "editor"), Plan: set("name", "same")})
	if err != nil || !r.NoOp || r.EventID != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestLegacyChannelBypassesRules(t *testing.T) {
	e := newEnv(t)
	id := e.createWF(t, "hist")
	a := core.Actor{Account: "alice", Side: "system", Role: "editor", Channel: core.ChannelLegacy}
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: a, Plan: set("stage.cutover", "pass")}); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFromComments(t *testing.T) {
	e := newEnv(t)
	wf := e.createWF(t, "w")
	mk := func(kind string) int64 {
		r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "comment", Actor: e.actor("delivery", "editor"),
			Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
				return []PlannedChange{{Key: "on", New: core.JSON(wf)}, {Key: "kind", New: core.JSON(kind)}, {Key: "body", New: core.JSON("b")}}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return r.EntityID
	}
	health := func() string {
		w, err := e.st.GetWorkflow(e.ctx, e.p, wf)
		if err != nil {
			t.Fatal(err)
		}
		return string(*w.Health)
	}
	if health() != "NORMAL" {
		t.Fatal(health())
	}
	risk := mk("risk")
	if health() != "AT_RISK" {
		t.Fatal(health())
	}
	issue := mk("issue")
	if health() != "BLOCKED" {
		t.Fatal(health())
	}
	resolve := func(id int64, v bool) {
		if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "comment", EntityID: id, Actor: e.actor("delivery", "editor"), Plan: set()}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "comment", EntityID: id, Actor: e.actor("delivery", "editor"),
			Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
				return []PlannedChange{{Key: "resolved", New: core.JSON(v)}}, nil
			}}); err != nil {
			t.Fatal(err)
		}
	}
	resolve(issue, true)
	if health() != "AT_RISK" {
		t.Fatal(health())
	}
	resolve(risk, true)
	if health() != "NORMAL" {
		t.Fatal(health())
	}
	cs, _, err := e.st.ListComments(e.ctx, e.p, wf, ListOpts{Filter: "kind = RISK AND resolved = true"})
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %v", cs, err)
	}
}

func TestListFilterPagingAndAsOf(t *testing.T) {
	e := newEnv(t)
	var ids []int64
	for i := 0; i < 5; i++ {
		res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", Actor: e.actor("delivery", "editor"),
			Plan: set("name", fmt.Sprintf("wf_%d", i), "attr.priority", map[bool]string{true: "a1", false: "b1"}[i%2 == 0])})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.EntityID)
	}
	// paging
	var got []int64
	tok := ""
	for page := 0; page < 10; page++ {
		ws, next, err := e.st.ListWorkflows(e.ctx, e.p, ListOpts{PageSize: 2, PageToken: tok})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range ws {
			var n int64
			fmt.Sscanf(*w.Uid, "%d", &n)
			got = append(got, n)
		}
		if next == "" {
			break
		}
		tok = next
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Fatalf("paging got %v want %v", got, ids)
	}
	// descending + offset-style ordering
	ws, _, _ := e.st.ListWorkflows(e.ctx, e.p, ListOpts{OrderBy: "create_time desc", PageSize: 1})
	if *ws[0].Name != e.p.WorkflowName(ids[4]) {
		t.Fatal("desc order")
	}
	ws, next, err := e.st.ListWorkflows(e.ctx, e.p, ListOpts{OrderBy: "display_name desc", PageSize: 2})
	if err != nil || ws[0].DisplayName == nil || *ws[0].DisplayName != "wf_4" || next == "" {
		t.Fatalf("%v %v %v", ws, next, err)
	}
	// filters
	ws, _, err = e.st.ListWorkflows(e.ctx, e.p, ListOpts{Filter: `labels.priority = a1 AND current_stage = development`})
	if err != nil || len(ws) != 3 {
		t.Fatalf("labels filter: %d %v", len(ws), err)
	}
	ws, _, err = e.st.ListWorkflows(e.ctx, e.p, ListOpts{Filter: `NOT labels.priority = a1 OR display_name:wf_0`})
	if err != nil || len(ws) != 3 {
		t.Fatalf("or/not filter: %d %v", len(ws), err)
	}
	if _, _, err = e.st.ListWorkflows(e.ctx, e.p, ListOpts{Filter: `bogus = 1`}); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, _, err = e.st.ListWorkflows(e.ctx, e.p, ListOpts{Filter: `labels.x = "'; DROP TABLE entity; --"`}); err != nil {
		t.Fatal(err)
	}
	// soft delete hides unless show_deleted
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: ids[0], Actor: e.actor("delivery", "editor"), Plan: set()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: ids[0], Actor: e.actor("delivery", "editor"),
		Plan: func(*Entity, core.Env) ([]PlannedChange, error) {
			return []PlannedChange{{Key: "deleted", New: json.RawMessage("true")}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	ws, _, _ = e.st.ListWorkflows(e.ctx, e.p, ListOpts{})
	if len(ws) != 4 {
		t.Fatalf("deleted workflow listed: %d", len(ws))
	}
	ws, _, _ = e.st.ListWorkflows(e.ctx, e.p, ListOpts{ShowDeleted: true})
	if len(ws) != 5 || ws[0].DeleteTime == nil {
		t.Fatalf("show_deleted: %d", len(ws))
	}

	// as_of on events: bounded by create_time
	wf := ids[1]
	evs, _, err := e.st.ListEvents(e.ctx, e.p, wf, ListOpts{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	first := *evs[0].CreateTime
	time.Sleep(20 * time.Millisecond)
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: wf, Actor: e.actor("delivery", "editor"), Plan: set("stage.development", "pass")}); err != nil {
		t.Fatal(err)
	}
	evs, _, _ = e.st.ListEvents(e.ctx, e.p, wf, ListOpts{})
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	evs, _, _ = e.st.ListEvents(e.ctx, e.p, wf, ListOpts{AsOf: first})
	if len(evs) != 1 {
		t.Fatalf("as_of should hide the later event, got %d", len(evs))
	}
	evs, _, _ = e.st.ListEvents(e.ctx, e.p, wf, ListOpts{AsOf: first.Add(-time.Second)})
	if len(evs) != 0 {
		t.Fatalf("as_of before creation: %d", len(evs))
	}
	evs, _, err = e.st.ListEvents(e.ctx, e.p, wf, ListOpts{Filter: `actor = accounts/alice AND side = DELIVERY AND channel = api`})
	if err != nil || len(evs) != 2 {
		t.Fatalf("event filter: %d %v", len(evs), err)
	}
	ev, err := e.st.GetEvent(e.ctx, e.p, wf, mustID(*evs[1].Uid))
	if err != nil || len(*ev.Changes) != 1 || *(*ev.Changes)[0].Key != "stage.development" {
		t.Fatalf("%v %v", ev, err)
	}
	if _, err := e.st.GetEvent(e.ctx, e.p, ids[2], mustID(*evs[1].Uid)); err == nil {
		t.Fatal("event of another workflow reachable")
	}
}

func mustID(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

func TestProjectIsolation(t *testing.T) {
	a := newEnv(t)
	b := newEnv(t)
	idA := a.createWF(t, "in-a")
	if _, err := b.st.GetWorkflow(b.ctx, b.p, idA); err == nil {
		t.Fatal("workflow of project A readable from B")
	}
	_, err := b.st.Apply(b.ctx, ChangeRequest{ProjectID: b.p.ID, EntityType: "workflow", EntityID: idA, Actor: b.actor("delivery", "editor"), Plan: set("name", "hijack")})
	coreErr(t, err, core.StatusNotFound, "NOT_FOUND")
	r, err := b.st.Query(b.ctx, b.p, "SELECT name FROM workflows", 0, "")
	if err != nil || len(*r.Rows) != 0 {
		t.Fatalf("query leaked rows: %v %v", r, err)
	}
	r, err = a.st.Query(a.ctx, a.p, "SELECT name FROM workflows", 0, "")
	if err != nil || len(*r.Rows) != 1 {
		t.Fatalf("query: %v %v", r, err)
	}
}

func TestQueryGuard(t *testing.T) {
	bad := []string{
		"", "DELETE FROM workflows", "SELECT * FROM entity", "SELECT * FROM v_workflow", "SELECT 1; SELECT 2", "SELECT 1; DROP TABLE x",
		"SELECT * FROM workflows INTO OUTFILE '/tmp/x'", "SELECT * FROM workflows -- x", "SELECT /* x */ 1", "SELECT * FROM mysql.user",
		"SELECT * FROM workflows w, entity e", "SELECT SLEEP(10)", "SELECT @@version", "SELECT * FROM workflows FOR UPDATE",
		"WITH x AS (SELECT 1) SELECT * FROM x", "SELECT * FROM information_schema.tables", "SELECT * FROM (SELECT * FROM entity) t",
		"SELECT * FROM workflows JOIN delta d ON 1=1", "UPDATE workflows SET name='x'", "SELECT LOAD_FILE('/etc/passwd')",
		"SELECT * FROM workflows WHERE id IN (SELECT entity_id FROM delta)",
	}
	for _, q := range bad {
		if _, err := GuardQuery(q, 1); err == nil {
			t.Errorf("accepted %q", q)
		}
	}
	good := []string{
		"SELECT stage, health, workflows FROM stage_distribution",
		"SELECT w.id, t.id FROM workflows w JOIN tasks t ON t.primary_workflow_id = w.id WHERE w.name LIKE 'a;b%' ORDER BY 1 LIMIT 5;",
		"select count(*) from workflows where health = 'blocked'",
		"SELECT * FROM `workflows` UNION SELECT * FROM `workflows`",
		"SELECT x.n FROM (SELECT COUNT(*) AS n FROM comments) x",
	}
	for _, q := range good {
		out, err := GuardQuery(q, 7)
		if err != nil {
			t.Errorf("rejected %q: %v", q, err)
			continue
		}
		if !contains(out, "project_id = 7") {
			t.Errorf("no project filter in %q -> %q", q, out)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSplitSQL(t *testing.T) {
	got := SplitSQL("-- c; x\nCREATE TABLE a (v VARCHAR(5) DEFAULT 'x;y'); -- tail\nINSERT INTO a VALUES('--not a comment');\n")
	if len(got) != 2 || got[1] != "INSERT INTO a VALUES('--not a comment')" {
		t.Fatalf("%q", got)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	tok, err := e.st.CreateToken(e.ctx, e.p.Name, "bob", "customer", "viewer", "agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 20 || tok[:5] != TokenPrefix {
		t.Fatalf("token %q", tok)
	}
	acc, ok, err := e.st.AuthenticateToken(e.ctx, tok)
	if err != nil || !ok || acc.UserName != "bob" || acc.Kind != "agent" {
		t.Fatalf("%+v %v %v", acc, ok, err)
	}
	if _, ok, _ := e.st.AuthenticateToken(e.ctx, tok+"x"); ok {
		t.Fatal("bad token accepted")
	}
	var plain int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM api_token WHERE token_hash = ?`, tok).Scan(&plain)
	if plain != 0 {
		t.Fatal("plaintext token stored")
	}
	short, _ := e.st.CreateToken(e.ctx, e.p.Name, "carol", "customer", "editor", "human", time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, ok, _ := e.st.AuthenticateToken(e.ctx, short); ok {
		t.Fatal("expired token accepted")
	}
}
