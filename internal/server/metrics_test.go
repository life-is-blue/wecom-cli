package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteLabel(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/projects/acme/workflows/wf-000012/attachments/7:download": "/v1/projects/{p}/workflows/{id}/attachments/{id}:download",
		"/v1/projects/acme:query":                                      "/v1/projects/{p}:query",
		"/v1/projects/acme/importJobs/5:apply":                         "/v1/projects/{p}/importJobs/{id}:apply",
		"/v1/blobs/a/b/c":                                              "/v1/blobs/{key}",
		"/v1/projects/acme/zzz-random":                                 "/v1/projects/{p}/{x}",
		"/nope":                                                        "unmatched",
	} {
		if got := routeLabel(in); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// No database needed: /healthz and unauthenticated calls exercise counters, the access log and the /metrics guard.
func TestMetricsEndpoint(t *testing.T) {
	var logbuf bytes.Buffer
	s := New(nil, nil)
	s.AccessLog = &logbuf
	srv := httptest.NewServer(s)
	defer srv.Close()

	get := func(path string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	get("/healthz", nil)
	if code, _ := get("/v1/projects/secret-project/workflows", nil); code != 401 { // no token
		t.Fatalf("want 401, got %d", code)
	}
	code, body := get("/metrics", nil) // httptest listens on loopback
	if code != 200 {
		t.Fatalf("metrics: %d %s", code, body)
	}
	for _, want := range []string{
		`openetl_http_requests_total{route="/healthz",method="GET",class="2xx"} 1`,
		`openetl_http_requests_total{route="/v1/projects/{p}/workflows",method="GET",class="4xx"} 1`,
		`openetl_http_request_duration_seconds_bucket{route="/healthz",method="GET",class="2xx",le="+Inf"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-project") {
		t.Error("metrics leaked a project name")
	}
	if code, _ := get("/metrics", map[string]string{"X-Forwarded-For": "203.0.113.9"}); code != 403 {
		t.Errorf("proxied /metrics should be refused, got %d", code)
	}
	t.Setenv(EnvMetricsToken, "tok")
	if code, _ := get("/metrics", map[string]string{"X-Forwarded-For": "203.0.113.9", "Authorization": "Bearer tok"}); code != 200 {
		t.Errorf("token should open /metrics, got %d", code)
	}
	var line map[string]any // the 401 line: JSON with request id, route, status, duration; no secrets
	for _, l := range strings.Split(strings.TrimSpace(logbuf.String()), "\n") {
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("access log line is not JSON: %q", l)
		}
	}
	if line["route"] != "/v1/projects/{p}/workflows" || line["status"].(float64) != 401 || line["request_id"] == "" || strings.Contains(logbuf.String(), "secret-project") {
		t.Errorf("unexpected access log line: %v", line)
	}
}
