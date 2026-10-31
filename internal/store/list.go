package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// ListOpts are the common list parameters.
type ListOpts struct {
	Filter      string
	OrderBy     string
	PageSize    int
	PageToken   string
	ShowDeleted bool
	AsOf        time.Time
}

const (
	defaultPageSize = 50
	maxPageSize     = 1000
)

var safeKey = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

// latestRaw is a SQL expression for the latest raw JSON value of a key of entity alias.id (keys are validated).
func latestRaw(alias, key string) string {
	return "(SELECT l.v FROM entity_latest l WHERE l.entity_id = " + alias + ".id AND l.k = '" + key + "')"
}

func latestText(alias, key string) string { return "JSON_UNQUOTE(" + latestRaw(alias, key) + ")" }

type orderSpec struct {
	expr string
	byID bool // equivalent to ordering by the id column: enables keyset paging
}

type listQuery struct {
	from    string
	idCol   string
	where   []string
	args    []any
	orders  map[string]orderSpec
	defDesc bool
}

// run executes the query and returns up to PageSize ids plus the next page token.
func (lq *listQuery) run(ctx context.Context, q execer, o ListOpts) ([]int64, string, error) {
	size := o.PageSize
	if size < 0 {
		return nil, "", core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_SIZE", "page_size must not be negative")
	}
	if size == 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	type ord struct {
		spec orderSpec
		desc bool
	}
	var ords []ord
	for _, part := range strings.Split(o.OrderBy, ",") {
		f := strings.Fields(part)
		if len(f) == 0 {
			continue
		}
		spec, ok := lq.orders[strings.ToLower(f[0])]
		if !ok || len(f) > 2 {
			return nil, "", core.NewError(core.StatusInvalidArgument, "INVALID_ORDER_BY", "cannot order by %q", strings.TrimSpace(part))
		}
		desc := false
		if len(f) == 2 {
			switch strings.ToLower(f[1]) {
			case "desc":
				desc = true
			case "asc":
			default:
				return nil, "", core.NewError(core.StatusInvalidArgument, "INVALID_ORDER_BY", "bad direction %q", f[1])
			}
		}
		ords = append(ords, ord{spec, desc})
	}
	keyset := len(ords) == 0 || (len(ords) == 1 && ords[0].spec.byID)
	desc := len(ords) == 1 && ords[0].desc

	where := append([]string{}, lq.where...)
	args := append([]any{}, lq.args...)
	offset := 0
	if o.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(o.PageToken)
		kind, val, ok := strings.Cut(string(raw), ":")
		n, err2 := strconv.ParseInt(val, 10, 64)
		if err != nil || !ok || err2 != nil || n < 0 {
			return nil, "", core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_TOKEN", "invalid page_token")
		}
		switch {
		case kind == "k" && keyset:
			if desc {
				where = append(where, lq.idCol+" < ?")
			} else {
				where = append(where, lq.idCol+" > ?")
			}
			args = append(args, n)
		case kind == "o" && !keyset:
			offset = int(n)
		default:
			return nil, "", core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_TOKEN", "page_token does not match this query")
		}
	}
	var ob []string
	for _, x := range ords {
		d := "ASC"
		if x.desc {
			d = "DESC"
		}
		ob = append(ob, x.spec.expr+" "+d)
	}
	tie := "ASC"
	if desc {
		tie = "DESC"
	}
	ob = append(ob, lq.idCol+" "+tie)
	query := "SELECT " + lq.idCol + " FROM " + lq.from + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + strings.Join(ob, ", ") + fmt.Sprintf(" LIMIT %d", size+1)
	if offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > size {
		ids = ids[:size]
		if keyset {
			next = "k:" + strconv.FormatInt(ids[len(ids)-1], 10)
		} else {
			next = "o:" + strconv.Itoa(offset+size)
		}
		next = base64.RawURLEncoding.EncodeToString([]byte(next))
	}
	return ids, next, nil
}

// parseTime parses an RFC 3339 filter value.
func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return t, badFilter("%q is not an RFC 3339 timestamp", v)
	}
	return t.UTC(), nil
}
