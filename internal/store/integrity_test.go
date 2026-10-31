package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/life-is-blue/openetl-cli/internal/store"
)

var dbSeq atomic.Int64

// freshStore opens OPENETL_TEST_DSN's server, creates a private database on it (dropped at cleanup),
// migrates it and returns a Store on it. If the user may not create databases the DSN's own database
// is used. Skips when OPENETL_TEST_DSN is unset.
func freshStore(t testing.TB) *store.Store {
	st, _ := freshStoreDSN(t)
	return st
}

// freshStoreDSN is freshStore that also returns the DSN of the private database.
func freshStoreDSN(t testing.TB) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s_t%d_%d", strings.TrimSuffix(cfg.DBName, "_"), time.Now().UnixNano()%1e10, dbSeq.Add(1))
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	use := dsn
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err == nil {
		c := *cfg
		c.DBName = name
		use = c.FormatDSN()
		t.Cleanup(func() {
			a, err := sql.Open("mysql", dsn)
			if err == nil {
				a.Exec("DROP DATABASE `" + name + "`")
				a.Close()
			}
		})
	}
	st, err := store.Open(use)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st, use
}

// logProblems checks the structural invariants of the event log and returns one line per violation:
//   - every event has at least one delta (no event without its deltas);
//   - every delta belongs to an event of the same entity (no orphan or crossed delta);
//   - per entity, event ids and recording times never go backwards together (ids strictly increase);
//   - each delta's old value is the new value of the previous delta of the same (entity, key).
func logProblems(ctx context.Context, db *sql.DB) ([]string, error) {
	var out []string
	one := func(label, q string) error {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var a, b sql.NullString
			if err := rows.Scan(&a, &b); err != nil {
				return err
			}
			if n++; n <= 5 {
				out = append(out, fmt.Sprintf("%s: %s %s", label, a.String, b.String))
			}
		}
		return rows.Err()
	}
	if err := one("event without delta", `SELECT ev.id, ev.entity_id FROM event ev
		WHERE NOT EXISTS (SELECT 1 FROM delta d WHERE d.event_id = ev.id)`); err != nil {
		return nil, err
	}
	if err := one("delta without matching event", `SELECT d.event_id, d.k FROM delta d
		LEFT JOIN event ev ON ev.id = d.event_id AND ev.entity_id = d.entity_id WHERE ev.id IS NULL`); err != nil {
		return nil, err
	}
	if err := one("event ids not in recording order", `SELECT a.id, b.id FROM event a JOIN event b
		ON a.entity_id = b.entity_id AND a.id < b.id AND a.create_time > b.create_time`); err != nil {
		return nil, err
	}
	if err := one("old_v is not the previous new_v", `SELECT d.event_id, d.k FROM delta d
		LEFT JOIN delta p ON p.entity_id = d.entity_id AND p.k = d.k AND p.event_id =
			(SELECT MAX(x.event_id) FROM delta x WHERE x.entity_id = d.entity_id AND x.k = d.k AND x.event_id < d.event_id)
		WHERE NOT (CAST(d.old_v AS CHAR) <=> CAST(p.new_v AS CHAR))`); err != nil {
		return nil, err
	}
	if err := one("entity of event in another project", `SELECT ev.id, ev.entity_id FROM event ev
		JOIN entity en ON en.id = ev.entity_id WHERE en.project_id <> ev.project_id`); err != nil {
		return nil, err
	}
	return out, nil
}

func requireCleanLog(t testing.TB, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if p, err := logProblems(ctx, st.DB); err != nil {
		t.Fatal(err)
	} else if len(p) > 0 {
		t.Fatalf("event log integrity: %s", strings.Join(p, "; "))
	}
	if n, first, err := st.VerifyLatest(ctx, 5); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("VerifyLatest: %d mismatches, first %v", n, first)
	}
}
