package core

import (
	"encoding/json"
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
}

// Validate applies the built-in checks and the rule table to a proposed event.
// Events with channel legacy/import are historical facts and are never rejected.
func Validate(in Input) error {
	if in.Actor.Channel == ChannelLegacy || in.Actor.Channel == ChannelImport {
		return nil
	}
	var reason *Reason
	if in.ReasonKey != "" {
		r, ok := in.Cfg.Reasons[in.ReasonKey]
		if !ok || !r.Active {
			return NewError(StatusInvalidArgument, "UNKNOWN_REASON", "reason %q is not defined or not active", in.ReasonKey)
		}
		reason = &r
		if r.NoteRequired && strings.TrimSpace(in.Note) == "" {
			return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "reason %q requires a note", in.ReasonKey).With("reason", in.ReasonKey)
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

	for _, c := range in.Changes {
		if err := builtin(in, c, after, reason); err != nil {
			return err
		}
		if err := checkRules(in, c, reason); err != nil {
			return err
		}
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
			if reason == nil || reason.Kind != KindNotApplicable {
				return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "marking stage %q not applicable requires a not_applicable reason", key).With("required_kind", KindNotApplicable)
			}
		case "":
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "stage %q cannot be cleared", key)
		default:
			return NewError(StatusInvalidArgument, "INVALID_VALUE", "invalid state %q for stage %q", nv, key)
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
func checkRules(in Input, c Change, reason *Reason) error {
	var byKey, byValue []Rule
	for _, r := range in.Cfg.Rules {
		if RuleMatch(r.KPattern, c.Key) {
			byKey = append(byKey, r)
		}
	}
	if len(byKey) == 0 {
		return nil
	}
	nv := Scalar(c.New)
	for _, r := range byKey {
		if r.ToValue == nil || *r.ToValue == nv {
			byValue = append(byValue, r)
		}
	}
	if len(byValue) == 0 {
		return NewError(StatusInvalidArgument, "NO_RULE_FOR_VALUE", "no rule allows setting %s to %q", c.Key, nv)
	}
	var bySide []Rule
	for _, r := range byValue {
		if r.Side == SideAny || r.Side == in.Actor.Side || in.Actor.Role == RoleAdmin {
			bySide = append(bySide, r)
		}
	}
	if len(bySide) == 0 {
		return NewError(StatusPermissionDenied, "SIDE_NOT_ALLOWED", "side %q may not set %s to %q", in.Actor.Side, c.Key, nv).
			With("key", c.Key).With("side", in.Actor.Side)
	}
	for _, r := range bySide {
		if r.ReasonKind == nil || (reason != nil && reason.Kind == *r.ReasonKind) {
			return nil
		}
	}
	// no rule satisfied: report the first required kind
	var kinds []string
	for _, r := range bySide {
		if r.ReasonKind != nil {
			kinds = append(kinds, *r.ReasonKind)
		}
	}
	return NewError(StatusFailedPrecondition, "REASON_REQUIRED", "changing %s to %q requires a reason of kind %s", c.Key, nv, strings.Join(kinds, " or ")).
		With("key", c.Key).With("required_kind", strings.Join(kinds, ","))
}
