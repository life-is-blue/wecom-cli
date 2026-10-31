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
	queryDefaultPS = 1000
	queryMaxPS     = 10000
)

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
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	defer conn.Close()
	// best-effort server-side limits: MariaDB (seconds) and MySQL (milliseconds); each fails on the other server
	_, _ = conn.ExecContext(ctx, "SET SESSION max_statement_time = 10")
	_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = 10000")
	defer func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		_, _ = conn.ExecContext(c2, "SET SESSION max_statement_time = 0")
		_, _ = conn.ExecContext(c2, "SET SESSION max_execution_time = 0")
	}()
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
	n := 0
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
