package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// The embedded `openetl views` table is what Agents write queries from, so it must not drift from the real
// views. Needs a migrated database: OPENETL_TEST_DSN (skipped otherwise); the store tests migrate it.
func TestViewsJSONMatchesDatabase(t *testing.T) {
	dsn := os.Getenv("OPENETL_TEST_DSN")
	if dsn == "" {
		t.Skip("OPENETL_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var views []struct {
		Name    string `json:"name"`
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(viewsJSON, &views); err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		rows, err := db.Query(`SELECT COLUMN_NAME FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? ORDER BY ORDINAL_POSITION`, v.Name)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			var c string
			rows.Scan(&c)
			want = append(want, c)
		}
		rows.Close()
		var got []string
		for _, c := range v.Columns {
			got = append(got, c.Name)
		}
		if len(want) == 0 {
			t.Fatalf("view %s not found in the database (is it migrated?)", v.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("view %s: views.json has %v, database has %v", v.Name, got, want)
		}
	}
}
