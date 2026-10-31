// Command guardcheck runs store.GuardQuery (the server's query guard) on SQL snippets without a database
// or a server. Used by scripts/audit/audit.py.
//
//	stdin : one JSON object per line {"id": "...", "sql": "..."}
//	stdout: one JSON object per line {"id", "ok", "err", "reason", "rewritten"}
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

type in struct {
	ID  string `json:"id"`
	SQL string `json:"sql"`
}

type out struct {
	ID        string `json:"id"`
	OK        bool   `json:"ok"`
	Err       string `json:"err,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Rewritten string `json:"rewritten,omitempty"`
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var q in
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			fmt.Fprintln(os.Stderr, "bad input line:", err)
			os.Exit(2)
		}
		rew, err := store.GuardQuery(q.SQL, 1)
		o := out{ID: q.ID, OK: err == nil, Rewritten: rew}
		if err != nil {
			o.Err = err.Error()
			var ce *core.Error
			if errors.As(err, &ce) {
				o.Reason = ce.Reason
			}
		}
		_ = enc.Encode(o)
	}
}
