package store

import (
	"context"
	"database/sql"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// LoadConfig reads stage_def, reason and rule (global tables in M1).
func (s *Store) LoadConfig(ctx context.Context) (core.Config, error) {
	return loadConfig(ctx, s.DB)
}

func loadConfig(ctx context.Context, q execer) (core.Config, error) {
	cfg := core.Config{Reasons: map[string]core.Reason{}}
	rows, err := q.QueryContext(ctx, `SELECT stage_key, seq, name_zh, name_en FROM stage_def ORDER BY seq`)
	if err != nil {
		return cfg, err
	}
	for rows.Next() {
		var st core.Stage
		if err := rows.Scan(&st.Key, &st.Seq, &st.NameZh, &st.NameEn); err != nil {
			rows.Close()
			return cfg, err
		}
		cfg.Stages = append(cfg.Stages, st)
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, `SELECT reason_key, kind, owner_side, note_required, active FROM reason`)
	if err != nil {
		return cfg, err
	}
	for rows.Next() {
		var r core.Reason
		if err := rows.Scan(&r.Key, &r.Kind, &r.OwnerSide, &r.NoteRequired, &r.Active); err != nil {
			rows.Close()
			return cfg, err
		}
		cfg.Reasons[r.Key] = r
	}
	rows.Close()

	rows, err = q.QueryContext(ctx, `SELECT k_pattern, to_value, side, reason_kind FROM rule ORDER BY id`)
	if err != nil {
		return cfg, err
	}
	defer rows.Close()
	for rows.Next() {
		var r core.Rule
		var tv, rk sql.NullString
		if err := rows.Scan(&r.KPattern, &tv, &r.Side, &rk); err != nil {
			return cfg, err
		}
		if tv.Valid {
			r.ToValue = &tv.String
		}
		if rk.Valid {
			r.ReasonKind = &rk.String
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	return cfg, rows.Err()
}
