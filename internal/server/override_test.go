package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

// evidenceFor makes an attachment of the workflow (straight through the store) and returns its resource name.
func (f *fixture) evidenceFor(wfName string) string {
	f.t.Helper()
	m, _, err := f.st.MemberOf(context.Background(), f.project, "ed"+f.project)
	if err != nil {
		f.t.Fatal(err)
	}
	p := store.Project{ID: m.ProjectID, Name: f.project}
	wf, ok := p.ParseWorkflowRef(wfName)
	if !ok {
		f.t.Fatalf("workflow name %q", wfName)
	}
	res, err := f.st.Apply(context.Background(), store.ChangeRequest{ProjectID: p.ID, EntityType: "attachment",
		Actor: core.Actor{Account: "x", Side: "system", Role: "editor", Channel: core.ChannelLegacy},
		Plan: func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
			return []store.PlannedChange{{Key: "on", New: core.JSON(wf)}, {Key: "filename", New: core.JSON("proof.txt")}}, nil
		}})
	if err != nil {
		f.t.Fatal(err)
	}
	return p.AttachmentName(wf, res.EntityID)
}

// INV-29: an admin acts for the other side only with evidence; the event keeps the real actor and side and carries
// an override.<key> record that `SELECT ... FROM events` finds.
func TestAdminOverrideNeedsEvidence(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project
	w := f.wantOK(f.do(f.editor, "POST", base+"/workflows", map[string]any{"display_name": "w"}))
	name := str(w.body, "name")
	path := "/v1/" + name
	for _, st := range []string{"development", "deployment", "verify"} {
		f.wantOK(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": st}))
	}
	ev := f.evidenceFor(name)

	// accept belongs to the customer: an editor of the delivery side is denied, an admin needs evidence
	f.wantErr(f.do(f.editor, "POST", path+":advanceStage", map[string]any{"stage": "accept", "evidence_attachments": []string{ev}}), 403, "PERMISSION_DENIED", "SIDE_NOT_ALLOWED")
	r := f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "accept"})
	f.wantErr(r, 400, "FAILED_PRECONDITION", "EVIDENCE_REQUIRED_FOR_OVERRIDE")
	if msg := r.body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "customer") || !strings.Contains(msg, "evidence") {
		t.Fatalf("message must name the side and how to attach evidence: %s", msg)
	}
	f.wantErr(f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "accept", "evidence_attachments": []string{"at-999999"}}), 400, "INVALID_ARGUMENT", "INVALID_EVIDENCE")
	f.wantOK(f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "accept", "evidence_attachments": []string{ev}}))

	// regress and the stages mask go through the same rule
	f.wantErr(f.do(f.admin, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count"}), 400, "FAILED_PRECONDITION", "EVIDENCE_REQUIRED_FOR_OVERRIDE")
	f.wantOK(f.do(f.admin, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count", "evidence_attachments": []string{ev}}))
	patch := func(extra map[string]any) resp {
		b := map[string]any{"workflow": map[string]any{"stages": []any{map[string]any{"stage": "accept", "state": "PASSED"}}}}
		for k, v := range extra {
			b[k] = v
		}
		return f.do(f.admin, "PATCH", path+"?update_mask=stages.accept", b)
	}
	f.wantErr(patch(nil), 400, "FAILED_PRECONDITION", "EVIDENCE_REQUIRED_FOR_OVERRIDE")
	f.wantOK(patch(map[string]any{"evidence_attachments": []string{ev}}))
	// the same override twice leaves a record each time
	f.wantOK(f.do(f.admin, "POST", path+":regressStage", map[string]any{"stages": []string{"accept"}, "reason": "rej_row_count", "evidence_attachments": []string{ev}}))
	f.wantOK(patch(map[string]any{"evidence_attachments": []string{ev}}))

	// an admin acting for their own side needs no evidence and leaves no record; nor does the customer
	f.wantOK(f.do(f.admin, "POST", path+":advanceStage", map[string]any{"stage": "dual_run"}))

	q := f.wantOK(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT actor, side, k, new_v FROM events WHERE k LIKE 'override.%' ORDER BY event_id"}))
	rows := q.body["rows"].([]any)
	if len(rows) != 5 {
		t.Fatalf("want 5 override records (advance, regress, patch, regress, patch), got %d: %s", len(rows), q.raw)
	}
	for _, row := range rows {
		c := row.([]any)
		var rec struct {
			Side     string   `json:"side"`
			Evidence []string `json:"evidence"`
		}
		raw, _ := json.Marshal(c[3])
		if s, ok := c[3].(string); ok {
			raw = []byte(s)
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("record %v: %v", c[3], err)
		}
		if !strings.HasPrefix(fmt.Sprint(c[0]), "ad") || c[1] != "delivery" || c[2] != "override.stage.accept" || rec.Side != "customer" || len(rec.Evidence) != 1 || rec.Evidence[0] != ev {
			t.Fatalf("override record = %v (real actor and side must stay, reserved side = customer)", c)
		}
	}
	// the evidence of the advance is also kept under evidence.<stage>
	e := f.wantOK(f.do(f.viewer, "POST", base+":query", map[string]any{"sql": "SELECT COUNT(*) FROM events WHERE k = 'evidence.accept'"}))
	if e.body["rows"].([]any)[0].([]any)[0].(float64) != 1 {
		t.Fatalf("evidence.accept: %s", e.raw)
	}
}
