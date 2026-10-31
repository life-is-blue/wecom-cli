package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

type cenv struct {
	dsn string
	st  *store.Store
	pid int64
	ctx context.Context
}

func newCEnv(t testing.TB) *cenv {
	st, dsn := freshStoreDSN(t)
	ctx := context.Background()
	name := fmt.Sprintf("c%d", time.Now().UnixNano()%1e9)
	if _, err := st.CreateToken(ctx, name, "alice", "delivery", "editor", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	m, _, err := st.MemberOf(ctx, name, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return &cenv{dsn: dsn, st: st, pid: m.ProjectID, ctx: ctx}
}

func (e *cenv) actor() core.Actor {
	return core.Actor{Account: "alice", Side: "delivery", Role: "editor", Channel: core.ChannelAPI}
}

func plan(kv ...string) func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
	return func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		var out []store.PlannedChange
		for i := 0; i < len(kv); i += 2 {
			out = append(out, store.PlannedChange{Key: kv[i], New: core.JSON(kv[i+1])})
		}
		return out, nil
	}
}

func (e *cenv) wf(t testing.TB, name string) int64 {
	r, err := e.st.Apply(e.ctx, store.ChangeRequest{ProjectID: e.pid, EntityType: "workflow", Actor: e.actor(), Plan: plan("name", name)})
	if err != nil {
		t.Fatal(err)
	}
	return r.EntityID
}

func (e *cenv) merge(id, into int64) error {
	_, err := e.st.Apply(e.ctx, store.ChangeRequest{ProjectID: e.pid, EntityType: "workflow", EntityID: id, ReasonKey: "merged", Actor: e.actor(),
		Plan: func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: "scope", New: core.JSON("out")}, {Key: "into", New: json.RawMessage(strconv.FormatInt(into, 10))}}, nil
		}})
	return err
}

// Two merges that each look fine alone (A into B, B into A) must not both commit.
func TestConcurrentMergesNeverFormACycle(t *testing.T) {
	e := newCEnv(t)
	for round := 0; round < 40; round++ {
		a, b := e.wf(t, "a"), e.wf(t, "b")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, pair := range [][2]int64{{a, b}, {b, a}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = e.merge(pair[0], pair[1])
			}()
		}
		close(start)
		wg.Wait()
		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("round %d: both merges committed: %d -> %d and %d -> %d is a cycle", round, a, b, b, a)
		}
	}
	requireCleanLog(t, e.st)
}

// Many writers on one entity: every success is exactly one event, ids rise, old values chain.
func TestConcurrentWritersOnOneEntity(t *testing.T) {
	e := newCEnv(t)
	id := e.wf(t, "shared")
	const writers, each = 16, 15
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := e.st.Apply(e.ctx, store.ChangeRequest{ProjectID: e.pid, EntityType: "workflow", EntityID: id, Actor: e.actor(),
					Plan: plan("attr.k", fmt.Sprintf("g%d-%d", g, i))})
				if err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				} else {
					t.Errorf("write failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE entity_id=?`, id).Scan(&n)
	if n != ok+1 {
		t.Fatalf("%d events for %d acknowledged writes (+1 create)", n, ok)
	}
	requireCleanLog(t, e.st)
}

// One request_id raced from many goroutines: one event, everybody gets the same answer.
func TestConcurrentSameRequestID(t *testing.T) {
	e := newCEnv(t)
	for round := 0; round < 10; round++ {
		key := fmt.Sprintf("00000000-0000-4000-8000-%012d", round)
		var wg sync.WaitGroup
		res := make([]*store.Result, 24)
		errs := make([]error, 24)
		for g := range res {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[g], errs[g] = e.st.Apply(e.ctx, store.ChangeRequest{ProjectID: e.pid, EntityType: "workflow", RequestID: key, Fingerprint: "fp",
					Actor: e.actor(), Plan: plan("name", "once")})
			}()
		}
		wg.Wait()
		replays := 0
		for g := range res {
			if errs[g] != nil {
				t.Fatalf("round %d: %v", round, errs[g])
			}
			if res[g].EventID != res[0].EventID || res[g].EntityID != res[0].EntityID {
				t.Fatalf("round %d: different answers %+v %+v", round, res[g], res[0])
			}
			if res[g].Replayed {
				replays++
			}
		}
		if replays != len(res)-1 {
			t.Fatalf("round %d: %d replays, want %d", round, replays, len(res)-1)
		}
		var n int
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE idempotency_key=?`, key).Scan(&n)
		if n != 1 {
			t.Fatalf("round %d: %d events for one request_id", round, n)
		}
	}
	requireCleanLog(t, e.st)
}

// A client whose connection is killed in the middle of a transaction sees an error and nothing
// partial persists; retrying with the same request_id then succeeds exactly once.
func TestKilledConnectionLeavesNothingPartial(t *testing.T) {
	e := newCEnv(t)
	existing := e.wf(t, "victim")

	// the victim store has a single connection whose id we know, so the kill hits exactly the writer
	// same DSN normalisation as store.Open (parseTime, UTC), otherwise reading timestamps back fails
	vcfg, err := mysql.ParseDSN(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	vcfg.ParseTime, vcfg.Loc = true, time.UTC
	vdb, err := sql.Open("mysql", vcfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer vdb.Close()
	vdb.SetMaxOpenConns(1)
	victim := &store.Store{DB: vdb}

	for _, create := range []bool{true, false} {
		var connID int64
		if err := vdb.QueryRow(`SELECT CONNECTION_ID()`).Scan(&connID); err != nil {
			t.Fatal(err)
		}
		var before, entsBefore int
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&before)
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM entity`).Scan(&entsBefore)
		inTx, proceed := make(chan struct{}), make(chan struct{})
		key := fmt.Sprintf("11111111-0000-4000-8000-%012d", map[bool]int{true: 1, false: 2}[create])
		req := store.ChangeRequest{ProjectID: e.pid, EntityType: "workflow", RequestID: key, Fingerprint: "fp", Actor: e.actor()}
		if !create {
			req.EntityID = existing
		}
		req.Plan = func(cur *store.Entity, env core.Env) ([]store.PlannedChange, error) {
			close(inTx) // the entity row is inserted (create) or locked (update) and uncommitted
			<-proceed
			return []store.PlannedChange{{Key: "name", New: core.JSON("new name")}, {Key: "attr.x", New: core.JSON("1")}}, nil
		}
		done := make(chan error, 1)
		go func() { _, err := victim.Apply(e.ctx, req); done <- err }()
		<-inTx
		_, kerr := e.st.DB.Exec(fmt.Sprintf("KILL %d", connID))
		close(proceed)
		if kerr != nil {
			t.Fatalf("KILL: %v", kerr)
		}
		if err := <-done; err == nil {
			t.Fatalf("create=%v: the client was told the write succeeded after its connection was killed", create)
		}
		var after, entsAfter int
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&after)
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM entity`).Scan(&entsAfter)
		if after != before || entsAfter != entsBefore {
			t.Fatalf("create=%v: partial state persisted: events %d->%d entities %d->%d", create, before, after, entsBefore, entsAfter)
		}
		requireCleanLog(t, e.st)
		// the retry works and is applied exactly once
		req.Plan = plan("name", "new name", "attr.x", "1")
		for i := 0; i < 2; i++ {
			if _, err := victim.Apply(e.ctx, req); err != nil {
				t.Fatalf("create=%v retry %d: %v", create, i, err)
			}
		}
		var n int
		e.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE idempotency_key=?`, key).Scan(&n)
		if n != 1 {
			t.Fatalf("create=%v: %d events after retry", create, n)
		}
	}
	requireCleanLog(t, e.st)
}
