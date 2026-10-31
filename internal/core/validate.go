package core

import (
	"encoding/json"
	"sort"
	"strings"
)

// Stage state values stored under stage.<key>.
const (
	StatePass = "pass"
	StateOpen = "open"
	StateNA   = "na"
)

// Env gives validation read access to other workflows (merge checks). Implemented by the store.
type Env interface {
	// WorkflowExists reports whether a live (not deleted) workflow with this entity id exists in the project.
	WorkflowExists(id int64) (bool, error)
	// MergedInto returns the merge target of a workflow that is out of scope because it was merged.
	MergedInto(id int64) (int64, bool, error)
}

// Input is everything Validate needs; no SQL involved.
type Input struct {
	Cfg       Config
	Actor     Actor
	EntityTyp string // workflow|task|comment|attachment
	EntityID  int64  // 0 when the entity is being created
	ReasonKey string
	Note      string
	Current   map[string]json.RawMessage // latest values before this event
	Changes   []Change                   // Old/New filled in, no-ops removed
	Env       Env
	// Evidence are the validated attachment names the request carries. An admin may set a key that a
	// rule reserves for the other side only with evidence (INV-29).
	Evidence []string
}

// OverrideKeyPrefix starts the key of the record an admin override leaves on the event:
// override.<changed key> = {"side": <side the rule reserved>, "evidence": [<attachment names>]}.
const OverrideKeyPrefix = "override."

// Validate applies the built-in checks and the rule table to a proposed event. It returns the
// override records the event must carry (one per change an admin made for the other side); the
// caller appends them to the event unconditionally.
// Events with channel legacy/import are historical facts and are never rejected.
func Validate(in Input) ([]Change, error) {
	if in.Actor.Channel == ChannelLegacy || in.Actor.Channel == ChannelImport {
		return nil, nil
	}
	if err := checkLive(in); err != nil {
		return nil, err
	}
	var reason *Reason
	if in.ReasonKey != "" {
		r, ok := in.Cfg.Reasons[in.ReasonKey]
		if !ok || !r.Active {
			var keys []string
			for k, rr := range in.Cfg.Reasons {
				if rr.Active {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			return nil, NewError(StatusInvalidArgument, "UNKNOWN_REASON", "reason %q is not defined or not active; valid reasons: %s", in.ReasonKey, strings.Join(keys, ", ")).
				With("valid_reasons", strings.Join(keys, ","))
		}
		reason = &r
		if r.NoteRequired && strings.TrimSpace(in.Note) == "" {
			return nil, NewError(StatusFailedPrecondition, "REASON_REQUIRED", "reason %q requires a note", in.ReasonKey).With("reason", in.ReasonKey)
		}
	}

	// state after applying the whole event, used by order and merge checks
	after := map[string]json.RawMessage{}
	for k, v := range in.Current {
		after[k] = v
	}
	for _, c := range in.Changes {
		if len(c.New) == 0 || string(c.New) == "null" {
			delete(after, c.Key)
		} else {
			after[c.Key] = c.New
		}
	}

	if err := checkLaterStages(in, after); err != nil {
		return nil, err
	}

	var overrides []Change
	for _, c := range in.Changes {
		if err := builtin(in, c, after, reason); err != nil {
			return nil, err
		}
		side, err := checkRules(in, c, reason)
		if err != nil {
			return nil, err
		}
		if side != "" {
			rec, _ := json.Marshal(map[string]any{"side": side, "evidence": in.Evidence})
			k := OverrideKeyPrefix + c.Key
			overrides = append(overrides, Change{Key: k, Old: in.Current[k], New: rec})
		}
	}
	return overrides, nil
}

// checkLaterStages refuses a change set that sets a stage to OPEN and leaves a LATER stage PASSED in the
// resulting state (INV-31): a passed stage must never sit behind an open one. There is no automatic
// cascade (reopening a later stage is the call of the side that owns it), so the caller names every stage
// it reopens. Applies to every path that produces such a change set. NOT_APPLICABLE counts like PASSED
// for ordering, so a later N/A stage is fine.
func checkLaterStages(in Input, after map[string]json.RawMessage) error {
	var reopened, later []string
	seen := map[string]bool{}
	for _, c := range in.Changes {
		key := strings.TrimPrefix(c.Key, "stage.")
		if key == c.Key {
			continue
		}
		if st, _ := Str(c.New); st != StateOpen {
			continue
		}
		seq := in.Cfg.StageSeq(key)
		if seq == 0 {
			continue // builtin reports UNKNOWN_STAGE
		}
		reopened = append(reopened, key)
		for _, s := range in.Cfg.Stages {
			if st, _ := Str(after["stage."+s.Key]); s.Seq > seq && st == StatePass && !seen[s.Key] {
				seen[s.Key] = true
				later = append(later, s.Key)
			}
		}
	}
	if len(later) == 0 {
		return nil
	}
	sort.Strings(reopened)
	return NewError(StatusFailedPrecondition, "REGRESS_LATER_STAGES_PASSED",
		"reopening %s would leave later stage(s) %s PASSED; include them in the change (reopening them is subject to the rules of their own side)",
		strings.Join(reopened, ", "), strings.Join(later, ", ")).
		With("stages", strings.Join(later, ","))
}

// checkLive refuses every change to a soft-deleted entity except the one that restores it, and every
// change to a comment whose workflow is soft-deleted (INV-26).
func checkLive(in Input) error {
	if in.EntityID == 0 {
		return nil
	}
	if Bool(in.Current["deleted"]) {
		for _, c := range in.Changes {
			if c.Key == "deleted" && !Bool(c.New) {
				return checkParentLive(in) // undelete
			}
		}
		return NewError(StatusFailedPrecondition, "ENTITY_DELETED", "the %s is deleted; only undelete is allowed", in.EntityTyp).With("entity", in.EntityTyp)
	}
	if in.EntityTyp == "comment" && in.Env != nil {
		var wf int64
		if json.Unmarshal(in.Current["on"], &wf) == nil && wf != 0 {
			ok, err := in.Env.WorkflowExists(wf)
			if err != nil {
				return err
			}
			if !ok {
				return NewError(StatusFailedPrecondition, "ENTITY_DELETED", "the workflow of this comment is deleted").With("entity", "workflow")
			}
		}
	}
	return nil
}

// checkParentLive refuses to restore a task whose workflow is deleted (the workflow's own undelete
// restores the tasks its delete took along).
func checkParentLive(in Input) error {
	if in.EntityTyp != "task" || in.Env == nil {
		return nil
	}
	var wf int64
	if json.Unmarshal(in.Current["workflow"], &wf) != nil || wf == 0 {
		return nil
	}
	ok, err := in.Env.WorkflowExists(wf)
	if err != nil {
		return err
	}
	if !ok {
		return NewError(StatusFailedPrecondition, "PARENT_DELETED", "the workflow of this task is deleted; undelete the workflow first").With("entity", "workflow")
	}
	return nil
}

func builtin(in Input, c Change, after map[string]json.RawMessage, reason *Reason) error {
	switch {
	case strings.HasPrefix(c.Key, "stage."):
		key := strings.TrimPrefix(c.Key, "stage.")
		seq := in.Cfg.StageSeq(key)
		if seq == 0 {
			return NewError(StatusInvalidArgument, "UNKNOWN_STAGE", "unknown stage %q", key)
		}
		nv, _ := Str(c.New)
		ov, _ := Str(c.Old)
		switch nv {
		case StatePass:
			// rule 3: every earlier stage must be pass or na
			for _, s := range in.Cfg.Stages {
				if s.Seq >= seq {
					continue
				}
				st, _ := Str(after["stage."+s.Key])
				if st != StatePass && st != StateNA {
					return NewError(StatusFailedPrecondition, "STAGE_ORDER_VIOLATION",
						"stage %q cannot pass before earlier stage %q is passed or not applicable", key, s.Key).
						With("stage", key).With("blocking_stage", s.Key)
				}
			}
		case StateOpen:
			if ov == StatePass && (reason == nil || reason.Kind != KindReject) {
				return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "reopening passed stage %q requires a reject reason", key).With("required_kind", KindReject)
			}
		case StateNA:
			if ov == StatePass { // leaving PASSED takes a reject reason, N/A takes another kind: regress first
				return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "passed stage %q must be regressed with a reject reason before it can be marked not applicable", key).With("required_kind", KindReject)
			}
			if reason == nil || reason.Kind != KindNotApplicable {
				return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "marking stage %q not applicable requires a not_applicable reason", key).With("required_kind", KindNotApplicable)
			}
		case "":
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "stage %q cannot be cleared", key)
		default:
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "invalid state %q for stage %q", nv, key)
		}
	case c.Key == "workflow" && in.EntityTyp == "task":
		var wf int64
		if json.Unmarshal(c.New, &wf) != nil || wf <= 0 {
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "invalid workflow")
		}
		if in.Env != nil {
			ok, err := in.Env.WorkflowExists(wf)
			if err != nil {
				return err
			}
			if !ok {
				return NewError(StatusFailedPrecondition, "ENTITY_DELETED", "the workflow is deleted; a task cannot be placed in it").With("entity", "workflow")
			}
		}
	case c.Key == "into":
		if len(c.New) > 0 && string(c.New) != "null" {
			if err := checkMerge(in, after); err != nil {
				return err
			}
		}
	case c.Key == "scope":
		nv, _ := Str(c.New)
		switch nv {
		case "in":
		case "out":
			if reason == nil || reason.Kind != KindDiscard {
				return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "moving out of scope requires a discard reason").With("required_kind", KindDiscard)
			}
			if reason.Key == "merged" {
				return checkMerge(in, after)
			}
		default:
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "invalid scope %q", nv)
		}
	}
	return nil
}

func checkMerge(in Input, after map[string]json.RawMessage) error {
	raw, ok := after["into"]
	if !ok {
		return NewError(StatusInvalidArgument, "MERGE_TARGET_REQUIRED", "reason merged requires merged_into")
	}
	var target int64
	if json.Unmarshal(raw, &target) != nil || target <= 0 {
		return NewError(StatusInvalidArgument, "INVALID_VALUE", "invalid merge target")
	}
	if in.EntityID != 0 && target == in.EntityID {
		return NewError(StatusFailedPrecondition, "MERGE_CYCLE", "a workflow cannot be merged into itself")
	}
	if in.Env == nil {
		return nil
	}
	exists, err := in.Env.WorkflowExists(target)
	if err != nil {
		return err
	}
	if !exists {
		return NewError(StatusInvalidArgument, "MERGE_TARGET_INVALID", "merge target does not exist")
	}
	cur := target
	for i := 0; i < 1000; i++ {
		next, ok, err := in.Env.MergedInto(cur)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if next == in.EntityID {
			return NewError(StatusFailedPrecondition, "MERGE_CYCLE", "merging would create a cycle")
		}
		cur = next
	}
	return NewError(StatusFailedPrecondition, "MERGE_CYCLE", "merge chain too long")
}

// checkRules implements the rule-table semantics: a key matching any k_pattern must satisfy
// one matching rule (value, side, reason kind); keys matching no rule are free for editors.
// An admin whose side no rule allows may still make the change with evidence (the override): the
// returned string is then the side(s) the rule reserved, "" otherwise.
func checkRules(in Input, c Change, reason *Reason) (string, error) {
	var byKey, byValue []Rule
	for _, r := range in.Cfg.Rules {
		if RuleMatch(r.KPattern, c.Key) {
			byKey = append(byKey, r)
		}
	}
	if len(byKey) == 0 {
		return "", nil
	}
	nv := Scalar(c.New)
	for _, r := range byKey {
		if r.ToValue == nil || *r.ToValue == nv {
			byValue = append(byValue, r)
		}
	}
	if len(byValue) == 0 {
		return "", NewError(StatusInvalidArgument, "NO_RULE_FOR_VALUE", "no rule allows setting %s to %q", c.Key, nv)
	}
	var bySide []Rule
	for _, r := range byValue {
		if r.Side == SideAny || r.Side == in.Actor.Side {
			bySide = append(bySide, r)
		}
	}
	override := ""
	if len(bySide) == 0 {
		if in.Actor.Role != RoleAdmin {
			return "", NewError(StatusPermissionDenied, "SIDE_NOT_ALLOWED", "side %q may not set %s to %q", in.Actor.Side, c.Key, nv).
				With("key", c.Key).With("side", in.Actor.Side)
		}
		sides := map[string]bool{}
		for _, r := range byValue {
			sides[r.Side] = true
		}
		var names []string
		for s := range sides {
			names = append(names, s)
		}
		sort.Strings(names)
		override = strings.Join(names, ",")
		if len(in.Evidence) == 0 {
			return "", NewError(StatusFailedPrecondition, "EVIDENCE_REQUIRED_FOR_OVERRIDE",
				"setting %s to %q is reserved for side %s; an admin of side %q may do it for them only with evidence: attach the proof to the workflow first (comment add --attach) and pass its name in evidence_attachments (CLI --evidence)",
				c.Key, nv, override, in.Actor.Side).With("key", c.Key).With("side", override)
		}
		bySide = byValue
	}
	for _, r := range bySide {
		if r.ReasonKind == nil || (reason != nil && reason.Kind == *r.ReasonKind) {
			return override, nil
		}
	}
	// no rule satisfied: report the first required kind
	var kinds []string
	for _, r := range bySide {
		if r.ReasonKind != nil {
			kinds = append(kinds, *r.ReasonKind)
		}
	}
	return "", NewError(StatusFailedPrecondition, "REASON_REQUIRED", "changing %s to %q requires a reason of kind %s", c.Key, nv, strings.Join(kinds, " or ")).
		With("key", c.Key).With("required_kind", strings.Join(kinds, ","))
}
