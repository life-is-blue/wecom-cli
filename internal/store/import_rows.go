package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

var (
	importLabelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	importRoleKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
)

// RowError describes one staged row that was rejected. Bad rows never fail the batch.
type RowError struct {
	RowNo   int    `json:"row_no"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func rowErr(no int, reason, format string, args ...any) *RowError {
	return &RowError{RowNo: no, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// parseImportRow validates one raw row (api.ImportRow JSON) and normalizes it.
func parseImportRow(no int, raw json.RawMessage, defaultSystem string, cfg core.Config, p Project) (*core.ImportRow, *RowError) {
	var in api.ImportRow
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, rowErr(no, "INVALID_ROW", "row is not a valid import row: %v", err)
	}
	d := in.Data
	var re *RowError
	r := &core.ImportRow{No: no, Keys: map[string]json.RawMessage{}}
	r.System = defaultSystem
	if in.Source.System != nil && strings.TrimSpace(*in.Source.System) != "" {
		r.System = strings.TrimSpace(*in.Source.System)
	}
	if len(r.System) > 64 {
		return nil, rowErr(no, "INVALID_SOURCE", "source.system must be at most 64 characters")
	}
	if in.Source.Reference != nil {
		r.Ref = strings.TrimSpace(*in.Source.Reference)
	}
	if len(r.Ref) > 255 {
		return nil, rowErr(no, "INVALID_SOURCE", "source.reference must be at most 255 characters")
	}
	if d.Name != nil && strings.TrimSpace(*d.Name) != "" {
		id, ok := p.ParseWorkflowRef(*d.Name)
		if !ok {
			return nil, rowErr(no, "INVALID_NAME", "data.name %q is not a workflow resource name", *d.Name)
		}
		r.Code = id
	}
	if d.DisplayName == nil || strings.TrimSpace(*d.DisplayName) == "" {
		return nil, rowErr(no, "INVALID_DISPLAY_NAME", "data.display_name is required")
	}
	name := strings.TrimSpace(*d.DisplayName)
	if len(name) > 255 {
		return nil, rowErr(no, "INVALID_DISPLAY_NAME", "data.display_name must be at most 255 characters")
	}
	r.Keys["name"] = core.JSON(name)
	if d.MergedInto != nil && *d.MergedInto != "" {
		return nil, rowErr(no, "UNSUPPORTED_FIELD", "data.merged_into cannot be imported")
	}
	if e := putMaps(no, r.Keys, mapOf(d.Labels), mapOf(d.Annotations), mapOf(d.Assignees)); e != nil {
		return nil, e
	}
	if d.Stages != nil {
		for _, st := range *d.Stages {
			key := strings.ToLower(st.Stage)
			if cfg.StageSeq(key) == 0 {
				return nil, rowErr(no, "UNKNOWN_STAGE", "unknown stage %q", st.Stage)
			}
			var v string
			switch st.State {
			case "PASSED":
				v = core.StatePass
			case "OPEN":
				v = core.StateOpen
			case "NOT_APPLICABLE":
				v = core.StateNA
			default:
				return nil, rowErr(no, "INVALID_STAGE_STATE", "invalid state %q for stage %q", st.State, st.Stage)
			}
			r.Keys["stage."+key] = core.JSON(v)
		}
	}
	if d.Scope != nil && *d.Scope == "OUT_OF_SCOPE" {
		r.Keys["scope"] = core.JSON("out")
	}
	if r.Hash, re = rowHash(no, in.Source, r.Keys); re != nil {
		return nil, re
	}
	if r.Ref == "" && r.Code == 0 && r.System == "" {
		return nil, rowErr(no, "INVALID_SOURCE", "source.system is required (set it on the row or on the job)")
	}
	if in.Tasks != nil {
		for i, t := range *in.Tasks {
			it, re := parseImportTask(no, i+1, t)
			if re != nil {
				return nil, re
			}
			r.Tasks = append(r.Tasks, *it)
		}
	}
	return r, nil
}

func rowHash(no int, src api.Source, keys map[string]json.RawMessage) (string, *RowError) {
	h := ""
	if src.RowHash != nil {
		h = strings.TrimSpace(*src.RowHash)
	}
	if len(h) > 64 {
		return "", rowErr(no, "INVALID_SOURCE", "source.row_hash must be at most 64 characters")
	}
	if h == "" {
		h = hashKeys(keys)
	}
	return h, nil
}

func mapOf[T ~map[string]string](p *T) map[string]string {
	if p == nil {
		return nil
	}
	return map[string]string(*p)
}

// putMaps normalizes labels (attr.), annotations (ann.) and assignees (assignee.) into keys.
func putMaps(no int, keys map[string]json.RawMessage, labels, annotations, assignees map[string]string) *RowError {
	put := func(prefix string, m map[string]string, keyRe *regexp.Regexp, norm func(string) string) *RowError {
		for k, v := range m {
			k = strings.ToLower(k)
			if !keyRe.MatchString(k) {
				return rowErr(no, "INVALID_KEY", "invalid key %q under %s", k, strings.TrimSuffix(prefix, "."))
			}
			if len(v) > 4000 {
				return rowErr(no, "VALUE_TOO_LONG", "value of %q is too long", k)
			}
			keys[prefix+k] = core.JSON(norm(v))
		}
		return nil
	}
	lower := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	if e := put("attr.", labels, importLabelKeyRe, lower); e != nil {
		return e
	}
	if e := put("ann.", annotations, importLabelKeyRe, func(s string) string { return s }); e != nil {
		return e
	}
	return put("assignee.", assignees, importRoleKeyRe, AccountID)
}

// parseImportTask validates the n-th task of row no.
func parseImportTask(no, n int, t api.ImportTask) (*core.ImportTask, *RowError) {
	it := &core.ImportTask{Keys: map[string]json.RawMessage{}}
	if t.Source.Reference != nil {
		it.Ref = strings.TrimSpace(*t.Source.Reference)
	}
	if it.Ref == "" || len(it.Ref) > 255 {
		return nil, rowErr(no, "INVALID_TASK", "tasks[%d].source.reference is required (at most 255 characters)", n)
	}
	name := strings.TrimSpace(t.Data.DisplayName)
	if name == "" || len(name) > 255 {
		return nil, rowErr(no, "INVALID_TASK", "tasks[%d].data.display_name is required (at most 255 characters)", n)
	}
	it.Keys["name"] = core.JSON(name)
	if e := putMaps(no, it.Keys, mapOf(t.Data.Labels), mapOf(t.Data.Annotations), mapOf(t.Data.Assignees)); e != nil {
		e.Message = fmt.Sprintf("tasks[%d]: %s", n, e.Message)
		return nil, e
	}
	var re *RowError
	if it.Hash, re = rowHash(no, t.Source, it.Keys); re != nil {
		return nil, re
	}
	return it, nil
}

// hashKeys is the fallback row hash: sha256 over the canonical JSON of the desired keys.
func hashKeys(keys map[string]json.RawMessage) string {
	b, _ := json.Marshal(keys) // map keys are emitted sorted
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
