package store

import (
	"fmt"
	"strconv"
	"strings"
)

// Project identifies a project in resource names.
type Project struct {
	ID   int64
	Name string
}

// Resource id prefixes.
const (
	PrefixWorkflow = "wf"
	PrefixTask     = "tk"
	PrefixComment  = "cm"
	PrefixEvent    = "ev"
	PrefixAttach   = "at"
)

// FormatID renders wf-000123 style ids (events are not zero padded: ev-8102).
func FormatID(prefix string, id int64) string {
	if prefix == PrefixEvent {
		return fmt.Sprintf("%s-%d", prefix, id)
	}
	return fmt.Sprintf("%s-%06d", prefix, id)
}

// ParseID parses "WF-000123" / "wf-123" (case-insensitive) with the given prefix.
func ParseID(prefix, s string) (int64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	rest, ok := strings.CutPrefix(s, prefix+"-")
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (p Project) workflowName(id int64) string {
	return "projects/" + p.Name + "/workflows/" + FormatID(PrefixWorkflow, id)
}
func (p Project) taskName(id int64) string {
	return "projects/" + p.Name + "/tasks/" + FormatID(PrefixTask, id)
}
func (p Project) commentName(wf, id int64) string {
	return p.workflowName(wf) + "/comments/" + FormatID(PrefixComment, id)
}
func (p Project) eventName(wf, id int64) string {
	return p.workflowName(wf) + "/events/" + FormatID(PrefixEvent, id)
}

// WorkflowName is the exported resource-name helper.
func (p Project) WorkflowName(id int64) string { return p.workflowName(id) }

// TaskName is the exported resource-name helper.
func (p Project) TaskName(id int64) string { return p.taskName(id) }

// ParseWorkflowRef accepts a full resource name or a bare id and returns the workflow entity id.
func (p Project) ParseWorkflowRef(ref string) (int64, bool) {
	ref = strings.TrimSpace(ref)
	if rest, ok := strings.CutPrefix(strings.ToLower(ref), "projects/"+strings.ToLower(p.Name)+"/workflows/"); ok {
		ref = rest
	}
	return ParseID(PrefixWorkflow, ref)
}

func accountName(user string) string { return "accounts/" + user }

// AccountID strips an optional "accounts/" prefix.
func AccountID(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "accounts/") }
