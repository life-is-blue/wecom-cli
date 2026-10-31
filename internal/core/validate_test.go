package core

import (
	"encoding/json"
	"errors"
	"testing"
)

func sp(s string) *string { return &s }

func testCfg() Config {
	names := []string{"development", "deployment", "verify", "accept", "dual_run", "cutover"}
	var st []Stage
	for i, n := range names {
		st = append(st, Stage{Key: n, Seq: i + 1})
	}
	r := map[string]Reason{}
	add := func(k, kind string, note bool) { r[k] = Reason{Key: k, Kind: kind, NoteRequired: note, Active: true} }
	add("rej_row_count", KindReject, false)
	add("rej_other", KindReject, true)
	add("na_manual_trigger", KindNotApplicable, true)
	add("obsolete", KindDiscard, false)
	add("merged", KindDiscard, false)
	add("customer_env_issue", KindGateOverride, false)
	r["inactive"] = Reason{Key: "inactive", Kind: KindReject}
	rules := []Rule{
		{"stage.development", sp("pass"), "delivery", nil},
		{"stage.deployment", sp("pass"), "delivery", nil},
		{"stage.verify", sp("pass"), "delivery", nil},
		{"stage.accept", sp("pass"), "customer", nil},
		{"stage.dual_run", sp("pass"), "delivery", nil},
		{"stage.cutover", sp("pass"), "delivery", nil},
		{"stage.%", sp("na"), "any", sp("not_applicable")},
		{"stage.development", sp("open"), "delivery", sp("reject")},
		{"stage.verify", sp("open"), "delivery", sp("reject")},
		{"stage.verify", sp("open"), "customer", sp("reject")},
		{"stage.accept", sp("open"), "customer", sp("reject")},
		{"scope", sp("out"), "any", sp("discard")},
		{"scope", sp("in"), "any", nil},
	}
	return Config{Stages: st, Reasons: r, Rules: rules}
}

type fakeEnv struct {
	exists map[int64]bool
	into   map[int64]int64
}

func (f fakeEnv) WorkflowExists(id int64) (bool, error) { return f.exists[id], nil }
func (f fakeEnv) MergedInto(id int64) (int64, bool, error) {
	v, ok := f.into[id]
	return v, ok, nil
}

func j(s string) json.RawMessage { return json.RawMessage(s) }

func TestValidate(t *testing.T) {
	delivery := Actor{Account: "a", Side: "delivery", Role: "editor", Channel: "api"}
	customer := Actor{Account: "c", Side: "customer", Role: "editor", Channel: "api"}
	admin := Actor{Account: "x", Side: "delivery", Role: "admin", Channel: "api"}
	legacy := Actor{Account: "x", Side: "system", Role: "editor", Channel: "legacy"}
	env := fakeEnv{exists: map[int64]bool{1: true, 2: true, 3: true}, into: map[int64]int64{2: 1, 1: 3}}

	tests := []struct {
		name   string
		actor  Actor
		reason string
		note   string
		cur    map[string]json.RawMessage
		ch     []Change
		id     int64
		status string // "" = ok
		code   string
	}{
		{name: "advance first stage", actor: delivery, ch: []Change{{Key: "stage.development", New: j(`"pass"`)}}},
		{name: "free key", actor: delivery, ch: []Change{{Key: "attr.domain", New: j(`"x"`)}}},
		{name: "order violation", actor: delivery, ch: []Change{{Key: "stage.deployment", New: j(`"pass"`)}},
			status: StatusFailedPrecondition, code: "STAGE_ORDER_VIOLATION"},
		{name: "order ok after prior pass", actor: delivery,
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.deployment", Old: nil, New: j(`"pass"`)}}},
		{name: "order ok after na", actor: delivery, reason: "na_manual_trigger", note: "n",
			ch: []Change{{Key: "stage.development", New: j(`"na"`)}, {Key: "stage.deployment", New: j(`"pass"`)}}},
		{name: "same event ordered passes", actor: delivery,
			ch: []Change{{Key: "stage.development", New: j(`"pass"`)}, {Key: "stage.deployment", New: j(`"pass"`)}}},
		{name: "accept by delivery denied", actor: delivery,
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`), "stage.deployment": j(`"pass"`), "stage.verify": j(`"pass"`)},
			ch:  []Change{{Key: "stage.accept", New: j(`"pass"`)}}, status: StatusPermissionDenied, code: "SIDE_NOT_ALLOWED"},
		{name: "accept by customer ok", actor: customer,
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`), "stage.deployment": j(`"pass"`), "stage.verify": j(`"pass"`)},
			ch:  []Change{{Key: "stage.accept", New: j(`"pass"`)}}},
		{name: "accept by admin ok", actor: admin,
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`), "stage.deployment": j(`"pass"`), "stage.verify": j(`"pass"`)},
			ch:  []Change{{Key: "stage.accept", New: j(`"pass"`)}}},
		{name: "regress without reason", actor: delivery,
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"open"`)}}, status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "regress wrong kind", actor: delivery, reason: "obsolete",
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"open"`)}}, status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "regress with reject reason", actor: delivery, reason: "rej_row_count",
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"open"`)}}},
		{name: "regress needs note when reason demands", actor: delivery, reason: "rej_other",
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"open"`)}}, status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "regress deployment by delivery has no open rule", actor: delivery, reason: "rej_row_count",
			cur: map[string]json.RawMessage{"stage.deployment": j(`"pass"`)},
			ch:  []Change{{Key: "stage.deployment", Old: j(`"pass"`), New: j(`"open"`)}}, status: StatusInvalidArgument, code: "NO_RULE_FOR_VALUE"},
		{name: "customer rejects verify", actor: customer, reason: "rej_row_count",
			cur: map[string]json.RawMessage{"stage.verify": j(`"pass"`)},
			ch:  []Change{{Key: "stage.verify", Old: j(`"pass"`), New: j(`"open"`)}}},
		{name: "na without reason", actor: delivery, ch: []Change{{Key: "stage.verify", New: j(`"na"`)}},
			status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "na wrong kind", actor: delivery, reason: "rej_row_count", ch: []Change{{Key: "stage.verify", New: j(`"na"`)}},
			status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "passed to na with na reason is refused", actor: delivery, reason: "na_manual_trigger", note: "n",
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"na"`)}}, status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "passed to na with reject reason is refused", actor: delivery, reason: "rej_row_count",
			cur: map[string]json.RawMessage{"stage.development": j(`"pass"`)},
			ch:  []Change{{Key: "stage.development", Old: j(`"pass"`), New: j(`"na"`)}}, status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "unknown stage", actor: delivery, ch: []Change{{Key: "stage.nope", New: j(`"pass"`)}}, status: StatusInvalidArgument, code: "UNKNOWN_STAGE"},
		{name: "unknown reason", actor: delivery, reason: "zzz", ch: []Change{{Key: "attr.a", New: j(`"x"`)}}, status: StatusInvalidArgument, code: "UNKNOWN_REASON"},
		{name: "inactive reason", actor: delivery, reason: "inactive", ch: []Change{{Key: "attr.a", New: j(`"x"`)}}, status: StatusInvalidArgument, code: "UNKNOWN_REASON"},
		{name: "scope out needs discard", actor: delivery, ch: []Change{{Key: "scope", New: j(`"out"`)}}, id: 5,
			status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "scope out wrong kind", actor: delivery, reason: "rej_row_count", ch: []Change{{Key: "scope", New: j(`"out"`)}}, id: 5,
			status: StatusFailedPrecondition, code: "REASON_REQUIRED"},
		{name: "scope out discard", actor: delivery, reason: "obsolete", ch: []Change{{Key: "scope", New: j(`"out"`)}}, id: 5},
		{name: "scope in", actor: customer, ch: []Change{{Key: "scope", New: j(`"in"`)}}, id: 5},
		{name: "merged without into", actor: delivery, reason: "merged", ch: []Change{{Key: "scope", New: j(`"out"`)}}, id: 5,
			status: StatusInvalidArgument, code: "MERGE_TARGET_REQUIRED"},
		{name: "merged into self", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`5`)}}, status: StatusFailedPrecondition, code: "MERGE_CYCLE"},
		{name: "merged into missing", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`99`)}}, status: StatusInvalidArgument, code: "MERGE_TARGET_INVALID"},
		{name: "merged ok", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`3`)}}},
		{name: "merge cycle via chain", actor: delivery, reason: "merged", id: 1, // 2 -> 1 already; merging 1 into 2 closes a cycle
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`2`)}}, status: StatusFailedPrecondition, code: "MERGE_CYCLE"},
		{name: "merge target zero", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`0`)}}, status: StatusInvalidArgument, code: "INVALID_VALUE"},
		{name: "merge target negative", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`-3`)}}, status: StatusInvalidArgument, code: "INVALID_VALUE"},
		{name: "merge cycle two hops away", actor: delivery, reason: "merged", id: 3, // 2 -> 1 -> 3 already; 3 into 2 closes it
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`2`)}}, status: StatusFailedPrecondition, code: "MERGE_CYCLE"},
		{name: "merge into chain end is fine", actor: delivery, reason: "merged", id: 5,
			ch: []Change{{Key: "scope", New: j(`"out"`)}, {Key: "into", New: j(`2`)}}},
		{name: "retargeting a merge is checked too", actor: delivery, reason: "merged", id: 5, // scope unchanged, only into moves
			cur: map[string]json.RawMessage{"scope": j(`"out"`), "into": j(`3`)},
			ch:  []Change{{Key: "into", Old: j(`3`), New: j(`99`)}}, status: StatusInvalidArgument, code: "MERGE_TARGET_INVALID"},
		{name: "legacy bypasses everything", actor: legacy, ch: []Change{{Key: "stage.cutover", New: j(`"pass"`)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Input{Cfg: testCfg(), Actor: tc.actor, EntityTyp: "workflow", EntityID: tc.id,
				ReasonKey: tc.reason, Note: tc.note, Current: tc.cur, Changes: tc.ch, Env: env})
			if tc.status == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("want %s/%s, got %v", tc.status, tc.code, err)
			}
			if ce.Status != tc.status || ce.Reason != tc.code {
				t.Fatalf("want %s/%s, got %s/%s (%s)", tc.status, tc.code, ce.Status, ce.Reason, ce.Message)
			}
		})
	}
}

func TestLikeMatch(t *testing.T) {
	tests := []struct {
		p, s string
		want bool
	}{
		{"stage.%", "stage.verify", true},
		{"stage.%", "stagexverify", false},
		{"stage.verify", "stage.verify", true},
		{"stage.verify", "stage.verifyx", false},
		{`stage\_x`, "stage_x", true},
		{`stage\_x`, "stageax", false},
		{"stage_x", "stageax", true},
		{`100\%`, "100%", true},
		{`100\%`, "100x", false},
		{"a.b", "aXb", false}, // dot is literal, not regex
		{"%", "", true},
	}
	for _, tc := range tests {
		if got := LikeMatch(tc.p, tc.s); got != tc.want {
			t.Errorf("LikeMatch(%q,%q)=%v want %v", tc.p, tc.s, got, tc.want)
		}
	}
	if EscapeLike(`a%b_c\d`) != `a\%b\_c\\d` {
		t.Errorf("EscapeLike wrong: %s", EscapeLike(`a%b_c\d`))
	}
}
