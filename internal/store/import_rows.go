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
	put := func(prefix string, m map[string]string, keyRe *regexp.Regexp, norm func(string) string) *RowError {
		for k, v := range m {
			k = strings.ToLower(k)
			if !keyRe.MatchString(k) {
				return rowErr(no, "INVALID_KEY", "invalid key %q under %s", k, strings.TrimSuffix(prefix, "."))
			}
			if len(v) > 4000 {
				return rowErr(no, "VALUE_TOO_LONG", "value of %q is too long", k)
			}
			r.Keys[prefix+k] = core.JSON(norm(v))
		}
		return nil
	}
	if d.Labels != nil {
		if e := put("attr.", *d.Labels, importLabelKeyRe, func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }); e != nil {
			return nil, e
		}
	}
	if d.Annotations != nil {
		if e := put("ann.", *d.Annotations, importLabelKeyRe, func(s string) string { return s }); e != nil {
			return nil, e
		}
	}
	if d.Assignees != nil {
		if e := put("assignee.", *d.Assignees, importRoleKeyRe, AccountID); e != nil {
			return nil, e
		}
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
	if in.Source.RowHash != nil {
		r.Hash = strings.TrimSpace(*in.Source.RowHash)
	}
	if len(r.Hash) > 64 {
		return nil, rowErr(no, "INVALID_SOURCE", "source.row_hash must be at most 64 characters")
	}
	if r.Hash == "" {
		r.Hash = hashKeys(r.Keys)
	}
	if r.Ref == "" && r.Code == 0 && r.System == "" {
		return nil, rowErr(no, "INVALID_SOURCE", "source.system is required (set it on the row or on the job)")
	}
	return r, nil
}

// hashKeys is the fallback row hash: sha256 over the canonical JSON of the desired keys.
func hashKeys(keys map[string]json.RawMessage) string {
	b, _ := json.Marshal(keys) // map keys are emitted sorted
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
