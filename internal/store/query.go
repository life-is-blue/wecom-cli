package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

const (
	queryTimeout   = 15 * time.Second
	queryStmtMS    = 10000 // server-side statement timeout in milliseconds
	queryDefaultPS = 1000
	queryMaxPS     = 1000
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

// queryDB is the handle used for user SQL: the dedicated read-only pool when configured.
func (s *Store) queryDB() *sql.DB {
	if s.QueryDB != nil {
		return s.QueryDB
	}
	return s.DB
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
		raw, err := base64.RawURLEncoding.DecodeString(pageToken)
		n, err2 := strconv.Atoi(strings.TrimPrefix(string(raw), "o:"))
		if err != nil || err2 != nil || n < 0 || !strings.HasPrefix(string(raw), "o:") {
			return nil, core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_TOKEN", "invalid page_token")
		}
		offset = n
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	conn, err := s.queryDB().Conn(ctx)
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
		return nil, core.NewError(core.StatusInvalidArgument, "QUERY_FAILED", "query failed: %v", err)
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
		return nil, core.NewError(core.StatusInvalidArgument, "QUERY_FAILED", "query failed: %v", err)
	}
	resp.Rows = &out
	if more {
		resp.NextPageToken = ptr(base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset+pageSize))))
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
