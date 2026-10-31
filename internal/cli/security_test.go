package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPSRequiredForRemoteBase(t *testing.T) {
	f := newFake(t)
	e := newEnv(t, f)
	e.vars["OPENETL_BASE_URL"] = "http://openetl.invalid:8080"
	code, _, errs := e.run("", "auth", "status")
	if code != 2 || !strings.Contains(errs, "plain http") {
		t.Fatalf("remote http base accepted: %d %s", code, errs)
	}
	e.vars["OPENETL_ALLOW_HTTP"] = "1"
	if code, _, errs = e.run("", "auth", "status"); strings.Contains(errs, "plain http") {
		t.Fatalf("OPENETL_ALLOW_HTTP ignored: %d %s", code, errs)
	}
	// loopback in all spellings is fine without the override
	for _, h := range []string{"localhost", "127.0.0.1", "127.9.9.9", "[::1]"} {
		u, _ := url.Parse("http://" + h + ":1")
		if !isLoopbackHost(u.Hostname()) {
			t.Errorf("%s is loopback", h)
		}
	}
	for _, h := range []string{"example.com", "10.0.0.1", "localhost.evil.com", "128.0.0.1"} {
		if isLoopbackHost(h) {
			t.Errorf("%s is not loopback", h)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	b, _ := url.Parse("https://gw.example.com")
	for u, want := range map[string]bool{
		"https://gw.example.com/x": true, "https://GW.example.com:443/x": true,
		"http://gw.example.com/x": false, "https://gw.example.com:8443/x": false, "https://other.example.com/x": false,
	} {
		pu, _ := url.Parse(u)
		if got := sameOrigin(pu, b); got != want {
			t.Errorf("%s: got %v want %v", u, got, want)
		}
	}
}

func TestTokenNotForwardedOnRedirectToOtherOrigin(t *testing.T) {
	var gotAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"account":"accounts/alice","kind":"HUMAN"}`)
	}))
	defer other.Close()
	f := newFake(t)
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		http.Redirect(w, r, other.URL+"/v1/me", http.StatusTemporaryRedirect) // same host, different port
		return true
	}
	e := newEnv(t, f)
	e.run("", "auth", "status")
	if gotAuth != "" {
		t.Fatalf("token leaked to another port: %q", gotAuth)
	}
}

func TestPresignedSamePortGetsNoTokenAcrossSchemes(t *testing.T) {
	var storageAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = r.Header.Get("Authorization")
		io.WriteString(w, "data")
	}))
	defer storage.Close()
	f := newFake(t)
	e := newEnv(t, f)
	// same hostname (127.0.0.1) as the gateway but another port: no bearer token
	f.h = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		reply(w, `{"download_url":"`+storage.URL+`/b"}`)
		return true
	}
	dst := t.TempDir() + "/x"
	if code, _, errs := e.run("", "attachment", "get", "WF-000123", "at-000009", "-o", dst); code != 0 || storageAuth != "" {
		t.Fatalf("%d %s auth=%q", code, errs, storageAuth)
	}
}
