//go:build perf

package store

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// TestPerf times the store/API-level reads and one write against a database seeded by
// scripts/perf-seed.sh (project "perf"):
//
//	OPENETL_PERF_DSN='user:pw@tcp(127.0.0.1:33306)/openetl_perf2' go test -tags perf -run TestPerf -v ./internal/store
//
// Each operation runs 5 times with a 60s limit; the median is reported.
func TestPerf(t *testing.T) {
	dsn := os.Getenv("OPENETL_PERF_DSN")
	if dsn == "" {
		t.Skip("OPENETL_PERF_DSN not set")
	}
	st, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if os.Getenv("OPENETL_PERF_MIGRATE") != "" {
		if _, err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p := Project{Name: "perf"}
	if err := st.DB.QueryRow(`SELECT id FROM project WHERE name='perf'`).Scan(&p.ID); err != nil {
		t.Fatal(err)
	}
	var wf, wfOpen, comment int64
	st.DB.QueryRow(`SELECT MIN(id) FROM entity WHERE type='workflow'`).Scan(&wf)
	st.DB.QueryRow(`SELECT MIN(id) FROM entity WHERE type='comment'`).Scan(&comment)
	// workflows without any stage delta, to advance one after the other
	rows, err := st.DB.Query(`SELECT e.id FROM entity e WHERE e.type='workflow' AND NOT EXISTS
		(SELECT 1 FROM delta d WHERE d.entity_id=e.id AND d.k LIKE 'stage.%') ORDER BY e.id LIMIT 20`)
	if err != nil {
		t.Fatal(err)
	}
	var fresh []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		fresh = append(fresh, id)
	}
	rows.Close()
	var task int64
	st.DB.QueryRow(`SELECT MIN(id) FROM entity WHERE type='task'`).Scan(&task)
	_ = wfOpen

	measure := func(name string, f func(ctx context.Context, i int) error) {
		var ds []time.Duration
		for i := 0; i < 5; i++ {
			c, cancel := context.WithTimeout(ctx, 60*time.Second)
			t0 := time.Now()
			err := f(c, i)
			d := time.Since(t0)
			cancel()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			ds = append(ds, d)
		}
		sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
		t.Logf("PERF %-34s median %9.1f ms  (max %.1f ms)", name, float64(ds[2].Microseconds())/1000, float64(ds[4].Microseconds())/1000)
	}
	measure("workflows list page of 100", func(c context.Context, _ int) error {
		_, _, err := st.ListWorkflows(c, p, ListOpts{PageSize: 100})
		return err
	})
	measure("workflows list filter stage+health", func(c context.Context, _ int) error {
		_, _, err := st.ListWorkflows(c, p, ListOpts{PageSize: 100, Filter: `current_stage = "verify" AND health = "BLOCKED"`})
		return err
	})
	measure("workflows list order by display_name", func(c context.Context, _ int) error {
		_, _, err := st.ListWorkflows(c, p, ListOpts{PageSize: 100, OrderBy: "display_name"})
		return err
	})
	measure("workflow get", func(c context.Context, _ int) error {
		_, err := st.GetWorkflow(c, p, wf)
		return err
	})
	measure("tasks list page of 100", func(c context.Context, _ int) error {
		_, _, err := st.ListTasks(c, p, ListOpts{PageSize: 100})
		return err
	})
	measure("task get", func(c context.Context, _ int) error {
		_, err := st.GetTask(c, p, task)
		return err
	})
	measure("comments list (one workflow)", func(c context.Context, _ int) error {
		_, _, err := st.ListComments(c, p, wf, ListOpts{PageSize: 100})
		return err
	})
	measure("events list (one workflow)", func(c context.Context, _ int) error {
		_, _, err := st.ListEvents(c, p, wf, ListOpts{PageSize: 100})
		return err
	})
	for _, q := range []string{"workflows", "tasks", "comments", "events", "workflow_stages", "open_items", "stage_distribution"} {
		q := q
		measure("query COUNT(*) "+q, func(c context.Context, _ int) error {
			_, err := st.Query(c, p, "SELECT COUNT(*) FROM "+q, 10, "")
			return err
		})
	}
	if len(fresh) >= 5 {
		measure("stage advance (Apply)", func(c context.Context, i int) error {
			_, err := st.Apply(c, ChangeRequest{ProjectID: p.ID, EntityType: "workflow", EntityID: fresh[i],
				Actor: core.Actor{Account: "alice", Side: "delivery", Role: "admin", Channel: core.ChannelAPI}, Plan: set("stage.development", "pass")})
			return err
		})
	}
}
