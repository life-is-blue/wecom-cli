package store_test

// Crash consistency: the real openetl-server binary runs against MySQL while concurrent writers hammer
// it; the process is killed with SIGKILL several times in the middle of the load and restarted.
// Afterwards (and after every kill): no event without deltas, entity_latest verifies clean, ids and
// old values chain, every acknowledged write (HTTP 200) is in the log exactly once, a request_id never
// produced more than one event, and replaying acknowledged requests returns the original result.
//
//	CHAOS_KILLS=3 CHAOS_ROUND_MS=1500 CHAOS_WRITERS=12   (scripts/chaos.sh turns these up)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/store"
)

type acked struct {
	key    string // request_id ("" when none)
	path   string
	method string
	body   string
	name   string // resource name answered
	label  string // for label writes: value that must exist in the log
}

func TestChaosKill9(t *testing.T) {
	st, dsn := freshStoreDSN(t)
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "openetl-server")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/openetl-server").CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	project := fmt.Sprintf("chaos%d", time.Now().UnixNano()%1e9)
	tok, err := st.CreateToken(ctx, project, "w", "delivery", "editor", "human", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/" + project

	var srv *exec.Cmd
	var url string
	start := func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		srv = exec.Command(bin, "serve")
		srv.Env = append(os.Environ(), "OPENETL_DB_DSN="+dsn, "OPENETL_ADDR="+addr, "OPENETL_ACCESS_LOG=off")
		srv.Stdout, srv.Stderr = io.Discard, io.Discard
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		url = "http://" + addr
		for i := 0; i < 100; i++ {
			if r, err := http.Get(url + "/healthz"); err == nil {
				r.Body.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("server did not come up")
	}
	kill := func() {
		srv.Process.Kill() // SIGKILL
		srv.Wait()
	}
	defer func() {
		if srv != nil && srv.ProcessState == nil {
			srv.Process.Kill()
			srv.Wait()
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path string, body any) (int, map[string]any, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, url+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		r, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(raw, &m)
		return r.StatusCode, m, nil
	}

	start()
	// a few shared workflows to contend on
	var shared []string
	for i := 0; i < 4; i++ {
		code, m, err := call("POST", base+"/workflows", map[string]any{"display_name": fmt.Sprintf("shared %d", i)})
		if err != nil || code != 200 {
			t.Fatalf("setup: %d %v %v", code, m, err)
		}
		shared = append(shared, m["name"].(string))
	}

	kills := envInt("CHAOS_KILLS", 3)
	roundMs := envInt("CHAOS_ROUND_MS", 1500)
	writers := envInt("CHAOS_WRITERS", 12)
	uuid := func(r *rand.Rand) string {
		b := make([]byte, 16)
		r.Read(b)
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	// a pool of request ids that many goroutines race on
	pool := make([]string, 16)
	prnd := rand.New(rand.NewSource(7))
	for i := range pool {
		pool[i] = uuid(prnd)
	}

	var mu sync.Mutex
	var acks []acked
	var attempts = map[string]bool{} // every request id ever sent
	record := func(a acked) { mu.Lock(); acks = append(acks, a); mu.Unlock() }

	for round := 0; round <= kills; round++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			w := w
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rand.New(rand.NewSource(int64(round*1000 + w)))
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					switch r.Intn(4) {
					case 0: // create workflow with its own request id
						key := uuid(r)
						mu.Lock()
						attempts[key] = true
						mu.Unlock()
						p := base + "/workflows?request_id=" + key
						b := map[string]any{"display_name": "w " + key[:8]}
						if code, m, err := call("POST", p, b); err == nil && code == 200 {
							record(acked{key: key, method: "POST", path: p, body: key, name: m["name"].(string)})
						}
					case 1: // same-entity contention: unique label value, must be in the log when acknowledged
						wf := shared[r.Intn(len(shared))]
						val := fmt.Sprintf("r%dw%dn%d", round, w, n)
						p := "/v1/" + wf + "?update_mask=labels.k"
						if code, _, err := call("PATCH", p, map[string]any{"workflow": map[string]any{"labels": map[string]string{"k": val}}}); err == nil && code == 200 {
							record(acked{label: val, name: wf})
						}
					case 2: // one request id from many goroutines: comment on a shared workflow
						key := pool[r.Intn(len(pool))]
						mu.Lock()
						attempts[key] = true
						mu.Unlock()
						wf := shared[int(key[0])%len(shared)]
						p := "/v1/" + wf + "/comments"
						b := map[string]any{"kind": "NOTE", "body": "note " + key}
						if code, m, err := call("POST", p+"?request_id="+key, b); err == nil && code == 200 {
							record(acked{key: key, method: "POST", path: p + "?request_id=" + key, body: "note " + key, name: m["name"].(string)})
						}
					default: // advance/regress dance on a shared workflow (fails often, that is fine)
						wf := shared[r.Intn(len(shared))]
						stage := []string{"development", "deployment", "verify"}[r.Intn(3)]
						call("POST", "/v1/"+wf+":advanceStage", map[string]any{"stage": stage})
						if r.Intn(3) == 0 {
							call("POST", "/v1/"+wf+":regressStage", map[string]any{"stages": []string{stage}, "reason": "rej_row_count"})
						}
					}
				}
			}()
		}
		time.Sleep(time.Duration(roundMs/2+rand.Intn(roundMs)) * time.Millisecond)
		if round < kills {
			kill() // in the middle of the load
		}
		close(stop)
		wg.Wait()
		if round < kills {
			requireCleanLog(t, st) // the database must be consistent right after the crash
			start()
		}
	}
	kill()
	requireCleanLog(t, st)

	// every acknowledged write is in the log; no request id wrote twice
	rows, err := st.DB.Query(`SELECT idempotency_key, COUNT(*) FROM event WHERE idempotency_key IS NOT NULL GROUP BY idempotency_key HAVING COUNT(*) > 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		t.Errorf("request_id %s produced %d events", k, n)
	}
	rows.Close()
	lost := 0
	for _, a := range acks {
		var n int
		if a.key != "" {
			st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE idempotency_key=?`, a.key).Scan(&n)
		} else {
			st.DB.QueryRow(`SELECT COUNT(*) FROM delta WHERE k='attr.k' AND CAST(new_v AS CHAR) = CONCAT('"', ?, '"')`, a.label).Scan(&n)
		}
		if n != 1 {
			lost++
			t.Errorf("acknowledged write %+v found %d times in the log", a, n)
		}
	}
	if len(acks) < 20 {
		t.Fatalf("only %d acknowledged writes: the load did not run", len(acks))
	}
	var events, deltas int
	st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&events)
	st.DB.QueryRow(`SELECT COUNT(*) FROM delta`).Scan(&deltas)
	t.Logf("%d kills, %d acknowledged writes (%d lost), %d events, %d deltas, %d distinct request ids", kills, len(acks), lost, events, deltas, len(attempts))

	// replays after the last restart return the original result and write nothing
	start()
	defer kill()
	replayed := 0
	for _, a := range acks {
		if a.key == "" || replayed >= 60 {
			continue
		}
		var before int
		st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&before)
		var b map[string]any
		if strings.Contains(a.path, "/comments") {
			b = map[string]any{"kind": "NOTE", "body": a.body}
		} else {
			b = map[string]any{"display_name": "w " + a.body[:8]}
		}
		method := a.method
		code, m, err := call(method, a.path, b)
		if err != nil || code != 200 || m["name"] != a.name {
			t.Errorf("replay of %s: %d %v %v, want 200 %s", a.key, code, m["name"], err, a.name)
		}
		var after int
		st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&after)
		if after != before {
			t.Errorf("replay of %s wrote %d new events", a.key, after-before)
		}
		replayed++
	}
}

var _ = store.EtagOf
