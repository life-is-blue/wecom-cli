package store

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

func (e *env) verifyLatest(t *testing.T) {
	t.Helper()
	total, first, err := e.st.VerifyLatest(e.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("entity_latest differs from delta in %d row(s): %v", total, first)
	}
}

// latestDigest fingerprints the whole cache table.
func (e *env) latestDigest(t *testing.T) string {
	t.Helper()
	var n int
	var sum string
	if err := e.st.DB.QueryRow(`SELECT COUNT(*), COALESCE(MD5(GROUP_CONCAT(CONCAT_WS('|', entity_id, k, event_id, IFNULL(CAST(v AS CHAR),'~'))
		ORDER BY entity_id, k SEPARATOR ';')),'') FROM entity_latest`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%s", n, sum)
}

func planKV(kv map[string]json.RawMessage) func(*Entity, core.Env) ([]PlannedChange, error) {
	return func(*Entity, core.Env) ([]PlannedChange, error) {
		var out []PlannedChange
		for k, v := range kv {
			out = append(out, PlannedChange{Key: k, New: v})
		}
		return out, nil
	}
}

// TestEntityLatestMatchesDelta applies a random mix of creates, updates, key removals,
// corrections (corrects_event_id) and soft deletes and checks that the cache equals a
// recomputation from delta, that the cached values are what the entity folds to, and that a
// rebuild is a no-op.
func TestEntityLatestMatchesDelta(t *testing.T) {
	e := newEnv(t)
	rng := rand.New(rand.NewSource(7))
	type ent struct {
		id  int64
		typ string
		ev  []int64
	}
	var ents []*ent
	keys := []string{"name", "attr.a", "attr.b", "attr.c", "assignee.dev_owner", "scope", "note.x"}
	apply := func(en *ent, typ string, changes map[string]json.RawMessage, corrects int64) {
		var id int64
		if en != nil {
			id = en.id
		}
		res, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: typ, EntityID: id,
			Actor: core.Actor{Account: e.acct, Side: "delivery", Role: "admin", Channel: core.ChannelLegacy}, CorrectsEvent: corrects, Plan: planKV(changes)})
		if err != nil {
			t.Fatal(err)
		}
		if res.NoOp {
			return
		}
		if en == nil {
			en = &ent{id: res.EntityID, typ: typ}
			ents = append(ents, en)
		}
		en.ev = append(en.ev, res.EventID)
	}
	for i := 0; i < 400; i++ {
		typ := []string{"workflow", "task", "comment"}[rng.Intn(3)]
		changes := map[string]json.RawMessage{}
		if len(ents) == 0 || rng.Intn(5) == 0 { // create
			changes["name"] = core.JSON(fmt.Sprintf("n%d", i))
			apply(nil, typ, changes, 0)
			continue
		}
		en := ents[rng.Intn(len(ents))]
		for j, n := 0, 1+rng.Intn(3); j < n; j++ {
			k := keys[rng.Intn(len(keys))]
			if k == "scope" {
				k = "attr.s" // keep scope rules out of the way: this test is about the cache
			}
			switch rng.Intn(4) {
			case 0:
				changes[k] = nil // remove the key (v = NULL in the cache)
			default:
				changes[k] = core.JSON(fmt.Sprintf("v%d", rng.Intn(5)))
			}
		}
		if rng.Intn(15) == 0 {
			changes["deleted"] = core.JSON(true)
		} else if rng.Intn(15) == 0 {
			changes["deleted"] = core.JSON(false)
		}
		var corrects int64
		if len(en.ev) > 0 && rng.Intn(4) == 0 { // a correction of an earlier event of the same entity
			corrects = en.ev[rng.Intn(len(en.ev))]
		}
		apply(en, en.typ, changes, corrects)
		if i%97 == 0 {
			e.verifyLatest(t)
		}
	}
	e.verifyLatest(t)

	// the folded entity agrees with the cache
	for _, en := range ents[:20] {
		got, err := loadEntity(e.ctx, e.st.DB, e.p.ID, en.typ, en.id)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := e.st.DB.Query(`SELECT k, v FROM entity_latest WHERE entity_id=? AND v IS NOT NULL`, en.id)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var k, v string
			rows.Scan(&k, &v)
			if string(got.Vals[k]) != v {
				t.Fatalf("entity %d key %s: loadEntity %s, cache %s", en.id, k, got.Vals[k], v)
			}
			n++
		}
		rows.Close()
		if n != len(got.Vals) {
			t.Fatalf("entity %d: %d cached values vs %d folded", en.id, n, len(got.Vals))
		}
	}

	// rebuild reproduces the same cache
	before := e.latestDigest(t)
	if _, err := e.st.RebuildLatest(e.ctx); err != nil {
		t.Fatal(err)
	}
	if after := e.latestDigest(t); after != before {
		t.Fatal("rebuild changed a cache that verified clean")
	}
}

// TestEntityLatestUntouchedByRollback: validate_only, rejected writes, no-ops and replays never move
// the cache; a damaged cache is reported by verify and repaired by rebuild.
func TestEntityLatestUntouchedByRollback(t *testing.T) {
	e := newEnv(t)
	id := e.createWF(t, "w")
	base := e.latestDigest(t)
	same := func(what string) {
		t.Helper()
		if got := e.latestDigest(t); got != base {
			t.Fatalf("%s changed entity_latest", what)
		}
	}
	actor := e.actor("delivery", "editor")
	// validate_only on an existing entity and as a create
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, ValidateOnly: true, Actor: actor,
		Plan: set("stage.development", "pass", "attr.x", "1")}); err != nil {
		t.Fatal(err)
	}
	same("validate_only update")
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", ValidateOnly: true, Actor: actor, Plan: set("name", "ghost")}); err != nil {
		t.Fatal(err)
	}
	same("validate_only create")
	// rejected by a rule after the plan ran
	_, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: actor, Plan: set("stage.verify", "pass")})
	coreErr(t, err, core.StatusFailedPrecondition, "STAGE_ORDER_VIOLATION")
	same("rejected write")
	// a stale etag
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: actor, Etag: "ev-1", Plan: set("name", "x")})
	coreErr(t, err, core.StatusAborted, "ETAG_MISMATCH")
	same("etag mismatch")
	// no-op
	if r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: actor, Plan: set("name", "w")}); err != nil || !r.NoOp {
		t.Fatalf("%+v %v", r, err)
	}
	same("no-op")
	// idempotent replay
	rid := "eeeeeeee-0000-4000-8000-0000000000e1"
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: actor, RequestID: rid, Plan: set("attr.y", "1")}); err != nil {
		t.Fatal(err)
	}
	base = e.latestDigest(t)
	if r, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", EntityID: id, Actor: actor, RequestID: rid, Plan: set("attr.y", "2")}); err != nil || !r.Replayed {
		t.Fatalf("%+v %v", r, err)
	}
	same("replay")
	e.verifyLatest(t)

	// a damaged cache is detected (all three kinds) and rebuild repairs it
	var ev int64
	e.st.DB.QueryRow(`SELECT event_id FROM entity_latest WHERE entity_id=? AND k='name'`, id).Scan(&ev)
	for _, q := range []string{
		fmt.Sprintf(`DELETE FROM entity_latest WHERE entity_id=%d AND k='name'`, id),
		fmt.Sprintf(`UPDATE entity_latest SET v='"zzz"' WHERE entity_id=%d AND k='attr.y'`, id),
		fmt.Sprintf(`INSERT INTO entity_latest(entity_id,k,v,event_id) VALUES (%d,'bogus','"1"',%d)`, id, ev),
	} {
		if _, err := e.st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	total, first, err := e.st.VerifyLatest(e.ctx, 10)
	if err != nil || total != 3 {
		t.Fatalf("verify: total %d err %v", total, err)
	}
	var kinds []string
	for _, m := range first {
		kinds = append(kinds, m.Problem)
	}
	if got := strings.Join(kinds, ","); !strings.Contains(got, "missing") || !strings.Contains(got, "extra") || !strings.Contains(got, "stale") {
		t.Fatalf("problems: %s", got)
	}
	if _, err := e.st.RebuildLatest(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.verifyLatest(t)
}
