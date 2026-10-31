package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

func filterReason(t *testing.T, filter string) string {
	t.Helper()
	_, _, err := compileFilter(filter, func(f, op, v string) (string, []any, error) { return "x = ?", []any{v}, nil })
	if err == nil {
		return ""
	}
	ce, ok := err.(*core.Error)
	if !ok {
		t.Fatalf("%q: %v", filter, err)
	}
	return ce.Reason
}

// There is no implicit AND: two conditions side by side are refused, and so is a bare AND/OR mix
// (AIP-160 and SQL disagree on precedence), rather than guessed.
func TestFilterRejectsAmbiguousMix(t *testing.T) {
	for _, f := range []string{"a=1 AND b=2 OR c=3", "a=1 OR b=2 AND c=3", "a=1 b=2 OR c=3", "(a=1 OR b=2 c=3)", "NOT a=1 OR b=2 AND c=3",
		"a=1 b=2", "a=1 (b=2 OR c=3)", "a=1 NOT b=2", "a=1 AND b=2 c=3", "(a=1) (b=2)", "a=1 ANDb=2"} {
		if r := filterReason(t, f); r != "AMBIGUOUS_FILTER" {
			t.Errorf("%q: reason %q, want AMBIGUOUS_FILTER", f, r)
		}
	}
	for _, f := range []string{"a=1", "a=1 AND b=2 AND c=3", "a=1 OR b=2 OR c=3", "(a=1 AND b=2) OR c=3", "a=1 AND (b=2 OR c=3)", "NOT (a=1 OR b=2)"} {
		if r := filterReason(t, f); r != "" {
			t.Errorf("%q: unexpected %q", f, r)
		}
	}
}

func TestPageTokenBoundAndSigned(t *testing.T) {
	b1 := listBinding(ListOpts{Filter: "a=1", OrderBy: "name"}, []string{"e.project_id = ?"}, []any{int64(1)})
	b2 := listBinding(ListOpts{Filter: "a=2", OrderBy: "name"}, []string{"e.project_id = ?"}, []any{int64(1)})
	b3 := listBinding(ListOpts{Filter: "a=1", OrderBy: "name"}, []string{"e.project_id = ?"}, []any{int64(2)})
	tok := sealPageToken("k", 42, b1)
	if k, n, err := openPageToken(tok, b1); err != nil || k != "k" || n != 42 {
		t.Fatalf("round trip: %v %v %v", k, n, err)
	}
	for name, b := range map[string]string{"other filter": b2, "other project": b3} {
		_, _, err := openPageToken(tok, b)
		if ce, ok := err.(*core.Error); !ok || ce.Reason != "INVALID_PAGE_TOKEN" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// tampered position, plain legacy token, garbage
	forged := strings.Replace(tok, tok[:4], "AAAA", 1)
	for _, bad := range []string{forged, "azo0Mg", "zzz", ""} {
		if _, _, err := openPageToken(bad, b1); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
}

// The same request_id with a different payload must not return the first result.
func TestIdempotencyKeyBoundToPayload(t *testing.T) {
	e := newEnv(t)
	mk := func(fp, name string) (*Result, error) {
		return e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "11111111-2222-4333-8444-555555555555",
			Fingerprint: fp, Actor: e.actor("delivery", "editor"), Plan: set("name", name)})
	}
	r1, err := mk("aaaa", "one")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mk("aaaa", "one")
	if err != nil || !r2.Replayed || r2.EventID != r1.EventID {
		t.Fatalf("same payload must replay: %+v %v", r2, err)
	}
	_, err = mk("bbbb", "two")
	coreErr(t, err, core.StatusAlreadyExists, "IDEMPOTENCY_KEY_REUSED")
	// validate_only stores nothing, so it cannot bind a key
	_, err = e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "22222222-2222-4333-8444-555555555555",
		Fingerprint: "cccc", ValidateOnly: true, Actor: e.actor("delivery", "editor"), Plan: set("name", "v")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Apply(e.ctx, ChangeRequest{ProjectID: e.p.ID, EntityType: "workflow", RequestID: "22222222-2222-4333-8444-555555555555",
		Fingerprint: "dddd", Actor: e.actor("delivery", "editor"), Plan: set("name", "v2")}); err != nil {
		t.Fatalf("a validate_only run must not reserve the key: %v", err)
	}
}

// Admin lists (members, tokens) use signed tokens bound to their query, like every other list.
func TestAdminPageTokensSignedAndBound(t *testing.T) {
	e := newEnv(t)
	oname := fmt.Sprintf("o%d-%d", time.Now().UnixNano()%1e9, seq.Add(1))
	if _, err := e.st.CreateToken(e.ctx, oname, "alice", "delivery", "editor", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	om, ok, err := e.st.MemberOf(e.ctx, oname, "alice")
	if err != nil || !ok {
		t.Fatalf("other project: %v %v", ok, err)
	}
	other := Project{ID: om.ProjectID, Name: oname}
	sfx := fmt.Sprintf("-%d", seq.Add(1))
	bob, carol := "adm-bob"+sfx, "adm-carol"+sfx
	for _, a := range []string{bob, carol, "adm-dave" + sfx} {
		if _, err := e.st.CreateToken(e.ctx, e.p.Name, a, "delivery", "editor", "human", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	ms, next, err := e.st.ListMembers(e.ctx, e.p, 2, "")
	if err != nil || len(ms) != 2 || next == "" {
		t.Fatalf("members page 1: %v %q %v", ms, next, err)
	}
	if _, _, err := e.st.ListMembers(e.ctx, e.p, 2, "YWxpY2U"); err == nil { // base64("alice"), the old unsigned format
		t.Error("unsigned members token accepted")
	}
	if _, _, err := e.st.ListMembers(e.ctx, other, 2, next); err == nil {
		t.Error("members token accepted for another project")
	}
	ms2, next2, err := e.st.ListMembers(e.ctx, e.p, 2, next)
	if err != nil || len(ms2) != 2 || next2 != "" {
		t.Fatalf("members page 2: %v %q %v", ms2, next2, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.st.CreateToken(e.ctx, e.p.Name, bob, "delivery", "editor", "human", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	ts, tnext, err := e.st.ListTokens(e.ctx, bob, 2, "")
	if err != nil || len(ts) != 2 || tnext == "" {
		t.Fatalf("tokens page 1: %v %q %v", ts, tnext, err)
	}
	if _, _, err := e.st.ListTokens(e.ctx, carol, 2, tnext); err == nil {
		t.Error("tokens token accepted for another account")
	}
	if _, _, err := e.st.ListTokens(e.ctx, bob, 2, "MQ"); err == nil { // base64("1")
		t.Error("unsigned tokens token accepted")
	}
	if _, _, err := e.st.ListTokens(e.ctx, bob, 2, tnext); err != nil {
		t.Errorf("own token rejected: %v", err)
	}
}
