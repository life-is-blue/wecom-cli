package core

import (
	"encoding/json"
	"testing"
)

func ex(id int64, kv ...string) *ImportExisting {
	e := &ImportExisting{ID: id, Vals: map[string]json.RawMessage{}, Touched: map[string]bool{}, Platform: map[string]bool{}}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Vals[kv[i]] = JSON(kv[i+1])
		e.Touched[kv[i]] = true
	}
	return e
}

func TestClassify(t *testing.T) {
	a := ex(1, "name", "A", "source.system", "s", "source.ref", "a", "source.hash", "h1", "assignee.dev_owner", "bob", "attr.group", "g")
	a.Platform["assignee.dev_owner"] = true
	a.Vals["assignee.dev_owner"] = JSON("carol")
	b := ex(2, "name", "Plain", "attr.group", "g")
	del := ex(3, "name", "Gone", "source.system", "s", "source.ref", "gone", "source.hash", "x", "deleted", "")
	del.Vals["deleted"] = JSON(true)
	del.Deleted = true
	m := NewImportMatcher([]*ImportExisting{a, b, del})
	row := func(ref, hash, name string, keys ...string) ImportRow {
		r := ImportRow{System: "s", Ref: ref, Hash: hash, Keys: map[string]json.RawMessage{"name": JSON(name)}}
		for i := 0; i+1 < len(keys); i += 2 {
			r.Keys[keys[i]] = JSON(keys[i+1])
		}
		return r
	}
	cases := []struct {
		name string
		row  ImportRow
		base map[string]json.RawMessage
		want string
	}{
		{"same hash", row("a", "h1", "A"), nil, ActionUnchanged},
		{"source changed", row("a", "h2", "A2"), nil, ActionChanged},
		{"platform edit, source changed it too", row("a", "h2", "A", "assignee.dev_owner", "bob"), nil, ActionConflict},
		{"platform edit, source unchanged since base", row("a", "h2", "A", "assignee.dev_owner", "bob"), map[string]json.RawMessage{"assignee.dev_owner": JSON("bob")}, ActionChanged},
		{"unknown ref", row("zzz", "h", "A"), nil, ActionNew},
		{"unknown ref same name stays new", row("zzz", "h", "Plain", "attr.group", "g"), nil, ActionNew},
		{"no id, name+group match", ImportRow{System: "s", Hash: "h", Keys: map[string]json.RawMessage{"name": JSON("plain"), "attr.group": JSON("G")}}, nil, ActionSuspect},
		{"no id, other group", ImportRow{System: "s", Hash: "h", Keys: map[string]json.RawMessage{"name": JSON("Plain"), "attr.group": JSON("other")}}, nil, ActionNew},
		{"deleted in platform", row("gone", "h9", "Gone"), nil, ActionConflict},
		{"platform code wins", ImportRow{System: "s", Ref: "unrelated", Code: 2, Hash: "h", Keys: map[string]json.RawMessage{"name": JSON("Plain2")}}, nil, ActionChanged},
	}
	for _, c := range cases {
		if got := Classify(c.row, m, c.base); got.Action != c.want {
			t.Errorf("%s: %s, want %s (%+v)", c.name, got.Action, c.want, got.Changes)
		}
	}
	// platform-owned keys that were never set are filled, others never overwritten
	d := Classify(row("a", "h2", "A", "attr.priority", "p1", "stage.development", "pass"), m, nil)
	var fill, other int
	for _, c := range d.Changes {
		if c.Kind == ChangeFill {
			fill++
		} else if !IsSourceOwned(c.Key) {
			other++
		}
	}
	if fill != 2 || other != 0 {
		t.Errorf("changes %+v", d.Changes)
	}
	// missing
	miss := m.Missing("s", map[int64]bool{})
	if len(miss) != 1 || miss[0].ID != 1 {
		t.Errorf("missing %+v", miss)
	}
}
