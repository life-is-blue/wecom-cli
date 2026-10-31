// Package store is the MySQL access layer: one write primitive (Apply), view-based reads,
// the migration runner and token/membership lookups. Hand-written SQL, no ORM.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/life-is-blue/openetl-cli/db"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// Store wraps the database handle.
type Store struct {
	DB *sql.DB
	// QueryDB, when set, is the (ideally SELECT-only on the public views) pool used for user SQL.
	QueryDB *sql.DB
	Now     func() time.Time // injectable clock; defaults to time.Now().UTC()

	dsn                         string // normalized main DSN, used for the lazily opened dedicated query pool
	qOnce                       sync.Once
	qdb                         *sql.DB
	qErr                        error
	qlim                        queryLimiter
	qlimOnce                    sync.Once
	QueryPerAccount, QueryTotal int // concurrency caps for user SQL; <=0 means the env/default value
}

// Open connects to MySQL. The DSN is a go-sql-driver DSN; parseTime and UTC are forced.
func Open(dsn string) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4"
	d, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(20)
	d.SetConnMaxLifetime(5 * time.Minute)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{DB: d, dsn: cfg.FormatDSN()}, nil
}

// OpenQuery opens the separate pool used for user queries (OPENETL_QUERY_DSN) and attaches it.
func (s *Store) OpenQuery(dsn string) error {
	q, err := Open(dsn)
	if err != nil {
		return err
	}
	q.DB.SetMaxOpenConns(QueryPoolSize)
	s.QueryDB = q.DB
	return nil
}

// Close closes the pools.
func (s *Store) Close() error {
	if s.QueryDB != nil {
		s.QueryDB.Close()
	}
	if s.qdb != nil {
		s.qdb.Close()
	}
	return s.DB.Close()
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

// Migrate applies the embedded migrations that are not yet recorded in schema_migrations.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	// serialize concurrent migrators (several servers, parallel tests) with an advisory lock
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('openetl_migrate', 60)`).Scan(&got); err != nil || got.Int64 != 1 {
		return nil, fmt.Errorf("could not take the migration lock: %v", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK('openetl_migrate')`)
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(64) PRIMARY KEY, applied_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var applied []string
	for _, n := range names {
		var v string
		err := s.DB.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version=?`, n).Scan(&v)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return applied, err
		}
		body, err := fs.ReadFile(db.Migrations, "migrations/"+n)
		if err != nil {
			return applied, err
		}
		for i, stmt := range SplitSQL(string(body)) {
			if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
				return applied, fmt.Errorf("migration %s statement %d: %w", n, i+1, err)
			}
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, n, s.now()); err != nil {
			return applied, err
		}
		applied = append(applied, n)
	}
	return applied, nil
}

// SplitSQL splits a script into statements on ';', honouring quotes and -- / # line comments.
func SplitSQL(script string) []string {
	var out []string
	var cur strings.Builder
	rs := []rune(script)
	var quote rune
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			cur.WriteRune(c)
			if c == '\\' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"' || c == '`':
			quote = c
			cur.WriteRune(c)
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-' && (i+2 >= len(rs) || rs[i+2] == ' ' || rs[i+2] == '\n'):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			cur.WriteRune('\n')
		case c == ';':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func internalErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	return core.NewError(core.StatusInternal, "INTERNAL", "internal error: %v", err)
}

func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1213
}

func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// placeholders returns "?,?,?" for n args.
func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func idArgs(ids []int64) []any {
	a := make([]any, len(ids))
	for i, v := range ids {
		a[i] = v
	}
	return a
}

func asCore(err error, target **core.Error) bool { return errors.As(err, target) }

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }
