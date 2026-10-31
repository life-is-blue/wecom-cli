package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

const (
	queryTimeout   = 15 * time.Second
	queryStmtMS    = 10000 // server-side statement timeout in milliseconds
	queryDefaultPS = 1000
	queryMaxPS     = 1000
	// QueryPoolSize is the connection cap of the dedicated query pool.
	QueryPoolSize = 8
	// DefaultQueryMaxPerAccount / DefaultQueryMaxTotal are the default concurrency caps for user SQL
	// (OPENETL_QUERY_MAX_PER_ACCOUNT / OPENETL_QUERY_MAX_TOTAL).
	DefaultQueryMaxPerAccount = 3
	DefaultQueryMaxTotal      = 6
	// QueryMaxBytes is the default cap of the JSON size of one query response page.
	QueryMaxBytes = 8 << 20
)

var queryMaxBytes = QueryMaxBytes

// setStatementTimeout applies a server-side statement timeout to the connection: MySQL uses
// MAX_EXECUTION_TIME (milliseconds), MariaDB max_statement_time (seconds). Each variable is unknown
// on the other server; if neither could be set the query must not run. The returned func resets
// the variable that was set.
func setStatementTimeout(ctx context.Context, conn *sql.Conn) (func(), error) {
	var undo []string
	if _, err := conn.ExecContext(ctx, "SET SESSION MAX_EXECUTION_TIME = "+strconv.Itoa(queryStmtMS)); err == nil {
		undo = append(undo, "SET SESSION MAX_EXECUTION_TIME = 0")
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION max_statement_time = "+strconv.Itoa(queryStmtMS/1000)); err == nil {
		undo = append(undo, "SET SESSION max_statement_time = 0")
	}
	if len(undo) == 0 {
		return nil, core.NewError(core.StatusFailedPrecondition, "QUERY_TIMEOUT_UNSUPPORTED", "the database does not support a statement timeout; refusing to run the query")
	}
	return func() {
		c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, u := range undo {
			_, _ = conn.ExecContext(c2, u)
		}
	}, nil
}

// queryDB is the handle used for user SQL: always a dedicated, small pool, so user queries can never
// starve the connections used by authentication and writes. It is the OPENETL_QUERY_DSN pool when
// configured, otherwise a separate pool opened (lazily) on the main DSN.
func (s *Store) queryDB() (*sql.DB, error) {
	if s.QueryDB != nil {
		return s.QueryDB, nil
	}
	s.qOnce.Do(func() {
		if s.dsn == "" {
			s.qErr = errors.New("no dsn available for the query pool")
			return
		}
		q, err := Open(s.dsn)
		if err != nil {
			s.qErr = err
			return
		}
		q.DB.SetMaxOpenConns(QueryPoolSize)
		s.qdb = q.DB
	})
	if s.qErr != nil {
		return nil, s.qErr
	}
	return s.qdb, nil
}

type queryAcctKey struct{}

// WithQueryAccount tags ctx with the account whose query concurrency is limited.
func WithQueryAccount(ctx context.Context, account string) context.Context {
	return context.WithValue(ctx, queryAcctKey{}, account)
}

// queryLimiter caps concurrent user queries per account and in total. It never queues: a caller over
// the limit is refused at once.
type queryLimiter struct {
	mu       sync.Mutex
	total    int
	perAcct  map[string]int
	maxAcct  int
	maxTotal int
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return v
	}
	return def
}

// AcquireQuerySlot takes one query-concurrency slot for the account (see Query); call the returned func to release it.
func (s *Store) AcquireQuerySlot(account string) (func(), error) { return s.acquireQuery(account) }

func (s *Store) acquireQuery(account string) (func(), error) {
	s.qlimOnce.Do(func() {
		s.qlim.perAcct = map[string]int{}
		s.qlim.maxAcct = s.QueryPerAccount
		if s.qlim.maxAcct <= 0 {
			s.qlim.maxAcct = envInt("OPENETL_QUERY_MAX_PER_ACCOUNT", DefaultQueryMaxPerAccount)
		}
		s.qlim.maxTotal = s.QueryTotal
		if s.qlim.maxTotal <= 0 {
			s.qlim.maxTotal = envInt("OPENETL_QUERY_MAX_TOTAL", DefaultQueryMaxTotal)
		}
	})
	l := &s.qlim
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perAcct[account] >= l.maxAcct {
		return nil, core.NewError(core.StatusResourceExhausted, "QUERY_TOO_MANY_CONCURRENT",
			"too many concurrent queries for this account (max %d); retry shortly", l.maxAcct).With("retry_after_seconds", "1")
	}
	if l.total >= l.maxTotal {
		return nil, core.NewError(core.StatusResourceExhausted, "QUERY_SERVER_BUSY",
			"the query service is busy (max %d concurrent queries); retry shortly", l.maxTotal).With("retry_after_seconds", "1")
	}
	l.total++
	l.perAcct[account]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		if l.perAcct[account]--; l.perAcct[account] <= 0 {
			delete(l.perAcct, account)
		}
	}, nil
}

// MySQL/MariaDB error numbers caused by the user's SQL text (not by the server): their number is
// returned, their text (which can name databases and tables) is not.
var userSQLErrors = map[uint16]bool{
	1052: true, 1054: true, 1055: true, 1064: true, 1066: true, 1111: true, 1139: true, 1140: true, 1241: true, 1242: true,
	1248: true, 1249: true, 1267: true, 1270: true, 1271: true, 1305: true, 1318: true, 1364: true, 1365: true, 1366: true,
	1367: true, 1408: true, 1525: true, 1582: true, 1583: true, 1690: true, 3143: true, 3144: true, 3146: true, 3156: true,
	3141: true, 3140: true, 4078: true,
}

// queryFailed maps a database error to a client error without leaking its text; the raw error is logged.
func queryFailed(err error) error {
	log.Printf("query failed: %v", err)
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 3024, 1969:
			return core.NewError(core.StatusInvalidArgument, "QUERY_FAILED", "query failed: it exceeded the time limit")
		}
		if userSQLErrors[me.Number] {
			return core.NewError(core.StatusInvalidArgument, "QUERY_FAILED", "query failed (database error %d); check the column names and expressions", me.Number)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return core.NewError(core.StatusInvalidArgument, "QUERY_FAILED", "query failed: it exceeded the time limit")
	}
	return internalErr(err)
}

// Query runs a guarded read-only SELECT over the public views, restricted to the project.
// page_token is an opaque row offset: add ORDER BY for stable pagination.
func (s *Store) Query(ctx context.Context, p Project, sqlText string, pageSize int, pageToken string) (*api.QueryResponse, error) {
	rewritten, err := GuardQuery(sqlText, p.ID)
	if err != nil {
		return nil, err
	}
	if pageSize < 0 {
		return nil, core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_SIZE", "page_size must not be negative")
	}
	if pageSize == 0 {
		pageSize = queryDefaultPS
	}
	if pageSize > queryMaxPS {
		pageSize = queryMaxPS
	}
	offset := 0
	if pageToken != "" {
		kind, n, err := openPageToken(pageToken, queryBinding(p, sqlText))
		if err != nil || kind != "o" {
			return nil, badPageToken("invalid page_token")
		}
		offset = int(n)
	}

	account, _ := ctx.Value(queryAcctKey{}).(string)
	release, err := s.acquireQuery(account)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	qdb, err := s.queryDB()
	if err != nil {
		return nil, internalErr(err)
	}
	conn, err := qdb.Conn(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	defer conn.Close()
	reset, err := setStatementTimeout(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer reset()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, rewritten)
	if err != nil {
		return nil, queryFailed(err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &api.QueryResponse{}
	cols := make([]struct {
		Name *string `json:"name,omitempty"`
		Type *string `json:"type,omitempty"`
	}, len(cts))
	for i, ct := range cts {
		cols[i].Name = ptr(ct.Name())
		cols[i].Type = ptr(strings.ToLower(ct.DatabaseTypeName()))
	}
	resp.Columns = &cols
	out := [][]*api.AnyValue{}
	n, total := 0, 0
	more := false
	for rows.Next() {
		if n < offset {
			n++
			continue
		}
		if len(out) >= pageSize {
			more = true
			break
		}
		dest := make([]any, len(cts))
		for i := range dest {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, internalErr(err)
		}
		row := make([]*api.AnyValue, len(cts))
		for i := range dest {
			v := convertValue(*(dest[i].(*any)), cts[i])
			row[i] = &v
		}
		if rb, err := json.Marshal(row); err == nil {
			total += len(rb)
		}
		if total > queryMaxBytes {
			return nil, core.NewError(core.StatusInvalidArgument, "RESULT_TOO_LARGE", "the result exceeds %d bytes; select fewer columns or use a smaller page_size", queryMaxBytes)
		}
		out = append(out, row)
		n++
	}
	if err := rows.Err(); err != nil {
		return nil, queryFailed(err)
	}
	resp.Rows = &out
	if more {
		resp.NextPageToken = ptr(sealPageToken("o", int64(offset+pageSize), queryBinding(p, sqlText)))
	}
	return resp, nil
}

func convertValue(v any, ct *sql.ColumnType) api.AnyValue {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		name := strings.ToUpper(ct.DatabaseTypeName())
		switch name {
		case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT", "UNSIGNED BIGINT", "UNSIGNED INT", "UNSIGNED TINYINT",
			"UNSIGNED SMALLINT", "UNSIGNED MEDIUMINT", "DECIMAL", "DOUBLE", "FLOAT", "YEAR":
			if json.Valid(x) {
				return json.RawMessage(append([]byte(nil), x...))
			}
		case "JSON":
			if json.Valid(x) {
				return json.RawMessage(append([]byte(nil), x...))
			}
		}
		// MariaDB reports JSON as text: pass through well-formed objects of the known JSON view columns
		switch strings.ToLower(ct.Name()) {
		case "attrs", "assignees", "old_v", "new_v", "summary":
			if json.Valid(x) {
				return json.RawMessage(append([]byte(nil), x...))
			}
		}
		return string(x)
	}
	return v
}
