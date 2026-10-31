// Package importsrc reads legacy rows (NDJSON, CSV, MySQL), maps them to the
// standard import row (api.ImportRow) and computes a deterministic row hash.
package importsrc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FieldRef points at a source column, a constant, or a column with a default.
// In YAML it is a bare column name or {column, const, default}.
type FieldRef struct {
	Column  string `yaml:"column"`
	Const   string `yaml:"const"`
	Default string `yaml:"default"`
	isConst bool
}

func (f *FieldRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		f.Column = n.Value
		return nil
	case yaml.MappingNode:
		type plain FieldRef
		var p plain
		if err := n.Decode(&p); err != nil {
			return err
		}
		*f = FieldRef(p)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "const" {
				f.isConst = true
			}
		}
		if f.Column == "" && !f.isConst {
			return fmt.Errorf("line %d: field needs column or const", n.Line)
		}
		return nil
	}
	return fmt.Errorf("line %d: field must be a column name or a {column|const, default} mapping", n.Line)
}

// SourceSpec says what to read from a mysql:// source.
type SourceSpec struct {
	Table   string `yaml:"table"`
	Query   string `yaml:"query"`
	OrderBy string `yaml:"order_by"`
}

// TasksSpec maps the tasks of a workflow row. Exactly one origin: From (a JSON array inside the
// row) or Table/Query (a second mysql:// query joined to the rows by ParentKey).
type TasksSpec struct {
	// From is the column (or NDJSON key) holding a JSON array of task objects.
	From string `yaml:"from"`
	// Table or Query selects task rows from the mysql:// source; ParentKey is the column holding the
	// reference of the owning row (equal to the row's reference value).
	Table     string `yaml:"table"`
	Query     string `yaml:"query"`
	ParentKey string `yaml:"parent_key"`
	// Reference is the task's column/element key holding its stable id (required, unique in the source).
	Reference string `yaml:"reference"`
	// Name is the column/element key of the task display name (default "name").
	Name        string              `yaml:"name"`
	Labels      map[string]FieldRef `yaml:"labels"`
	Annotations map[string]FieldRef `yaml:"annotations"`
	Assignees   map[string]FieldRef `yaml:"assignees"`
	HashExclude []string            `yaml:"hash_exclude"`
}

// Mapping is the parsed mapping.yaml.
type Mapping struct {
	Version      int                 `yaml:"version"`
	SourceSystem string              `yaml:"source_system"`
	Source       SourceSpec          `yaml:"source"`
	Reference    []string            `yaml:"reference"`
	HashExclude  []string            `yaml:"hash_exclude"`
	Fields       map[string]FieldRef `yaml:"fields"`
	Labels       map[string]FieldRef `yaml:"labels"`
	Annotations  map[string]FieldRef `yaml:"annotations"`
	Assignees    map[string]FieldRef `yaml:"assignees"`
	Stages       map[string]FieldRef `yaml:"stages"`
	StageValues  map[string]string   `yaml:"stage_values"`
	Tasks        *TasksSpec          `yaml:"tasks"`
}

// standardLabels are the row fields that become labels of the same name.
var standardLabels = []string{"domain", "area", "application", "priority", "category"}

// LoadMapping reads and validates a mapping file.
func LoadMapping(path string) (*Mapping, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseMapping(b)
}

// ParseMapping parses and validates mapping YAML. Unknown keys are errors.
func ParseMapping(b []byte) (*Mapping, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var m Mapping
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, errEOF) {
			return nil, errors.New("mapping is empty")
		}
		return nil, fmt.Errorf("mapping: %w", err)
	}
	if m.Version != 0 && m.Version != 1 {
		return nil, fmt.Errorf("mapping: unsupported version %d", m.Version)
	}
	if len(m.Reference) == 0 {
		return nil, errors.New("mapping: reference (source column(s) that identify a row) is required")
	}
	if _, ok := m.Fields["name"]; !ok {
		return nil, errors.New("mapping: fields.name is required")
	}
	allowed := map[string]bool{"name": true}
	for _, k := range standardLabels {
		allowed[k] = true
	}
	var bad []string
	for k := range m.Fields {
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("mapping: unknown fields.%s (allowed: name, %s; use labels/annotations/assignees/stages for others)",
			strings.Join(bad, ", fields."), strings.Join(standardLabels, ", "))
	}
	if m.Source.Table != "" && m.Source.Query != "" {
		return nil, errors.New("mapping: source.table and source.query are mutually exclusive")
	}
	if t := m.Tasks; t != nil {
		origins := 0
		for _, v := range []string{t.From, t.Table, t.Query} {
			if v != "" {
				origins++
			}
		}
		switch {
		case t.Reference == "":
			return nil, errors.New("mapping: tasks.reference (task id column/key) is required")
		case origins != 1:
			return nil, errors.New("mapping: tasks needs exactly one of from, table, query")
		case t.From == "" && t.ParentKey == "":
			return nil, errors.New("mapping: tasks.parent_key is required with tasks.table/query")
		case t.From != "" && t.ParentKey != "":
			return nil, errors.New("mapping: tasks.parent_key only applies to tasks.table/query")
		}
	}
	return &m, nil
}
