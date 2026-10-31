package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidKey(t *testing.T) {
	for _, k := range []string{"1/2/abc", "a/b.c-d_e"} {
		if ValidKey(k) != nil {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []string{"", "../x", "a/../b", "/a", "a//b", "a/.hidden", "a/b c", `a\b`, "a/%2e%2e/b", strings.Repeat("a/", 9) + "a"} {
		if ValidKey(k) == nil {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func fsFixture(t *testing.T) (*FS, *httptest.Server, *time.Time) {
	t.Helper()
	now := time.Now()
	f, err := NewFS(t.TempDir(), []byte("test-secret"), "")
	if err != nil {
		t.Fatal(err)
	}
	f.Now = func() time.Time { return now }
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv, &now
}

func do(t *testing.T, method, u string, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Body.Close() })
	return r
}

func TestFSRoundTrip(t *testing.T) {
	f, srv, _ := fsFixture(t)
	ctx := context.Background()
	data := "hello blob"
	sum := sha256.Sum256([]byte(data))
	sha := hex.EncodeToString(sum[:])
	put, err := f.PresignPut(ctx, "1/2/obj", PutOptions{Size: int64(len(data)), SHA256: sha}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(put.URL, "/v1/blobs/1/2/obj?") {
		t.Fatal(put.URL)
	}
	if r := do(t, "PUT", srv.URL+put.URL, data); r.StatusCode != 200 {
		t.Fatalf("put: %d", r.StatusCode)
	}
	o, err := f.Stat(ctx, "1/2/obj")
	if err != nil || o.Size != int64(len(data)) || o.SHA256 != sha {
		t.Fatalf("stat %+v %v", o, err)
	}
	get, _ := f.PresignGet(ctx, "1/2/obj", `evil"name/../a.txt`, time.Minute)
	r := do(t, "GET", srv.URL+get.URL, "")
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), `attachment; filename="a.txt"`) {
		t.Fatalf("get: %d %v", r.StatusCode, r.Header)
	}
	if err := f.Delete(ctx, "1/2/obj"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Stat(ctx, "1/2/obj"); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestFSSignatureExpiryAndLimits(t *testing.T) {
	f, srv, now := fsFixture(t)
	ctx := context.Background()
	put, _ := f.PresignPut(ctx, "1/2/obj", PutOptions{Size: 4}, time.Minute)

	u, _ := url.Parse(put.URL)
	q := u.Query()
	q.Set("sig", strings.Repeat("0", 64))
	if r := do(t, "PUT", srv.URL+u.Path+"?"+q.Encode(), "abcd"); r.StatusCode != 403 {
		t.Errorf("bad sig: %d", r.StatusCode)
	}
	q = u.Query()
	q.Set("size", "99")
	if r := do(t, "PUT", srv.URL+u.Path+"?"+q.Encode(), "abcd"); r.StatusCode != 403 {
		t.Errorf("tampered size: %d", r.StatusCode)
	}
	if r := do(t, "PUT", srv.URL+"/v1/blobs/1/2/other?"+u.RawQuery, "abcd"); r.StatusCode != 403 {
		t.Errorf("other key: %d", r.StatusCode)
	}
	get, _ := f.PresignGet(ctx, "1/2/obj", "x", time.Minute)
	if r := do(t, "PUT", srv.URL+get.URL, "abcd"); r.StatusCode != 403 {
		t.Errorf("get url used for put: %d", r.StatusCode)
	}
	if r := do(t, "PUT", srv.URL+put.URL, "abcde"); r.StatusCode == 200 {
		t.Errorf("oversize body accepted")
	}
	if r := do(t, "PUT", srv.URL+put.URL, "abc"); r.StatusCode != 400 {
		t.Errorf("short body: %d", r.StatusCode)
	}
	if _, err := f.Stat(ctx, "1/2/obj"); err != ErrNotFound {
		t.Errorf("failed upload left an object: %v", err)
	}
	bad, _ := f.PresignPut(ctx, "1/2/obj", PutOptions{Size: 4, SHA256: strings.Repeat("a", 64)}, time.Minute)
	if r := do(t, "PUT", srv.URL+bad.URL, "abcd"); r.StatusCode != 400 {
		t.Errorf("sha mismatch: %d", r.StatusCode)
	}
	if r := do(t, "PUT", srv.URL+put.URL, "abcd"); r.StatusCode != 200 {
		t.Fatalf("valid put: %d", r.StatusCode)
	}
	*now = now.Add(2 * time.Minute)
	if r := do(t, "PUT", srv.URL+put.URL, "abcd"); r.StatusCode != 403 {
		t.Errorf("expired put: %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+get.URL, ""); r.StatusCode != 403 {
		t.Errorf("expired get: %d", r.StatusCode)
	}
}

func TestFSPathTraversal(t *testing.T) {
	f, srv, _ := fsFixture(t)
	ctx := context.Background()
	for _, k := range []string{"../etc/passwd", "a/../../x", "/abs", "a/./b"} {
		if _, err := f.PresignPut(ctx, k, PutOptions{Size: 1}, time.Minute); err == nil {
			t.Errorf("presign accepted %q", k)
		}
		if _, err := f.Stat(ctx, k); err == nil || err == ErrNotFound {
			t.Errorf("stat accepted %q", k)
		}
	}
	// even a correctly signed request for a bad key is refused by the handler
	exp := time.Now().Add(time.Minute).Unix()
	key := "../outside"
	sig := f.sign("GET", key, exp, 0, "", "x")
	r := do(t, "GET", srv.URL+"/v1/blobs/"+key+"?exp="+strconv.FormatInt(exp, 10)+"&fn=x&sig="+sig, "")
	if r.StatusCode == 200 {
		t.Error("traversal served")
	}
}

func TestS3PresignShape(t *testing.T) {
	s, err := NewS3(S3Config{Bucket: "bkt", Region: "eu-west-1", Endpoint: "https://s3.example.com", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secretsecret", Prefix: "pfx"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sha := strings.Repeat("ab", 32)
	p, err := s.PresignPut(ctx, "1/2/obj", PutOptions{Size: 10, SHA256: sha}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(p.URL)
	q := u.Query()
	if u.Host != "s3.example.com" || u.Path != "/bkt/pfx/1/2/obj" {
		t.Errorf("url %s", p.URL)
	}
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" || q.Get("X-Amz-Expires") != "300" || q.Get("X-Amz-Signature") == "" ||
		!strings.Contains(q.Get("X-Amz-Credential"), "AKIDEXAMPLE/") || !strings.Contains(q.Get("X-Amz-Credential"), "/eu-west-1/s3/") {
		t.Errorf("query %v", q)
	}
	if !strings.Contains(q.Get("X-Amz-SignedHeaders"), "content-length") || !strings.Contains(q.Get("X-Amz-SignedHeaders"), "x-amz-meta-sha256") {
		t.Errorf("signed headers %q", q.Get("X-Amz-SignedHeaders"))
	}
	if p.Headers["X-Amz-Meta-Sha256"] != sha {
		t.Errorf("headers %v", p.Headers)
	}
	if strings.Contains(p.URL, "secretsecret") {
		t.Error("secret leaked into URL")
	}
	g, err := s.PresignGet(ctx, "1/2/obj", "a.txt", time.Minute)
	if err != nil || !strings.Contains(g.URL, "response-content-disposition") {
		t.Errorf("get %v %v", g, err)
	}
	if _, err := s.PresignPut(ctx, "../x", PutOptions{Size: 1}, time.Minute); err == nil {
		t.Error("bad key accepted")
	}
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Error("missing creds accepted")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"OPENETL_STORAGE_DIR": t.TempDir()}
	get := func(k string) string { return env[k] }
	st, err := FromEnv(get)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*FS); !ok {
		t.Fatalf("%T", st)
	}
	env["OPENETL_STORAGE"] = "nope"
	if _, err := FromEnv(get); err == nil {
		t.Fatal("want error")
	}
}
