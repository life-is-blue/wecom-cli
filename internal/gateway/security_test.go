package gateway

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/storage"
)

// Two projects on one instance: an admin of A must not reach accounts that belong to B.
func TestCrossProjectAdminEscalation(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)
	baseA := "/v1/projects/" + a.project
	victim := "ed" + b.project // editor of B only
	bAdmin := "ad" + b.project

	// cannot link a foreign account into A ...
	for _, acct := range []string{victim, bAdmin} {
		a.wantErr(a.do(a.admin, "PUT", baseA+"/members/"+acct, map[string]any{"role": "ADMIN", "side": "DELIVERY"}), 403, "PERMISSION_DENIED", "ACCOUNT_IN_OTHER_PROJECT")
	}
	// ... and has no power over their tokens
	vt := "/v1/accounts/" + victim + "/tokens"
	a.wantErr(a.do(a.admin, "POST", vt, map[string]any{}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	a.wantErr(a.do(a.admin, "GET", vt, nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	a.wantErr(a.do(a.admin, "POST", vt+"/1:revoke", nil), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	// B's own admin still manages them
	b.wantOK(b.do(b.admin, "POST", vt, map[string]any{}))
	// the victim's token keeps working in B and gives nothing in A
	b.wantOK(b.do(b.editor, "GET", "/v1/me", nil))
	a.wantErr(a.do(b.editor, "GET", baseA+"/members", nil), 403, "PERMISSION_DENIED", "NOT_A_MEMBER")

	// a brand-new account can be added, and then its tokens are manageable
	a.wantOK(a.do(a.admin, "PUT", baseA+"/members/fresh"+a.project, map[string]any{"role": "EDITOR", "side": "DELIVERY"}))
	a.wantOK(a.do(a.admin, "POST", "/v1/accounts/fresh"+a.project+"/tokens", map[string]any{}))

	// an account of both projects: A's admin may edit its A role but not mint tokens for it
	shared := "shared" + a.project
	if _, err := a.st.CreateToken(context.Background(), a.project, shared, "delivery", "editor", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.CreateToken(context.Background(), b.project, shared, "delivery", "editor", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	a.wantOK(a.do(a.admin, "PUT", baseA+"/members/"+shared, map[string]any{"role": "VIEWER", "side": "DELIVERY"}))
	a.wantErr(a.do(a.admin, "POST", "/v1/accounts/"+shared+"/tokens", map[string]any{}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	// if the caller administers both projects it may
	if _, err := a.st.CreateToken(context.Background(), b.project, "ad"+a.project, "delivery", "admin", "human", time.Hour); err != nil {
		t.Fatal(err)
	}
	a.wantOK(a.do(a.admin, "POST", "/v1/accounts/"+shared+"/tokens", map[string]any{}))
}

func TestGlobalConfigGuard(t *testing.T) {
	f := newFixture(t)
	_ = newFixture(t) // a second project exists
	base := "/v1/projects/" + f.project + "/config"
	t.Setenv(EnvAllowGlobalConfig, "")
	f.wantErr(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"settings": map[string]string{"x.y": "1"}}), 403, "PERMISSION_DENIED", "GLOBAL_CONFIG_SHARED")
	f.wantErr(f.do(f.editor, "PATCH", base+"?update_mask=settings", map[string]any{}), 403, "PERMISSION_DENIED", "ROLE_INSUFFICIENT")
	f.wantOK(f.do(f.admin, "GET", base, nil)) // reading stays possible
	t.Setenv(EnvAllowGlobalConfig, "1")
	f.wantOK(f.do(f.admin, "PATCH", base+"?update_mask=settings", map[string]any{"settings": map[string]string{"x.y": ""}}))
}

func TestUploadURLReplayCannotOverwriteEvidence(t *testing.T) {
	f := withStorage(t, newFixture(t))
	wf := f.newWorkflow("replay_wf")
	data := []byte("original evidence")
	ini := f.wantOK(f.do(f.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "e.txt", "size": len(data), "sha256": sha(data)}))
	key, put := str(ini.body, "storage_key"), str(ini.body, "upload_url")
	if code, b := rawReq(t, "PUT", put, data); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	reg := map[string]any{"filename": "e.txt", "size": len(data), "sha256": sha(data), "storage_key": key}
	// the server hashes the bytes: a lying client hash is refused
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", map[string]any{"filename": "e.txt", "size": len(data), "sha256": sha([]byte("x")), "storage_key": key}), 400, "FAILED_PRECONDITION", "SHA256_MISMATCH")
	att := f.wantOK(f.do(f.editor, "POST", wf+"/attachments?request_id=r1", reg))
	// replaying the PUT URL after registration (same size, no sha pinned in the request) must not alter the blob
	evil := []byte("TAMPERED evidence")
	if code, _ := rawReq(t, "PUT", put, evil); code != 200 && code != 400 {
		t.Fatalf("unexpected PUT status %d", code)
	}
	dl := f.wantOK(f.do(f.viewer, "GET", "/v1/"+str(att.body, "name")+":download", nil))
	if code, b := rawReq(t, "GET", str(dl.body, "download_url"), nil); code != 200 || !bytes.Equal(b, data) {
		t.Fatalf("evidence changed: %d %q", code, b)
	}
	// the staged key itself is never downloadable
	// idempotent replay returns the first result; another request_id (or none) is a duplicate
	again := f.wantOK(f.do(f.editor, "POST", wf+"/attachments?request_id=r1", reg))
	if str(again.body, "name") != str(att.body, "name") {
		t.Fatalf("replay differs: %s vs %s", again.raw, att.raw)
	}
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments?request_id=r2", reg), 409, "ALREADY_EXISTS", "STORAGE_KEY_REGISTERED")
	f.wantErr(f.do(f.editor, "POST", wf+"/attachments", reg), 409, "ALREADY_EXISTS", "STORAGE_KEY_REGISTERED")
}

func TestPendingUploadQuotaAndCleanup(t *testing.T) {
	f := newFixture(t)
	fs, err := storage.NewFS(t.TempDir(), []byte("s"), "")
	if err != nil {
		t.Fatal(err)
	}
	g := New(f.st, nil)
	g.Storage = fs
	c := *f
	c.srv = httptest.NewServer(g)
	defer c.srv.Close()
	wf := c.newWorkflow("quota_wf")
	oldN, oldB := MaxPendingUploads, MaxPendingUploadBytes
	defer func() { MaxPendingUploads, MaxPendingUploadBytes = oldN, oldB }()
	MaxPendingUploads = 3
	var urls []string
	for i := 0; i < 3; i++ {
		r := c.wantOK(c.do(c.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "a", "size": 10}))
		urls = append(urls, str(r.body, "upload_url"))
	}
	c.wantErr(c.do(c.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "a", "size": 10}), 429, "RESOURCE_EXHAUSTED", "TOO_MANY_PENDING_UPLOADS")
	MaxPendingUploads = 100
	MaxPendingUploadBytes = 25
	c.wantErr(c.do(c.editor, "POST", wf+"/attachments:initUpload", map[string]any{"filename": "a", "size": 10}), 429, "RESOURCE_EXHAUSTED", "TOO_MANY_PENDING_UPLOADS")
	_ = urls

	// cleanup removes only stale staging objects
	ctx := context.Background()
	old, fresh := storage.StagingKey("1/1/old"), storage.StagingKey("1/1/fresh")
	for _, k := range []string{old, fresh} {
		p, _ := fs.PresignPut(ctx, k, storage.PutOptions{Size: 1}, time.Minute)
		if code, _ := rawReq(t, "PUT", c.srv.URL+p.URL, []byte("x")); code != 200 {
			t.Fatalf("put %d", code)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(fs.Root, filepath.FromSlash(old)), past, past); err != nil {
		t.Fatal(err)
	}
	n, err := g.CleanupStaging(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if _, err := fs.Stat(ctx, old); err != storage.ErrNotFound {
		t.Fatal("stale blob kept")
	}
	if _, err := fs.Stat(ctx, fresh); err != nil {
		t.Fatal("fresh blob removed")
	}
}

func TestAbsURLProxyTrust(t *testing.T) {
	r := httptest.NewRequest("GET", "http://gw.internal/x", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	t.Setenv(envPublicURL, "")
	t.Setenv(envTrustProxy, "")
	if got := absURL(r, "/v1/blobs/k"); got != "http://gw.internal/v1/blobs/k" {
		t.Fatalf("untrusted headers honoured: %s", got)
	}
	t.Setenv(envTrustProxy, "1")
	if got := absURL(r, "/v1/blobs/k"); got != "https://evil.example/v1/blobs/k" {
		t.Fatalf("trusted proxy headers ignored: %s", got)
	}
	t.Setenv(envPublicURL, "https://oe.example.com/")
	if got := absURL(r, "/v1/blobs/k"); got != "https://oe.example.com/v1/blobs/k" {
		t.Fatalf("public url ignored: %s", got)
	}
}
