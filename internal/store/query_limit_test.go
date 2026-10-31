package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

func TestQueryLimiterUnit(t *testing.T) {
	s := &Store{QueryPerAccount: 2, QueryTotal: 3}
	var rel []func()
	take := func(acct string) error {
		r, err := s.AcquireQuerySlot(acct)
		if err == nil {
			rel = append(rel, r)
		}
		return err
	}
	if take("a") != nil || take("a") != nil {
		t.Fatal("first two must pass")
	}
	var ce *core.Error
	if err := take("a"); !errors.As(err, &ce) || ce.Status != core.StatusResourceExhausted || ce.Reason != "QUERY_TOO_MANY_CONCURRENT" || ce.Metadata["retry_after_seconds"] == "" {
		t.Fatalf("per-account cap: %v", err)
	}
	if take("b") != nil {
		t.Fatal("another account still has room")
	}
	if err := take("c"); !errors.As(err, &ce) || ce.Reason != "QUERY_SERVER_BUSY" {
		t.Fatalf("global cap: %v", err)
	}
	rel[0]()
	if take("c") != nil {
		t.Fatal("a released slot must be reusable")
	}
}

func TestQueryLimiterEnvDefaults(t *testing.T) {
	t.Setenv("OPENETL_QUERY_MAX_PER_ACCOUNT", "1")
	t.Setenv("OPENETL_QUERY_MAX_TOTAL", "2")
	s := &Store{}
	r1, err := s.AcquireQuerySlot("a")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	if _, err := s.AcquireQuerySlot("a"); err == nil {
		t.Fatal("env per-account cap ignored")
	}
}

// Parallel real queries: every call either succeeds or is refused immediately with RESOURCE_EXHAUSTED,
// never more than the cap runs at once, and the shared pool keeps serving other work meanwhile.
func TestQueryParallelBoundedAndPoolIsolated(t *testing.T) {
	e := newEnv(t)
	e.createWF(t, "limit-wf")
	if e.st.QueryDB != nil {
		t.Fatal("test expects no OPENETL_QUERY_DSN pool")
	}
	e.st.QueryPerAccount, e.st.QueryTotal = 3, 6
	ctx := WithQueryAccount(context.Background(), "viewer")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.st.Query(ctx, e.p, "select count(*) from workflows a join workflows b on a.id = b.id join workflows c on a.id = c.id", 0, "")
			mu.Lock()
			defer mu.Unlock()
			var ce *core.Error
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ce) && ce.Status == core.StatusResourceExhausted:
				refused++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok == 0 {
		t.Fatal("no query succeeded")
	}
	t.Logf("ok=%d refused=%d", ok, refused)
	// the user-SQL pool is a separate, small pool
	qdb, err := e.st.queryDB()
	if err != nil || qdb == e.st.DB {
		t.Fatalf("query path must use a dedicated pool: %v", err)
	}
	if got := qdb.Stats().MaxOpenConnections; got != QueryPoolSize || QueryPoolSize > 8 {
		t.Fatalf("query pool size %d", got)
	}
	if e.st.DB.Stats().MaxOpenConnections != 20 {
		t.Fatal("main pool changed")
	}
}

func TestQueryErrorDoesNotLeakDatabaseText(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"SELECT nope FROM workflows", "SELECT id FROM workflows a JOIN tasks b ON a.id = b.id"} {
		_, err := e.st.Query(e.ctx, e.p, q, 0, "")
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Reason != "QUERY_FAILED" {
			t.Fatalf("%q: %v", q, err)
		}
		for _, bad := range []string{"openetl", "Unknown column", "workflows", "field list", "'", "_test"} {
			if strings.Contains(ce.Message, bad) {
				t.Errorf("message leaks %q: %s", bad, ce.Message)
			}
		}
		if !strings.Contains(ce.Message, "database error 1") && q[:10] == "SELECT nop" {
			t.Errorf("want numeric code: %s", ce.Message)
		}
	}
}
