package importsrc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var identRe = regexp.MustCompile(`^[A-Za-z0-9_$]+(\.[A-Za-z0-9_$]+)?$`)

type mysqlSource struct {
	cfg      *mysql.Config
	password string
	query    string
	label    string
}

func (s *mysqlSource) Label() string { return s.label }

// openMySQL parses mysql://user[:password]@host[:port]/db. The password comes
// from the DSN, else from OPENETL_SOURCE_PASSWORD; it is never echoed.
func openMySQL(from string, m *Mapping, getenv func(string) string) (Source, error) {
	u, err := url.Parse(from)
	if err != nil {
		return nil, errors.New("invalid mysql:// source URL")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if u.User == nil || u.User.Username() == "" || u.Host == "" || db == "" {
		return nil, errors.New("mysql source must look like mysql://user@host:port/db")
	}
	pw, ok := u.User.Password()
	if !ok {
		pw = getenv(EnvSourcePassword)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "3306")
	}
	cfg := mysql.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd = pw
	cfg.Net = "tcp"
	cfg.Addr = host
	cfg.DBName = db
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 5 * time.Minute
	cfg.ParseTime = false
	cfg.Collation = "utf8mb4_general_ci"
	for k, v := range u.Query() { // e.g. tls=true
		if k == "tls" && len(v) == 1 {
			cfg.TLSConfig = v[0]
		}
	}
	q, err := selectQuery("source", m.Source.Table, m.Source.Query, m.Source.OrderBy)
	if err != nil {
		return nil, err
	}
	return &mysqlSource{cfg: cfg, password: pw, query: q, label: "mysql://" + u.User.Username() + "@" + host + "/" + db}, nil
}

// selectQuery builds the single SELECT of a table or query spec; what names it in errors ("source", "tasks").
func selectQuery(what, table, query, orderBy string) (string, error) {
	switch {
	case query != "":
		q := strings.TrimSpace(query)
		low := strings.ToLower(q)
		if !(strings.HasPrefix(low, "select") || strings.HasPrefix(low, "with")) || strings.Contains(strings.TrimRight(q, "; \n"), ";") {
			return "", fmt.Errorf("mapping: %s.query must be a single SELECT statement", what)
		}
		return q, nil
	case table != "":
		if !identRe.MatchString(table) {
			return "", fmt.Errorf("mapping: invalid %s.table %q", what, table)
		}
		q := "SELECT * FROM " + quoteIdent(table)
		if orderBy != "" {
			if !identRe.MatchString(orderBy) {
				return "", fmt.Errorf("mapping: invalid %s.order_by %q", what, orderBy)
			}
			q += " ORDER BY " + quoteIdent(orderBy)
		}
		return q, nil
	}
	return "", fmt.Errorf("mapping: %s.table or %s.query is required for a mysql:// source", what, what)
}

func quoteIdent(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = "`" + p + "`"
	}
	return strings.Join(parts, ".")
}

func (s *mysqlSource) scrub(err error) error {
	if err == nil || s.password == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), s.password, "***"))
}

func (s *mysqlSource) Scan(ctx context.Context, fn func(Row) error) error {
	return s.scanQuery(ctx, s.query, fn)
}

// ScanTasks runs the mapping's tasks.table / tasks.query on the same server (read-only).
func (s *mysqlSource) ScanTasks(ctx context.Context, t *TasksSpec, fn func(Row) error) error {
	q, err := selectQuery("tasks", t.Table, t.Query, "")
	if err != nil {
		return err
	}
	return s.scanQuery(ctx, q, fn)
}

func (s *mysqlSource) scanQuery(ctx context.Context, query string, fn func(Row) error) error {
	conn, err := mysql.NewConnector(s.cfg)
	if err != nil {
		return s.scrub(err)
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Read-only transaction: the server rejects any write.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("connect %s: %w", s.label, s.scrub(err))
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query %s: %w", s.label, s.scrub(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return s.scrub(err)
	}
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return s.scrub(err)
		}
		row := make(Row, len(cols))
		for i, c := range cols {
			if vals[i].Valid {
				row[c] = vals[i].String
			} else {
				row[c] = nil
			}
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return s.scrub(rows.Err())
}
