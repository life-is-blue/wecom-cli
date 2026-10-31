package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestQueryConcurrencyLimitHTTP(t *testing.T) {
	f := newFixture(t)
	f.st.QueryPerAccount, f.st.QueryTotal = 2, 4
	base := "/v1/projects/" + f.project + ":query"
	body := map[string]any{"sql": "SELECT 1"}
	// the viewer's account is at its cap: refused at once, with Retry-After, while others are served
	var rel []func()
	for i := 0; i < 2; i++ {
		r, err := f.st.AcquireQuerySlot("vi" + f.project)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	f.wantErr(f.do(f.viewer, "POST", base, body), 429, "RESOURCE_EXHAUSTED", "QUERY_TOO_MANY_CONCURRENT")
	req, _ := http.NewRequest("POST", f.srv.URL+base, strings.NewReader(`{"sql":"SELECT 1"}`))
	req.Header.Set("Authorization", "Bearer "+f.viewer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("want 429 + Retry-After, got %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	f.wantOK(f.do(f.editor, "POST", base, body))
	f.wantOK(f.do(f.viewer, "GET", "/v1/me", nil)) // other endpoints are unaffected
	// the global cap
	for i := 0; i < 2; i++ {
		r, err := f.st.AcquireQuerySlot("other-account")
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	f.wantErr(f.do(f.editor, "POST", base, body), 429, "RESOURCE_EXHAUSTED", "QUERY_SERVER_BUSY")
	for _, r := range rel {
		r()
	}
	f.wantOK(f.do(f.viewer, "POST", base, body))

	// parallel requests: only 200 or 429, and /v1/me stays responsive
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.do(f.viewer, "POST", base, map[string]any{"sql": "select count(*) from workflows a join workflows b on a.id = b.id"})
			mu.Lock()
			codes[r.code]++
			mu.Unlock()
		}()
	}
	f.wantOK(f.do(f.admin, "GET", "/v1/me", nil))
	wg.Wait()
	for c := range codes {
		if c != 200 && c != 429 {
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	if codes[200] == 0 {
		t.Fatalf("no query served: %v", codes)
	}
}

func TestQueryRejectsCartesianAndSystemInfo(t *testing.T) {
	f := newFixture(t)
	base := "/v1/projects/" + f.project + ":query"
	for _, q := range []string{
		"select count(*) from workflows a join workflows b on 1 join workflows c on 1",
		"select * from workflows a join workflows b",
		"select current_user", "select user()", "select database()", "select version()", "select @@version", "select schema",
	} {
		f.wantErr(f.do(f.viewer, "POST", base, map[string]any{"sql": q}), 400, "INVALID_ARGUMENT", "INVALID_QUERY")
	}
}

func TestQueryErrorDoesNotLeakNames(t *testing.T) {
	f := newFixture(t)
	r := f.do(f.viewer, "POST", "/v1/projects/"+f.project+":query", map[string]any{"sql": "select nope from workflows"})
	f.wantErr(r, 400, "INVALID_ARGUMENT", "QUERY_FAILED")
	if strings.Contains(r.raw, "nope") || strings.Contains(r.raw, "Unknown column") || strings.Contains(r.raw, "openetl_test") {
		t.Fatalf("leak: %s", r.raw)
	}
	if !strings.Contains(r.raw, "1054") {
		t.Fatalf("want the numeric code: %s", r.raw)
	}
}

func TestProjectStorageQuota(t *testing.T) {
	f := withStorage(t, newFixture(t))
	wf := f.newWorkflow("quota_wf")
	t.Setenv("OPENETL_PROJECT_STORAGE_QUOTA_BYTES", "100")
	reg := func(data []byte) resp {
		key := f.upload(wf, data)
		return f.do(f.editor, "POST", wf+"/attachments", map[string]any{"filename": "e.txt", "size": len(data), "sha256": sha(data), "storage_key": key})
	}
	f.wantOK(reg([]byte(strings.Repeat("a", 60))))
	f.wantErr(reg([]byte(strings.Repeat("b", 41))), 429, "RESOURCE_EXHAUSTED", "PROJECT_STORAGE_QUOTA_EXCEEDED")
	f.wantOK(reg([]byte(strings.Repeat("c", 40))))
	f.wantErr(reg([]byte("d")), 429, "RESOURCE_EXHAUSTED", "PROJECT_STORAGE_QUOTA_EXCEEDED")
	// a different project is not affected
	g := withStorage(t, newFixture(t))
	g.wantOK(func() resp {
		w := g.newWorkflow("quota_other")
		data := []byte(strings.Repeat("e", 90))
		return g.do(g.editor, "POST", w+"/attachments", map[string]any{"filename": "e.txt", "size": 90, "sha256": sha(data), "storage_key": g.upload(w, data)})
	}())
}
