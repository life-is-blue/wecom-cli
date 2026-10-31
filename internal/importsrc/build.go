package importsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

var errEOF = io.EOF

// DefaultMaxRows bounds one staging request (the API stages a job in one call).
const DefaultMaxRows = 100000

var defaultStageValues = map[string]string{
	"pass": "PASSED", "passed": "PASSED", "done": "PASSED", "yes": "PASSED", "true": "PASSED", "1": "PASSED",
	"open": "OPEN", "no": "OPEN", "false": "OPEN", "0": "OPEN",
	"na": "NOT_APPLICABLE", "n/a": "NOT_APPLICABLE", "not_applicable": "NOT_APPLICABLE",
}

func (m *Mapping) value(ref FieldRef, r Row) (string, bool) {
	if ref.isConst {
		return ref.Const, true
	}
	s, ok := text(r[ref.Column])
	s = strings.TrimSpace(s)
	if !ok || s == "" {
		if ref.Default != "" {
			return ref.Default, true
		}
		return "", false
	}
	return s, true
}

// Map converts one source row to an import row for the given source system.
func (m *Mapping) Map(r Row, system string) (api.ImportRow, error) {
	var parts []string
	for _, c := range m.Reference {
		s, ok := text(r[c])
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			return api.ImportRow{}, fmt.Errorf("reference column %q is empty", c)
		}
		parts = append(parts, s)
	}
	ref := strings.Join(parts, ":")
	hash := Hash(r, m.HashExclude)

	name, ok := m.value(m.Fields["name"], r)
	if !ok {
		return api.ImportRow{}, fmt.Errorf("row %s: name is empty", ref)
	}
	wf := api.Workflow{DisplayName: &name}

	labels := map[string]string{}
	for _, k := range standardLabels {
		if f, ok := m.Fields[k]; ok {
			if v, ok := m.value(f, r); ok {
				labels[k] = strings.ToLower(v)
			}
		}
	}
	for k, f := range m.Labels {
		if v, ok := m.value(f, r); ok {
			labels[k] = strings.ToLower(v)
		}
	}
	if len(labels) > 0 {
		l := api.Labels(labels)
		wf.Labels = &l
	}
	ann := map[string]string{}
	for k, f := range m.Annotations {
		if v, ok := m.value(f, r); ok {
			ann[k] = v
		}
	}
	if len(ann) > 0 {
		a := api.Annotations(ann)
		wf.Annotations = &a
	}
	asg := map[string]string{}
	for k, f := range m.Assignees {
		if v, ok := m.value(f, r); ok {
			if !strings.HasPrefix(v, "accounts/") {
				v = "accounts/" + strings.ToLower(v)
			}
			asg[k] = v
		}
	}
	if len(asg) > 0 {
		a := api.Assignees(asg)
		wf.Assignees = &a
	}
	if len(m.Stages) > 0 {
		keys := make([]string, 0, len(m.Stages))
		for k := range m.Stages {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sts []api.StageStatus
		for _, k := range keys {
			v, ok := m.value(m.Stages[k], r)
			if !ok {
				continue
			}
			st, err := m.stageState(v)
			if err != nil {
				return api.ImportRow{}, fmt.Errorf("row %s: stage %s: %w", ref, k, err)
			}
			sts = append(sts, api.StageStatus{Stage: k, State: api.StageState(st)})
		}
		if len(sts) > 0 {
			wf.Stages = &sts
		}
	}
	out := api.ImportRow{
		Source: api.Source{System: &system, Reference: &ref, RowHash: &hash},
		Data:   wf,
	}
	if m.Tasks != nil && m.Tasks.From != "" {
		ts, err := m.nestedTasks(r)
		if err != nil {
			return api.ImportRow{}, fmt.Errorf("row %s: %w", ref, err)
		}
		out.Tasks = ts
	}
	return out, nil
}

func (m *Mapping) stageState(v string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(v))
	for from, to := range m.StageValues {
		if strings.ToLower(from) == k {
			return strings.ToUpper(to), nil
		}
	}
	if to, ok := defaultStageValues[k]; ok {
		return to, nil
	}
	return "", fmt.Errorf("unknown stage value %q (add it to stage_values)", v)
}

// nestedTasks maps the JSON array in column/key tasks.from (an array, or a string holding one).
func (m *Mapping) nestedTasks(r Row) (*[]api.ImportTask, error) {
	raw := r[m.Tasks.From]
	if s, ok := raw.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("tasks column %q is not JSON: %v", m.Tasks.From, err)
		}
		raw = v
	}
	if raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tasks column %q must hold a JSON array", m.Tasks.From)
	}
	out := make([]api.ImportTask, 0, len(arr))
	for i, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tasks[%d] is not an object", i)
		}
		t, err := m.mapTask(Row(obj))
		if err != nil {
			return nil, fmt.Errorf("tasks[%d]: %w", i, err)
		}
		out = append(out, t)
	}
	return &out, nil
}

// mapTask maps one task row/element: reference, name, labels, annotations, assignees, hash.
func (m *Mapping) mapTask(r Row) (api.ImportTask, error) {
	t := m.Tasks
	ref, ok := text(r[t.Reference])
	if ref = strings.TrimSpace(ref); !ok || ref == "" {
		return api.ImportTask{}, fmt.Errorf("reference %q is empty", t.Reference)
	}
	nameKey := t.Name
	if nameKey == "" {
		nameKey = "name"
	}
	name, ok := text(r[nameKey])
	if name = strings.TrimSpace(name); !ok || name == "" {
		return api.ImportTask{}, fmt.Errorf("task %s: %q is empty", ref, nameKey)
	}
	hash := Hash(r, t.HashExclude)
	d := api.Task{DisplayName: name}
	collect := func(fm map[string]FieldRef, norm func(string) string) map[string]string {
		out := map[string]string{}
		for k, f := range fm {
			if v, ok := m.value(f, r); ok {
				out[k] = norm(v)
			}
		}
		return out
	}
	if l := collect(t.Labels, strings.ToLower); len(l) > 0 {
		v := api.Labels(l)
		d.Labels = &v
	}
	if a := collect(t.Annotations, func(s string) string { return s }); len(a) > 0 {
		v := api.Annotations(a)
		d.Annotations = &v
	}
	if a := collect(t.Assignees, accountOf); len(a) > 0 {
		v := api.Assignees(a)
		d.Assignees = &v
	}
	return api.ImportTask{Source: api.Source{Reference: &ref, RowHash: &hash}, Data: d}, nil
}

func accountOf(v string) string {
	if strings.HasPrefix(v, "accounts/") {
		return v
	}
	return "accounts/" + strings.ToLower(v)
}

// taskScanner is implemented by sources that can run the tasks.table / tasks.query.
type taskScanner interface {
	ScanTasks(ctx context.Context, t *TasksSpec, fn func(Row) error) error
}

// queryTasks reads the task rows of tasks.table/query and groups them by parent reference.
func (m *Mapping) queryTasks(ctx context.Context, src Source) (map[string][]api.ImportTask, error) {
	ts, ok := src.(taskScanner)
	if !ok {
		return nil, errors.New("tasks.table / tasks.query need a mysql:// source (use tasks.from for files)")
	}
	byParent := map[string][]api.ImportTask{}
	n := 0
	var errs []error
	err := ts.ScanTasks(ctx, m.Tasks, func(r Row) error {
		n++
		parent, ok := text(r[m.Tasks.ParentKey])
		if parent = strings.TrimSpace(parent); !ok || parent == "" {
			errs = append(errs, fmt.Errorf("task row %d: parent_key %q is empty", n, m.Tasks.ParentKey))
		} else if t, err := m.mapTask(r); err != nil {
			errs = append(errs, fmt.Errorf("task row %d: %w", n, err))
		} else {
			byParent[parent] = append(byParent[parent], t)
		}
		if len(errs) >= 10 {
			return errors.Join(errs...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return byParent, errors.Join(errs...)
}

// RowError is a mapping failure for one source row.
type RowError struct {
	Row int
	Err error
}

func (e RowError) Error() string { return fmt.Sprintf("source row %d: %v", e.Row, e.Err) }

// Build scans src and maps every row. The result is sorted by reference so the
// payload is deterministic. It fails on the first few row errors, duplicate
// references, or more than maxRows rows.
func Build(ctx context.Context, src Source, m *Mapping, system string, maxRows int) ([]api.ImportRow, error) {
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	var rows []api.ImportRow
	var errs []error
	seen := map[string]int{}
	n := 0
	var tasksOf map[string][]api.ImportTask
	if m.Tasks != nil && m.Tasks.From == "" {
		var err error
		if tasksOf, err = m.queryTasks(ctx, src); err != nil {
			return nil, err
		}
	}
	seenTask := map[string]string{}
	err := src.Scan(ctx, func(r Row) error {
		n++
		if n > maxRows {
			return fmt.Errorf("more than %d rows; stage in smaller slices (use a narrower source.query)", maxRows)
		}
		ir, err := m.Map(r, system)
		if err != nil {
			errs = append(errs, RowError{n, err})
			if len(errs) >= 10 {
				return errors.Join(errs...)
			}
			return nil
		}
		ref := *ir.Source.Reference
		if first, dup := seen[ref]; dup {
			errs = append(errs, RowError{n, fmt.Errorf("duplicate reference %q (first seen in row %d)", ref, first)})
			return nil
		}
		seen[ref] = n
		if ts := tasksOf[ref]; len(ts) > 0 {
			ir.Tasks = &ts
			delete(tasksOf, ref)
		}
		if ir.Tasks != nil {
			sort.Slice(*ir.Tasks, func(i, j int) bool { return *(*ir.Tasks)[i].Source.Reference < *(*ir.Tasks)[j].Source.Reference })
			for _, t := range *ir.Tasks {
				tr := *t.Source.Reference
				if first, dup := seenTask[tr]; dup {
					return fmt.Errorf("duplicate task reference %q (rows %s and %s)", tr, first, ref)
				}
				seenTask[tr] = ref
			}
		}
		rows = append(rows, ir)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(rows) == 0 {
		return nil, errors.New("source produced no rows")
	}
	if len(tasksOf) > 0 {
		var parents []string
		for p := range tasksOf {
			parents = append(parents, p)
		}
		sort.Strings(parents)
		if len(parents) > 5 {
			parents = parents[:5]
		}
		return nil, fmt.Errorf("tasks reference %d unknown parent(s) via %q, e.g. %s", len(tasksOf), m.Tasks.ParentKey, strings.Join(parents, ", "))
	}
	sort.Slice(rows, func(i, j int) bool { return *rows[i].Source.Reference < *rows[j].Source.Reference })
	return rows, nil
}
