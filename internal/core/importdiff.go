package core

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Import row classifications (stored in import_row.action).
const (
	ActionNew       = "new"
	ActionChanged   = "changed"
	ActionUnchanged = "unchanged"
	ActionMissing   = "missing"
	ActionConflict  = "conflict"
	ActionSuspect   = "suspect"
)

// Kinds of a KeyChange.
const (
	ChangeSource   = "source"   // source-owned key (display name, source.*): imports may overwrite it
	ChangeFill     = "fill"     // platform-owned key that was never set: imports may fill it in
	ChangeConflict = "conflict" // platform-owned key edited in the platform and different in the source
)

// ImportRow is a validated, normalized staged row. Keys holds the desired storage keys
// (name, attr.*, ann.*, assignee.*, stage.*, scope); source.* keys are derived from System/Ref/Hash.
type ImportRow struct {
	No     int                        `json:"no"`
	System string                     `json:"system,omitempty"`
	Ref    string                     `json:"ref,omitempty"`
	Hash   string                     `json:"hash"`
	Code   int64                      `json:"code,omitempty"` // platform workflow id when the row carries a platform code
	Keys   map[string]json.RawMessage `json:"keys"`
}

// ImportExisting is the current state of one workflow as the matcher needs it.
type ImportExisting struct {
	ID       int64
	Deleted  bool
	Vals     map[string]json.RawMessage // latest non-null value per key
	Touched  map[string]bool            // key has ever been set (a removed key stays touched)
	Platform map[string]bool            // key's latest change was made by a person (cli/api channel)
}

// KeyChange is one proposed key change of a matched workflow.
type KeyChange struct {
	Key  string
	Old  json.RawMessage
	New  json.RawMessage
	Kind string
}

// Decision is the classification of one row.
type Decision struct {
	Action  string
	Match   int64 // matched workflow id (0 for new)
	Changes []KeyChange
}

// IsSourceOwned reports whether an import may overwrite the key on an existing workflow.
// Everything else (stages, scope, assignees, labels, annotations, deletion, ...) is platform-owned.
func IsSourceOwned(key string) bool {
	return key == "name" || strings.HasPrefix(key, "source.")
}

// SourceKeys returns the source.* keys derived from the row.
func (r ImportRow) SourceKeys() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if r.System != "" {
		out["source.system"] = JSON(r.System)
	}
	if r.Ref != "" {
		out["source.ref"] = JSON(r.Ref)
	}
	if r.Hash != "" {
		out["source.hash"] = JSON(r.Hash)
	}
	return out
}

// Desired returns Keys plus the source.* keys.
func (r ImportRow) Desired() map[string]json.RawMessage {
	out := r.SourceKeys()
	for k, v := range r.Keys {
		out[k] = v
	}
	return out
}

// ImportMatcher indexes existing workflows for row matching.
type ImportMatcher struct {
	byID    map[int64]*ImportExisting
	byRef   map[string]*ImportExisting
	byName  map[string]*ImportExisting
	ordered []*ImportExisting
}

func refKey(system, ref string) string { return system + "\x00" + ref }

func nameGroupKey(name, group string) string {
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" + strings.ToLower(strings.TrimSpace(group))
}

// NewImportMatcher indexes the existing workflows. Live workflows win over deleted ones for the
// same source reference; among equals the lowest id wins.
func NewImportMatcher(existing []*ImportExisting) *ImportMatcher {
	m := &ImportMatcher{byID: map[int64]*ImportExisting{}, byRef: map[string]*ImportExisting{}, byName: map[string]*ImportExisting{}}
	sorted := append([]*ImportExisting(nil), existing...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	m.ordered = sorted
	prefer := func(cur, cand *ImportExisting) bool { return cur == nil || (cur.Deleted && !cand.Deleted) }
	for _, e := range sorted {
		m.byID[e.ID] = e
		sys, _ := Str(e.Vals["source.system"])
		ref, _ := Str(e.Vals["source.ref"])
		if ref != "" {
			k := refKey(sys, ref)
			if prefer(m.byRef[k], e) {
				m.byRef[k] = e
			}
		}
		if !e.Deleted {
			name, _ := Str(e.Vals["name"])
			grp, _ := Str(e.Vals["attr.group"])
			k := nameGroupKey(name, grp)
			if m.byName[k] == nil {
				m.byName[k] = e
			}
		}
	}
	return m
}

// Match finds the workflow a row refers to: platform code, then source system+reference,
// else a suspect name+group match for rows that carry no source id. how is code|ref|suspect|"".
func (m *ImportMatcher) Match(r ImportRow) (e *ImportExisting, how string) {
	if r.Code != 0 {
		if e := m.byID[r.Code]; e != nil {
			return e, "code"
		}
	}
	if r.Ref != "" {
		if e := m.byRef[refKey(r.System, r.Ref)]; e != nil {
			return e, "ref"
		}
		return nil, ""
	}
	name, _ := Str(r.Keys["name"])
	grp, _ := Str(r.Keys["attr.group"])
	if e := m.byName[nameGroupKey(name, grp)]; e != nil {
		return e, "suspect"
	}
	return nil, ""
}

// Classify decides what a row means against the current data. base is the Keys of the last
// applied import of the same source reference (nil when unknown); it lets a platform edit
// win silently when the source did not change that key since.
func Classify(r ImportRow, m *ImportMatcher, base map[string]json.RawMessage) Decision {
	e, how := m.Match(r)
	if e == nil {
		return Decision{Action: ActionNew}
	}
	d := Decision{Match: e.ID}
	hash, _ := Str(e.Vals["source.hash"])
	if how != "suspect" && hash != "" && hash == r.Hash {
		d.Action = ActionUnchanged
		return d
	}
	if e.Deleted && how != "suspect" {
		d.Action = ActionConflict
		d.Changes = []KeyChange{{Key: "deleted", Old: JSON(true), Kind: ChangeConflict}}
		return d
	}
	changes, conflict := compareKeys(r, e, base)
	d.Changes = changes
	switch {
	case how == "suspect":
		d.Action = ActionSuspect
	case conflict:
		d.Action = ActionConflict
	default:
		d.Action = ActionChanged
	}
	return d
}

func compareKeys(r ImportRow, e *ImportExisting, base map[string]json.RawMessage) (changes []KeyChange, conflict bool) {
	want := r.Desired()
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		nv, cur := want[k], e.Vals[k]
		if JSONEqual(cur, nv) {
			continue
		}
		switch {
		case IsSourceOwned(k):
			changes = append(changes, KeyChange{Key: k, Old: cur, New: nv, Kind: ChangeSource})
		case !e.Touched[k]:
			changes = append(changes, KeyChange{Key: k, Old: cur, New: nv, Kind: ChangeFill})
		case e.Platform[k] && (base == nil || !JSONEqual(base[k], nv)):
			conflict = true
			changes = append(changes, KeyChange{Key: k, Old: cur, New: nv, Kind: ChangeConflict})
		}
		// otherwise the key was last set by an import and is platform-owned: never overwritten, not a change
	}
	return changes, conflict
}

// Missing lists live workflows of the given source system that no staged row matched.
func (m *ImportMatcher) Missing(system string, matched map[int64]bool) []*ImportExisting {
	var out []*ImportExisting
	for _, e := range m.ordered {
		if e.Deleted || matched[e.ID] {
			continue
		}
		sys, _ := Str(e.Vals["source.system"])
		ref, _ := Str(e.Vals["source.ref"])
		if sys == system && ref != "" {
			out = append(out, e)
		}
	}
	return out
}

// JSONEqual compares two raw JSON values semantically; nil, empty and null are all "absent".
func JSONEqual(a, b json.RawMessage) bool {
	pa, pb := parseAny(a), parseAny(b)
	return reflect.DeepEqual(pa, pb)
}

func parseAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return v
}
