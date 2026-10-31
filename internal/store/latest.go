package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// entity_latest is a derived cache of the append-only delta log: the newest value per
// (entity_id, k). delta is the only source of truth; the cache is written in the same transaction
// as the deltas (upsertLatest), can be rebuilt at any time (RebuildLatest) and checked against a
// recomputation (VerifyLatest). A removed key keeps a row with v = NULL.

// latestRow is one delta as seen by the cache.
type latestRow struct {
	EventID, EntityID int64
	Key               string
	New               sql.NullString
}

// upsertLatest merges delta rows into entity_latest. A row only replaces the cached value when its
// event_id is greater, so replays and out-of-order batches cannot move the cache backwards. Within
// one statement rows are applied in order. (Assignment order matters: v is decided from the old
// event_id before event_id itself is updated.)
func upsertLatest(ctx context.Context, q execer, rows []latestRow) error {
	for i := 0; i < len(rows); i += 400 {
		chunk := rows[i:min(i+400, len(rows))]
		args := make([]any, 0, len(chunk)*4)
		for _, r := range chunk {
			args = append(args, r.EntityID, r.Key, r.New, r.EventID)
		}
		_, err := q.ExecContext(ctx, `INSERT INTO entity_latest(entity_id, k, v, event_id) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(chunk)), ",")+
			` ON DUPLICATE KEY UPDATE v = IF(VALUES(event_id) > event_id, VALUES(v), v),
			event_id = IF(VALUES(event_id) > event_id, VALUES(event_id), event_id)`, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

// latestFromDelta recomputes the newest value per (entity, key) straight from delta.
const latestFromDelta = `SELECT d.entity_id, d.k, d.new_v AS v, d.event_id FROM delta d
	JOIN (SELECT entity_id, k, MAX(event_id) AS event_id FROM delta GROUP BY entity_id, k) m
	ON m.entity_id = d.entity_id AND m.k = d.k AND m.event_id = d.event_id`

// RebuildLatest empties entity_latest and refills it from delta in one transaction (DELETE, not
// TRUNCATE, so it stays transactional) and returns the number of cached rows.
func (s *Store) RebuildLatest(ctx context.Context) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM entity_latest`); err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO entity_latest(entity_id, k, v, event_id) `+latestFromDelta)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, tx.Commit()
}

// LatestMismatch is one difference between entity_latest and the recomputation from delta.
type LatestMismatch struct {
	EntityID int64
	Key      string
	Problem  string // missing | extra | stale
	// Cache and Delta describe the event id and value on each side ("-" when absent).
	Cache, Delta string
}

func (m LatestMismatch) String() string {
	return fmt.Sprintf("entity %d key %q: %s (cache %s, delta %s)", m.EntityID, m.Key, m.Problem, m.Cache, m.Delta)
}

// VerifyLatest compares entity_latest with a recomputation from delta inside one consistent
// snapshot. It returns the total number of mismatching rows and the first `limit` of them.
func (s *Store) VerifyLatest(ctx context.Context, limit int) (total int, first []LatestMismatch, err error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	// one pass over the full outer difference: rows only in delta, only in the cache, or different
	rows, err := tx.QueryContext(ctx, `
	SELECT r.entity_id, r.k, 'missing', CAST(NULL AS SIGNED), r.event_id, NULL, CAST(r.v AS CHAR)
	  FROM (`+latestFromDelta+`) r LEFT JOIN entity_latest l ON l.entity_id = r.entity_id AND l.k = r.k
	 WHERE l.entity_id IS NULL
	UNION ALL
	SELECT l.entity_id, l.k, 'extra', l.event_id, CAST(NULL AS SIGNED), CAST(l.v AS CHAR), NULL
	  FROM entity_latest l LEFT JOIN (`+latestFromDelta+`) r ON l.entity_id = r.entity_id AND l.k = r.k
	 WHERE r.entity_id IS NULL
	UNION ALL
	SELECT l.entity_id, l.k, 'stale', l.event_id, r.event_id, CAST(l.v AS CHAR), CAST(r.v AS CHAR)
	  FROM entity_latest l JOIN (`+latestFromDelta+`) r ON l.entity_id = r.entity_id AND l.k = r.k
	 WHERE l.event_id <> r.event_id OR NOT (CAST(l.v AS CHAR) <=> CAST(r.v AS CHAR))
	ORDER BY 1, 2`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m LatestMismatch
		var ce, de sql.NullInt64
		var cv, dv sql.NullString
		if err := rows.Scan(&m.EntityID, &m.Key, &m.Problem, &ce, &de, &cv, &dv); err != nil {
			return 0, nil, err
		}
		side := func(ev sql.NullInt64, v sql.NullString) string {
			if !ev.Valid {
				return "-"
			}
			if !v.Valid {
				return fmt.Sprintf("ev-%d NULL", ev.Int64)
			}
			return fmt.Sprintf("ev-%d %s", ev.Int64, v.String)
		}
		m.Cache, m.Delta = side(ce, cv), side(de, dv)
		total++
		if len(first) < limit {
			first = append(first, m)
		}
	}
	return total, first, rows.Err()
}
