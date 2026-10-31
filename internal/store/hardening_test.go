package store

import (
	"strings"
	"testing"

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

// AIP-160 and SQL disagree on AND/OR precedence, so a bare mix is refused rather than guessed.
func TestFilterRejectsAmbiguousMix(t *testing.T) {
	for _, f := range []string{"a=1 AND b=2 OR c=3", "a=1 OR b=2 AND c=3", "a=1 b=2 OR c=3", "(a=1 OR b=2 c=3)", "NOT a=1 OR b=2 AND c=3"} {
		if r := filterReason(t, f); r != "AMBIGUOUS_FILTER" {
			t.Errorf("%q: reason %q, want AMBIGUOUS_FILTER", f, r)
		}
	}
	for _, f := range []string{"a=1", "a=1 AND b=2 c=3", "a=1 OR b=2 OR c=3", "(a=1 AND b=2) OR c=3", "a=1 AND (b=2 OR c=3)", "NOT (a=1 OR b=2)"} {
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
