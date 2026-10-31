package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

// The database collation is case-insensitive, so "Acme" would silently alias "acme"; ids that are not the
// documented lowercase slug must be refused when a project is first created.
func TestCreateTokenRejectsNonSlugProjectIDs(t *testing.T) {
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Acme", "bad slug;", "a", "1abc", "has_underscore", ""} {
		_, err := st.CreateToken(context.Background(), bad, "alice", "delivery", "viewer", "human", 0)
		ce, ok := err.(*core.Error)
		if !ok || ce.Reason != "INVALID_PROJECT_ID" {
			t.Errorf("project %q: want INVALID_PROJECT_ID, got %v", bad, err)
		}
	}
}
