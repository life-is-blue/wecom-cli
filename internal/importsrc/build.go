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
	if m.Tasks != nil {
		ts, err := m.tasks(r)
		if err != nil {
			return api.ImportRow{}, fmt.Errorf("row %s: %w", ref, err)
		}
		if ts != "" {
			ann["tasks"] = ts
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
	return api.ImportRow{
		Source: api.Source{System: &system, Reference: &ref, RowHash: &hash},
		Data:   wf,
	}, nil
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

// tasks renders the nested task list as compact JSON: [{"display_name","labels"}].
func (m *Mapping) tasks(r Row) (string, error) {
	raw := r[m.Tasks.From]
	if s, ok := raw.(string); ok {
		if strings.TrimSpace(s) == "" {
			return "", nil
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return "", fmt.Errorf("tasks column %q is not JSON: %v", m.Tasks.From, err)
		}
		raw = v
	}
	if raw == nil {
		return "", nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("tasks column %q must hold a JSON array", m.Tasks.From)
	}
	nameKey := m.Tasks.Name
	if nameKey == "" {
		nameKey = "name"
	}
	out := make([]map[string]any, 0, len(arr))
	for i, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			return "", fmt.Errorf("tasks[%d] is not an object", i)
		}
		n, _ := text(obj[nameKey])
		if strings.TrimSpace(n) == "" {
			return "", fmt.Errorf("tasks[%d] has no %q", i, nameKey)
		}
		t := map[string]any{"display_name": strings.TrimSpace(n)}
		lb := map[string]string{}
		for lk, ek := range m.Tasks.Labels {
			if v, ok := text(obj[ek]); ok && strings.TrimSpace(v) != "" {
				lb[lk] = strings.ToLower(strings.TrimSpace(v))
			}
		}
		if len(lb) > 0 {
			t["labels"] = lb
		}
		out = append(out, t)
	}
	b, _ := json.Marshal(out)
	return string(b), nil
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
	sort.Slice(rows, func(i, j int) bool { return *rows[i].Source.Reference < *rows[j].Source.Reference })
	return rows, nil
}
