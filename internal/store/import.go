package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// Import job states (import_batch.state).
const (
	ImportStaged     = "staged"
	ImportApplied    = "applied"
	ImportFailed     = "failed"
	ImportRolledBack = "rolled_back"
)

// ImportJobResult is the ImportJob resource plus the per-row staging errors (an extension).
type ImportJobResult struct {
	api.ImportJob
	RowErrors []RowError `json:"row_errors,omitempty"`
}

// StageInput is the body of createImportJob after decoding.
type StageInput struct {
	Source    string
	Rows      []json.RawMessage
	RequestID string
}

// DiffRow is one line of the diff. RowNo 0 marks a "missing" line (no staged row).
type DiffRow struct {
	RowNo   int
	System  string
	Ref     string
	Hash    string
	Action  string
	Match   int64
	Changes []core.KeyChange
}

// ImportApplyOpts are the options of apply and rollback.
type ImportApplyOpts struct {
	Reason       string
	Note         string
	ValidateOnly bool
	// Resolutions maps row_no to a resolution: conflict rows accept "merge" or "skip",
	// suspect rows accept "link", "create" or "skip".
	Resolutions map[int]string
}

type batchRow struct {
	ID        int64
	Source    string
	State     string
	CreatedBy string
	CreatedAt time.Time
	AppliedAt sql.NullTime
	Summary   sql.NullString
}

// storedSummary is the JSON kept in import_batch.summary.
type storedSummary struct {
	Counts       map[string]int `json:"counts"`
	Applied      map[string]int `json:"applied,omitempty"`
	RolledBackAt string         `json:"rolled_back_at,omitempty"`
	Reverted     int            `json:"reverted,omitempty"`
}

// storedRow is import_row.row_json: a normalized row or the reason it was rejected.
type storedRow struct {
	Row   *core.ImportRow `json:"row,omitempty"`
	Error *RowError       `json:"error,omitempty"`
}

func importName(p Project, id int64) string {
	return "projects/" + p.Name + "/importJobs/" + strconv.FormatInt(id, 10)
}

func importEventKey(batch int64, rowNo int) string { return fmt.Sprintf("imp-%d-%d", batch, rowNo) }

func (b *batchRow) result(p Project, errs []RowError) *ImportJobResult {
	id := strconv.FormatInt(b.ID, 10)
	j := api.ImportJob{
		Name: ptr(importName(p, b.ID)), Uid: &id, Source: ptr(b.Source),
		State: ptr(api.ImportJobState(strings.ToUpper(b.State))), Creator: ptr(accountName(b.CreatedBy)),
		CreateTime: ptr(b.CreatedAt),
	}
	if b.AppliedAt.Valid {
		j.ApplyTime = ptr(b.AppliedAt.Time)
	}
	sum := map[string]int{}
	var st storedSummary
	if b.Summary.Valid && json.Unmarshal([]byte(b.Summary.String), &st) == nil {
		for k, v := range st.Counts {
			sum[strings.ToUpper(k)] = v
		}
		for k, v := range st.Applied {
			sum[strings.ToUpper(k)] = v
		}
	}
	j.Summary = &sum
	return &ImportJobResult{ImportJob: j, RowErrors: errs}
}

func getBatch(ctx context.Context, q execer, projectID, id int64, forUpdate bool) (*batchRow, error) {
	query := `SELECT id, source, state, created_by, created_at, applied_at, summary FROM import_batch WHERE id=? AND project_id=?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var b batchRow
	err := q.QueryRowContext(ctx, query, id, projectID).Scan(&b.ID, &b.Source, &b.State, &b.CreatedBy, &b.CreatedAt, &b.AppliedAt, &b.Summary)
	if isNoRows(err) {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "import job not found")
	}
	return &b, err
}

// ---------------------------------------------------------------- loading current data

// loadImportExisting folds every workflow of the project (soft-deleted ones included) from the
// delta history in one pass.
func loadImportExisting(ctx context.Context, q execer, projectID int64) ([]*core.ImportExisting, error) {
	rows, err := q.QueryContext(ctx, `SELECT d.entity_id, d.k, d.new_v, ev.channel
		FROM delta d JOIN entity e ON e.id = d.entity_id
		JOIN event ev ON ev.id = d.event_id
		WHERE e.project_id = ? AND e.type = 'workflow'
		ORDER BY d.event_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*core.ImportExisting{}
	var order []int64
	for rows.Next() {
		var id int64
		var k, channel string
		var nv sql.NullString
		if err := rows.Scan(&id, &k, &nv, &channel); err != nil {
			return nil, err
		}
		e := byID[id]
		if e == nil {
			e = &core.ImportExisting{ID: id, Vals: map[string]json.RawMessage{}, Touched: map[string]bool{}, Platform: map[string]bool{}}
			byID[id] = e
			order = append(order, id)
		}
		e.Touched[k] = true
		e.Platform[k] = channel == core.ChannelCLI || channel == core.ChannelAPI
		if nv.Valid && nv.String != "" && nv.String != "null" {
			e.Vals[k] = json.RawMessage(nv.String)
		} else {
			delete(e.Vals, k)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*core.ImportExisting, 0, len(order))
	for _, id := range order {
		e := byID[id]
		e.Deleted = core.Bool(e.Vals["deleted"])
		out = append(out, e)
	}
	return out, nil
}

// loadImportBase returns, per source reference, the normalized keys of the last applied import row.
func loadImportBase(ctx context.Context, q execer, projectID, exceptBatch int64, refs []string) (map[string]map[string]json.RawMessage, error) {
	out := map[string]map[string]json.RawMessage{}
	for i := 0; i < len(refs); i += 500 {
		end := min(i+500, len(refs))
		args := []any{projectID, exceptBatch}
		for _, r := range refs[i:end] {
			args = append(args, r)
		}
		rows, err := q.QueryContext(ctx, `SELECT r.row_json FROM import_row r JOIN import_batch b ON b.id = r.batch_id
			WHERE b.project_id = ? AND b.state = 'applied' AND b.id <> ? AND r.action IN ('new','changed','unchanged')
			AND r.source_ref IN (`+placeholders(end-i)+`) ORDER BY b.id, r.row_no`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var js string
			if err := rows.Scan(&js); err != nil {
				rows.Close()
				return nil, err
			}
			var sr storedRow
			if json.Unmarshal([]byte(js), &sr) == nil && sr.Row != nil {
				out[sr.Row.System+"\x00"+sr.Row.Ref] = sr.Row.Keys
			}
		}
		rows.Close()
	}
	return out, nil
}

// ---------------------------------------------------------------- planning

type planRow struct {
	Row *core.ImportRow
	Dec core.Decision
}

type importPlan struct {
	Rows    []planRow // valid rows in row order
	Invalid []RowError
	Missing []*core.ImportExisting
	Counts  map[string]int
}

func countsOf(pl *importPlan) map[string]int {
	c := map[string]int{}
	for _, r := range pl.Rows {
		c[r.Dec.Action]++
	}
	c[core.ActionMissing] = len(pl.Missing)
	c["invalid"] = len(pl.Invalid)
	return c
}

// buildPlan classifies parsed rows against the current data of the project.
func buildPlan(ctx context.Context, q execer, projectID, batchID int64, source string, rows []*core.ImportRow, invalid []RowError) (*importPlan, error) {
	existing, err := loadImportExisting(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	m := core.NewImportMatcher(existing)
	var refs []string
	for _, r := range rows {
		if r.Ref != "" {
			refs = append(refs, r.Ref)
		}
	}
	base, err := loadImportBase(ctx, q, projectID, batchID, refs)
	if err != nil {
		return nil, err
	}
	pl := &importPlan{Invalid: invalid}
	matched := map[int64]bool{}
	for _, r := range rows {
		dec := core.Classify(*r, m, base[r.System+"\x00"+r.Ref])
		if dec.Match != 0 {
			matched[dec.Match] = true
		}
		pl.Rows = append(pl.Rows, planRow{Row: r, Dec: dec})
	}
	pl.Missing = m.Missing(source, matched)
	pl.Counts = countsOf(pl)
	return pl, nil
}

// loadBatchRows reads the staged rows of a batch.
func loadBatchRows(ctx context.Context, q execer, batchID int64) (rows []*core.ImportRow, invalid []RowError, err error) {
	rs, err := q.QueryContext(ctx, `SELECT row_no, row_json FROM import_row WHERE batch_id=? ORDER BY row_no`, batchID)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var no int
		var js string
		if err := rs.Scan(&no, &js); err != nil {
			return nil, nil, err
		}
		var sr storedRow
		if err := json.Unmarshal([]byte(js), &sr); err != nil {
			return nil, nil, err
		}
		switch {
		case sr.Row != nil:
			sr.Row.No = no
			rows = append(rows, sr.Row)
		case sr.Error != nil:
			sr.Error.RowNo = no
			invalid = append(invalid, *sr.Error)
		}
	}
	return rows, invalid, rs.Err()
}

func planBatch(ctx context.Context, q execer, projectID int64, b *batchRow) (*importPlan, error) {
	rows, invalid, err := loadBatchRows(ctx, q, b.ID)
	if err != nil {
		return nil, err
	}
	return buildPlan(ctx, q, projectID, b.ID, b.Source, rows, invalid)
}

// ---------------------------------------------------------------- stage

// StageImport validates the rows, classifies them and stores the batch. Nothing in the workflow
// data changes. Rows that fail validation are reported individually and kept out of the diff.
func (s *Store) StageImport(ctx context.Context, p Project, actor core.Actor, in StageInput) (*ImportJobResult, error) {
	source := strings.TrimSpace(in.Source)
	if source == "" || len(source) > 64 {
		return nil, core.NewError(core.StatusInvalidArgument, "INVALID_SOURCE", "source is required and must be at most 64 characters")
	}
	if len(in.Rows) == 0 {
		return nil, core.NewError(core.StatusInvalidArgument, "NO_ROWS", "rows must not be empty")
	}
	if in.RequestID != "" {
		var id int64
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM import_batch WHERE project_id=? AND request_id=?`, p.ID, in.RequestID).Scan(&id)
		if err == nil {
			return s.GetImport(ctx, p, id)
		}
		if !isNoRows(err) {
			return nil, internalErr(err)
		}
	}
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	var valid []*core.ImportRow
	var invalid []RowError
	rawOf := map[int]json.RawMessage{}
	seenRef, seenCode := map[string]int{}, map[int64]int{}
	for i, raw := range in.Rows {
		no := i + 1
		rawOf[no] = raw
		r, re := parseImportRow(no, raw, source, cfg, p)
		if re == nil && r.Ref != "" {
			k := r.System + "\x00" + r.Ref
			if first, dup := seenRef[k]; dup {
				re = rowErr(no, "DUPLICATE_SOURCE_REFERENCE", "source %s/%s already appears in row %d", r.System, r.Ref, first)
			} else {
				seenRef[k] = no
			}
		}
		if re == nil && r.Code != 0 {
			if first, dup := seenCode[r.Code]; dup {
				re = rowErr(no, "DUPLICATE_NAME", "workflow %s already appears in row %d", p.workflowName(r.Code), first)
			} else {
				seenCode[r.Code] = no
			}
		}
		if re != nil {
			invalid = append(invalid, *re)
			continue
		}
		valid = append(valid, r)
	}

	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	pl, err := buildPlan(ctx, tx, p.ID, 0, source, valid, invalid)
	if err != nil {
		return nil, internalErr(err)
	}
	sumJSON, _ := json.Marshal(storedSummary{Counts: pl.Counts})
	var reqID sql.NullString
	if in.RequestID != "" {
		reqID = sql.NullString{String: in.RequestID, Valid: true}
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO import_batch(project_id, source, state, created_by, created_at, summary, request_id) VALUES(?,?,?,?,?,?,?)`,
		p.ID, source, ImportStaged, actor.Account, now, string(sumJSON), reqID)
	if err != nil {
		if isDup(err) && in.RequestID != "" {
			var id int64
			if e := s.DB.QueryRowContext(ctx, `SELECT id FROM import_batch WHERE project_id=? AND request_id=?`, p.ID, in.RequestID).Scan(&id); e == nil {
				return s.GetImport(ctx, p, id)
			}
		}
		return nil, internalErr(err)
	}
	batchID, _ := res.LastInsertId()

	type line struct {
		no     int
		ref    sql.NullString
		hash   string
		js     string
		match  sql.NullInt64
		action sql.NullString
	}
	var lines []line
	for _, r := range pl.Rows {
		js, _ := json.Marshal(storedRow{Row: r.Row})
		l := line{no: r.Row.No, hash: r.Row.Hash, js: string(js), action: sql.NullString{String: r.Dec.Action, Valid: true}}
		if r.Row.Ref != "" {
			l.ref = sql.NullString{String: r.Row.Ref, Valid: true}
		}
		if r.Dec.Match != 0 {
			l.match = sql.NullInt64{Int64: r.Dec.Match, Valid: true}
		}
		lines = append(lines, l)
	}
	for _, e := range pl.Invalid {
		e := e
		js, _ := json.Marshal(storedRow{Error: &e})
		lines = append(lines, line{no: e.RowNo, hash: hashKeys(map[string]json.RawMessage{"raw": rawOf[e.RowNo]}), js: string(js)})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].no < lines[j].no })
	for i := 0; i < len(lines); i += 300 {
		chunk := lines[i:min(i+300, len(lines))]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO import_row(batch_id, row_no, source_ref, row_hash, row_json, match_entity_id, action) VALUES `)
		var args []any
		for j, l := range chunk {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?,?,?)")
			args = append(args, batchID, l.no, l.ref, l.hash, l.js, l.match, l.action)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return nil, internalErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, internalErr(err)
	}
	b := &batchRow{ID: batchID, Source: source, State: ImportStaged, CreatedBy: actor.Account, CreatedAt: now, Summary: sql.NullString{String: string(sumJSON), Valid: true}}
	return b.result(p, pl.Invalid), nil
}

// ---------------------------------------------------------------- get / diff

// GetImport returns the job; staged jobs carry live counts against the current data.
func (s *Store) GetImport(ctx context.Context, p Project, id int64) (*ImportJobResult, error) {
	b, err := getBatch(ctx, s.DB, p.ID, id, false)
	if err != nil {
		return nil, internalErr(err)
	}
	rows, invalid, err := loadBatchRows(ctx, s.DB, b.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	if b.State == ImportStaged {
		pl, err := buildPlan(ctx, s.DB, p.ID, b.ID, b.Source, rows, invalid)
		if err != nil {
			return nil, internalErr(err)
		}
		sj, _ := json.Marshal(storedSummary{Counts: pl.Counts})
		b.Summary = sql.NullString{String: string(sj), Valid: true}
	}
	if len(invalid) > 1000 {
		invalid = invalid[:1000]
	}
	return b.result(p, invalid), nil
}

// DiffImport lists classified rows (and missing workflows) of a job. actions filters by action
// (empty = all). Applied and rolled back jobs report the stored actions without key changes.
func (s *Store) DiffImport(ctx context.Context, p Project, id int64, actions map[string]bool, offset, size int) ([]DiffRow, int, error) {
	b, err := getBatch(ctx, s.DB, p.ID, id, false)
	if err != nil {
		return nil, 0, internalErr(err)
	}
	rows, invalid, err := loadBatchRows(ctx, s.DB, b.ID)
	if err != nil {
		return nil, 0, internalErr(err)
	}
	var all []DiffRow
	if b.State == ImportStaged {
		pl, err := buildPlan(ctx, s.DB, p.ID, b.ID, b.Source, rows, invalid)
		if err != nil {
			return nil, 0, internalErr(err)
		}
		for _, r := range pl.Rows {
			all = append(all, DiffRow{RowNo: r.Row.No, System: r.Row.System, Ref: r.Row.Ref, Hash: r.Row.Hash,
				Action: r.Dec.Action, Match: r.Dec.Match, Changes: r.Dec.Changes})
		}
		for _, e := range pl.Missing {
			sys, _ := core.Str(e.Vals["source.system"])
			ref, _ := core.Str(e.Vals["source.ref"])
			hash, _ := core.Str(e.Vals["source.hash"])
			all = append(all, DiffRow{System: sys, Ref: ref, Hash: hash, Action: core.ActionMissing, Match: e.ID})
		}
	} else {
		act := map[int]sql.NullString{}
		match := map[int]sql.NullInt64{}
		rs, err := s.DB.QueryContext(ctx, `SELECT row_no, action, match_entity_id FROM import_row WHERE batch_id=?`, b.ID)
		if err != nil {
			return nil, 0, internalErr(err)
		}
		for rs.Next() {
			var no int
			var a sql.NullString
			var m sql.NullInt64
			if err := rs.Scan(&no, &a, &m); err != nil {
				rs.Close()
				return nil, 0, internalErr(err)
			}
			act[no], match[no] = a, m
		}
		rs.Close()
		for _, r := range rows {
			all = append(all, DiffRow{RowNo: r.No, System: r.System, Ref: r.Ref, Hash: r.Hash, Action: act[r.No].String, Match: match[r.No].Int64})
		}
	}
	var out []DiffRow
	for _, r := range all {
		if len(actions) == 0 || actions[r.Action] {
			out = append(out, r)
		}
	}
	if offset > len(out) {
		offset = len(out)
	}
	end := min(offset+size, len(out))
	next := 0
	if end < len(out) {
		next = end
	}
	return out[offset:end], next, nil
}

// ---------------------------------------------------------------- bulk writing

type kv struct {
	Key string
	Old json.RawMessage
	New json.RawMessage
}

// bulkEvent is one event to insert; EntityID 0 creates a workflow entity first.
type bulkEvent struct {
	EntityID int64
	IdemKey  string
	Note     string
	Corrects int64
	Deltas   []kv
	// out
	CreatedEntity int64
}

// bulkWriter inserts many events inside a caller-owned transaction with prepared statements and
// multi-row delta inserts.
type bulkWriter struct {
	ctx     context.Context
	tx      *sql.Tx
	project int64
	actor   core.Actor
	reason  sql.NullString
	now     time.Time

	entStmt, evStmt *sql.Stmt
	pending         []any
	pendingRows     int
}

func newBulkWriter(ctx context.Context, tx *sql.Tx, project int64, actor core.Actor, reason string, now time.Time) (*bulkWriter, error) {
	w := &bulkWriter{ctx: ctx, tx: tx, project: project, actor: actor, now: now}
	if reason != "" {
		w.reason = sql.NullString{String: reason, Valid: true}
	}
	var err error
	if w.entStmt, err = tx.PrepareContext(ctx, `INSERT INTO entity(project_id, type, create_time) VALUES(?, 'workflow', ?)`); err != nil {
		return nil, err
	}
	if w.evStmt, err = tx.PrepareContext(ctx, `INSERT INTO event(project_id, entity_id, actor, side, channel, reason_key, note, idempotency_key,
		corrects_event_id, event_time, create_time) VALUES(?,?,?,?,?,?,?,?,?,?,?)`); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *bulkWriter) close() {
	if w.entStmt != nil {
		w.entStmt.Close()
	}
	if w.evStmt != nil {
		w.evStmt.Close()
	}
}

func nullJSON(raw json.RawMessage) sql.NullString {
	t := normJSON(raw)
	if len(t) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(t), Valid: true}
}

func (w *bulkWriter) write(ev *bulkEvent) (eventID int64, err error) {
	entityID := ev.EntityID
	if entityID == 0 {
		r, err := w.entStmt.ExecContext(w.ctx, w.project, w.now)
		if err != nil {
			return 0, err
		}
		entityID, _ = r.LastInsertId()
		ev.CreatedEntity = entityID
	}
	var note sql.NullString
	if ev.Note != "" {
		note = sql.NullString{String: ev.Note, Valid: true}
	}
	var corrects sql.NullInt64
	if ev.Corrects != 0 {
		corrects = sql.NullInt64{Int64: ev.Corrects, Valid: true}
	}
	r, err := w.evStmt.ExecContext(w.ctx, w.project, entityID, w.actor.Account, w.actor.Side, w.actor.Channel, w.reason, note,
		sql.NullString{String: ev.IdemKey, Valid: ev.IdemKey != ""}, corrects, w.now, w.now)
	if err != nil {
		return 0, err
	}
	eventID, _ = r.LastInsertId()
	for _, d := range ev.Deltas {
		w.pending = append(w.pending, eventID, entityID, d.Key, nullJSON(d.Old), nullJSON(d.New))
		w.pendingRows++
		if w.pendingRows >= 400 {
			if err := w.flush(); err != nil {
				return 0, err
			}
		}
	}
	return eventID, nil
}

func (w *bulkWriter) flush() error {
	if w.pendingRows == 0 {
		return nil
	}
	q := `INSERT INTO delta(event_id, entity_id, k, old_v, new_v) VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", w.pendingRows), ",")
	_, err := w.tx.ExecContext(w.ctx, q, w.pending...)
	w.pending, w.pendingRows = w.pending[:0], 0
	return err
}

// ---------------------------------------------------------------- apply

func eventNote(batch int64, rowNo int, r *core.ImportRow, user string) string {
	n := fmt.Sprintf("import job %d row %d (%s", batch, rowNo, r.System)
	if r.Ref != "" {
		n += ":" + r.Ref
	}
	n += ")"
	if user != "" {
		n += ": " + user
	}
	return n
}

// ApplyImport merges a staged job in one transaction: new workflows are created, existing ones get
// their source-owned fields updated; platform-owned fields are never overwritten and missing
// workflows are only counted. Conflicts and suspects are skipped unless opts.Resolutions says
// otherwise. A second apply of an applied job returns it unchanged.
func (s *Store) ApplyImport(ctx context.Context, p Project, actor core.Actor, id int64, opts ImportApplyOpts) (*ImportJobResult, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	if opts.Reason != "" {
		if r, ok := cfg.Reasons[opts.Reason]; !ok || !r.Active {
			return nil, core.NewError(core.StatusInvalidArgument, "UNKNOWN_REASON", "reason %q is not defined or not active", opts.Reason)
		}
	}
	actor.Channel = core.ChannelImport
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	b, err := getBatch(ctx, tx, p.ID, id, true)
	if err != nil {
		return nil, internalErr(err)
	}
	switch b.State {
	case ImportApplied:
		return b.result(p, nil), nil // idempotent: nothing to do
	case ImportStaged:
	default:
		return nil, core.NewError(core.StatusFailedPrecondition, "IMPORT_JOB_STATE", "import job is %s and cannot be applied", b.State)
	}
	pl, err := planBatch(ctx, tx, p.ID, b)
	if err != nil {
		return nil, internalErr(err)
	}
	byNo := map[int]*planRow{}
	for i := range pl.Rows {
		byNo[pl.Rows[i].Row.No] = &pl.Rows[i]
	}
	for no, res := range opts.Resolutions {
		r := byNo[no]
		ok := false
		if r != nil {
			switch r.Dec.Action {
			case core.ActionConflict:
				ok = res == "merge" || res == "skip"
			case core.ActionSuspect:
				ok = res == "link" || res == "create" || res == "skip"
			}
		}
		if !ok {
			return nil, core.NewError(core.StatusInvalidArgument, "INVALID_RESOLUTION", "resolution %q does not apply to row %d", res, no)
		}
	}

	now := s.now()
	w, err := newBulkWriter(ctx, tx, p.ID, actor, opts.Reason, now)
	if err != nil {
		return nil, internalErr(err)
	}
	defer w.close()

	type effect struct {
		no     int
		action string
		match  int64
	}
	var effects []effect
	applied := map[string]int{"created": 0, "updated": 0, "skipped": 0}
	for i := range pl.Rows {
		r := &pl.Rows[i]
		row, dec := r.Row, r.Dec
		res := opts.Resolutions[row.No]
		create, update := false, false
		eff := dec.Action
		switch dec.Action {
		case core.ActionNew:
			create = true
		case core.ActionChanged:
			update = true
		case core.ActionConflict:
			if res == "merge" {
				update, eff = true, core.ActionChanged
			}
		case core.ActionSuspect:
			switch res {
			case "link":
				update, eff = true, core.ActionChanged
			case "create":
				create, eff = true, core.ActionNew
			}
		}
		switch {
		case create:
			keys := row.Desired()
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			ev := &bulkEvent{IdemKey: importEventKey(b.ID, row.No), Note: eventNote(b.ID, row.No, row, opts.Note)}
			for _, k := range names {
				ev.Deltas = append(ev.Deltas, kv{Key: k, New: keys[k]})
			}
			if _, err := w.write(ev); err != nil {
				return nil, internalErr(err)
			}
			applied["created"]++
			effects = append(effects, effect{row.No, eff, ev.CreatedEntity})
		case update:
			var planned []PlannedChange
			for _, c := range dec.Changes {
				if c.Kind != core.ChangeConflict {
					planned = append(planned, PlannedChange{Key: c.Key, New: c.New})
				}
			}
			if len(planned) == 0 {
				applied["skipped"]++
				effects = append(effects, effect{row.No, dec.Action, dec.Match})
				continue
			}
			if err := w.flush(); err != nil { // keep write order for the entity lock below
				return nil, internalErr(err)
			}
			rr, err := s.applyTx(ctx, tx, ChangeRequest{
				ProjectID: p.ID, EntityType: "workflow", EntityID: dec.Match, Actor: actor,
				ReasonKey: opts.Reason, Note: eventNote(b.ID, row.No, row, opts.Note), RequestID: importEventKey(b.ID, row.No),
				Plan: func(*Entity, core.Env) ([]PlannedChange, error) { return planned, nil },
			})
			if err != nil {
				return nil, err
			}
			if rr.NoOp {
				applied["skipped"]++
			} else {
				applied["updated"]++
			}
			effects = append(effects, effect{row.No, eff, dec.Match})
		default:
			if dec.Action != core.ActionUnchanged {
				applied["skipped"]++
			}
			effects = append(effects, effect{row.No, dec.Action, dec.Match})
		}
	}
	if err := w.flush(); err != nil {
		return nil, internalErr(err)
	}

	counts := pl.Counts
	sj, _ := json.Marshal(storedSummary{Counts: counts, Applied: applied})
	if opts.ValidateOnly {
		pb := *b
		pb.Summary = sql.NullString{String: string(sj), Valid: true}
		return pb.result(p, pl.Invalid), nil // deferred Rollback discards everything
	}
	up, err := tx.PrepareContext(ctx, `UPDATE import_row SET action=?, match_entity_id=? WHERE batch_id=? AND row_no=?`)
	if err != nil {
		return nil, internalErr(err)
	}
	defer up.Close()
	for _, e := range effects {
		var m sql.NullInt64
		if e.match != 0 {
			m = sql.NullInt64{Int64: e.match, Valid: true}
		}
		if _, err := up.ExecContext(ctx, e.action, m, b.ID, e.no); err != nil {
			return nil, internalErr(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_batch SET state=?, applied_at=?, summary=? WHERE id=?`, ImportApplied, now, string(sj), b.ID); err != nil {
		return nil, internalErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalErr(err)
	}
	b.State, b.AppliedAt, b.Summary = ImportApplied, sql.NullTime{Time: now, Valid: true}, sql.NullString{String: string(sj), Valid: true}
	return b.result(p, pl.Invalid), nil
}

// ---------------------------------------------------------------- rollback

type batchEvent struct {
	ID, Entity int64
	Created    bool
	Deltas     []kv
}

// RollbackImport undoes an applied job with correcting events (corrects_event_id points at the
// original). It refuses, listing the workflows, when any event outside the job touched an entity
// after the job did. Created workflows are reverted key by key and marked deleted.
func (s *Store) RollbackImport(ctx context.Context, p Project, actor core.Actor, id int64, opts ImportApplyOpts) (*ImportJobResult, error) {
	actor.Channel = core.ChannelImport
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	b, err := getBatch(ctx, tx, p.ID, id, true)
	if err != nil {
		return nil, internalErr(err)
	}
	switch b.State {
	case ImportRolledBack:
		return b.result(p, nil), nil
	case ImportApplied:
	default:
		return nil, core.NewError(core.StatusFailedPrecondition, "IMPORT_JOB_STATE", "import job is %s and cannot be rolled back", b.State)
	}

	// the events written by this job
	rs, err := tx.QueryContext(ctx, `SELECT id, entity_id FROM event WHERE project_id=? AND idempotency_key LIKE ? ORDER BY id`,
		p.ID, fmt.Sprintf("imp-%d-%%", b.ID))
	if err != nil {
		return nil, internalErr(err)
	}
	var evs []*batchEvent
	inBatch := map[int64]bool{}
	var entities []int64
	for rs.Next() {
		e := &batchEvent{}
		if err := rs.Scan(&e.ID, &e.Entity); err != nil {
			rs.Close()
			return nil, internalErr(err)
		}
		evs = append(evs, e)
		inBatch[e.ID] = true
		entities = append(entities, e.Entity)
	}
	rs.Close()

	// lock the touched entities, then look for later events from outside the job
	lastOwn, first := map[int64]int64{}, map[int64]int64{}
	for _, e := range evs {
		lastOwn[e.Entity] = max(lastOwn[e.Entity], e.ID)
	}
	var blocked []int64
	for i := 0; i < len(entities); i += 1000 {
		chunk := entities[i:min(i+1000, len(entities))]
		ph := placeholders(len(chunk))
		if _, err := tx.ExecContext(ctx, `SELECT id FROM entity WHERE id IN (`+ph+`) ORDER BY id FOR UPDATE`, idArgs(chunk)...); err != nil {
			return nil, internalErr(err)
		}
		er, err := tx.QueryContext(ctx, `SELECT entity_id, id FROM event WHERE entity_id IN (`+ph+`) ORDER BY id`, idArgs(chunk)...)
		if err != nil {
			return nil, internalErr(err)
		}
		seen := map[int64]bool{}
		for er.Next() {
			var ent, evID int64
			if err := er.Scan(&ent, &evID); err != nil {
				er.Close()
				return nil, internalErr(err)
			}
			if _, ok := first[ent]; !ok {
				first[ent] = evID
			}
			if evID > lastOwn[ent] && !inBatch[evID] && !seen[ent] {
				seen[ent] = true
				blocked = append(blocked, ent)
			}
		}
		er.Close()
	}
	if len(blocked) > 0 {
		sort.Slice(blocked, func(i, j int) bool { return blocked[i] < blocked[j] })
		var names []string
		for i, ent := range blocked {
			if i == 50 {
				break
			}
			names = append(names, p.workflowName(ent))
		}
		return nil, core.NewError(core.StatusFailedPrecondition, "ROLLBACK_BLOCKED",
			"%d workflow(s) were changed after the import by someone else; roll back is refused: %s", len(blocked), strings.Join(names, ", ")).
			With("count", strconv.Itoa(len(blocked))).With("workflows", strings.Join(names, ","))
	}

	// the deltas to revert
	byID := map[int64]*batchEvent{}
	for _, e := range evs {
		e.Created = first[e.Entity] == e.ID
		byID[e.ID] = e
	}
	for i := 0; i < len(evs); i += 500 {
		chunk := evs[i:min(i+500, len(evs))]
		var ids []int64
		for _, e := range chunk {
			ids = append(ids, e.ID)
		}
		dr, err := tx.QueryContext(ctx, `SELECT event_id, k, old_v, new_v FROM delta WHERE event_id IN (`+placeholders(len(ids))+`) ORDER BY event_id, k`, idArgs(ids)...)
		if err != nil {
			return nil, internalErr(err)
		}
		for dr.Next() {
			var evID int64
			var k string
			var ov, nv sql.NullString
			if err := dr.Scan(&evID, &k, &ov, &nv); err != nil {
				dr.Close()
				return nil, internalErr(err)
			}
			d := kv{Key: k}
			if ov.Valid {
				d.Old = json.RawMessage(ov.String)
			}
			if nv.Valid {
				d.New = json.RawMessage(nv.String)
			}
			byID[evID].Deltas = append(byID[evID].Deltas, d)
		}
		dr.Close()
	}

	now := s.now()
	w, err := newBulkWriter(ctx, tx, p.ID, actor, opts.Reason, now)
	if err != nil {
		return nil, internalErr(err)
	}
	defer w.close()
	for _, e := range evs {
		ev := &bulkEvent{EntityID: e.Entity, Corrects: e.ID, IdemKey: fmt.Sprintf("imprb-%d-%d", b.ID, e.ID),
			Note: strings.TrimSpace(fmt.Sprintf("rollback of import job %d (event ev-%d) %s", b.ID, e.ID, opts.Note))}
		for _, d := range e.Deltas { // swap old and new
			ev.Deltas = append(ev.Deltas, kv{Key: d.Key, Old: d.New, New: d.Old})
		}
		if e.Created {
			ev.Deltas = append(ev.Deltas, kv{Key: "deleted", New: core.JSON(true)})
		}
		if _, err := w.write(ev); err != nil {
			return nil, internalErr(err)
		}
	}
	if err := w.flush(); err != nil {
		return nil, internalErr(err)
	}

	var st storedSummary
	if b.Summary.Valid {
		_ = json.Unmarshal([]byte(b.Summary.String), &st)
	}
	st.RolledBackAt, st.Reverted = now.Format(time.RFC3339Nano), len(evs)
	sj, _ := json.Marshal(st)
	b.Summary = sql.NullString{String: string(sj), Valid: true}
	if opts.ValidateOnly {
		return b.result(p, nil), nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_batch SET state=?, summary=? WHERE id=?`, ImportRolledBack, string(sj), b.ID); err != nil {
		return nil, internalErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalErr(err)
	}
	b.State = ImportRolledBack
	return b.result(p, nil), nil
}
