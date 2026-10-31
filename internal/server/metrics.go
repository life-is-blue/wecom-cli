package server

// Hand-written observability, stdlib only: per-request counters and latency histograms, DB pool
// gauges, query-limiter rejections, import durations (GET /metrics, Prometheus text format) and one
// JSON access-log line per request. Everything exposed is aggregate: route templates (ids are
// replaced by placeholders), method, status class. No project names, account names or row data.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// EnvMetricsToken, when set, lets a caller outside loopback read /metrics with
// "Authorization: Bearer <token>". Without it only direct loopback connections are served.
const EnvMetricsToken = "OPENETL_METRICS_TOKEN"

var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}
var importBuckets = []float64{.1, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

type hist struct {
	bounds []float64
	counts []uint64 // per bucket (non-cumulative); the last slot is +Inf
	sum    float64
	n      uint64
}

func newHist(b []float64) *hist { return &hist{bounds: b, counts: make([]uint64, len(b)+1)} }

func (h *hist) observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	h.counts[i]++
	h.sum += v
	h.n++
}

func (h *hist) write(w io.Writer, name, labels string) {
	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i]
		fmt.Fprintf(w, "%s_bucket{%sle=\"%g\"} %d\n", name, labels, b, cum)
	}
	cum += h.counts[len(h.bounds)]
	fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", name, labels, cum)
	fmt.Fprintf(w, "%s_sum%s %g\n", name, braces(labels), h.sum)
	fmt.Fprintf(w, "%s_count%s %d\n", name, braces(labels), h.n)
}

func braces(labels string) string {
	if labels == "" {
		return ""
	}
	return "{" + strings.TrimSuffix(labels, ",") + "}"
}

type httpKey struct{ route, method, class string }

// Metrics is the in-process registry. The zero value is not usable; see newMetrics.
type Metrics struct {
	mu      sync.Mutex
	http    map[httpKey]*hist
	imports map[string]*hist // op (stage|apply|rollback) -> duration
	limiter map[string]uint64
	started time.Time
}

func newMetrics() *Metrics {
	return &Metrics{http: map[httpKey]*hist{}, imports: map[string]*hist{}, limiter: map[string]uint64{}, started: time.Now()}
}

// reqInfo travels with a request so the access log can name the authenticated account.
type reqInfo struct {
	id      string
	account string
}

type infoKey struct{}

// noteAccount records the authenticated account for the access log (called by ServeHTTP after auth).
func noteAccount(r *http.Request, account string) {
	if ri, ok := r.Context().Value(infoKey{}).(*reqInfo); ok {
		ri.account = account
	}
}

func withInfo(r *http.Request, ri *reqInfo) context.Context {
	return context.WithValue(r.Context(), infoKey{}, ri)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	reason string // AIP-193 reason of an error response, set by writeErr
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

var knownWords = map[string]bool{
	"projects": true, "accounts": true, "me": true, "workflows": true, "tasks": true, "importJobs": true,
	"members": true, "config": true, "comments": true, "attachments": true, "events": true, "blobs": true,
	"tokens": true, "stages": true, "reasons": true, "rules": true,
}

// routeLabel turns a request path into a bounded-cardinality template, e.g.
// /v1/projects/acme/workflows/wf-000012/attachments/7:download -> /v1/projects/{p}/workflows/{id}/attachments/{id}:download.
func routeLabel(path string) string {
	if path == "/healthz" || path == "/metrics" {
		return path
	}
	rest, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return "unmatched"
	}
	if strings.HasPrefix(rest, "blobs/") {
		return "/v1/blobs/{key}"
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	out := make([]string, 0, len(segs))
	for i, sg := range segs {
		verb := ""
		if j := strings.LastIndex(sg, ":"); j >= 0 && i == len(segs)-1 {
			sg, verb = sg[:j], ":"+sg[j+1:]
			if len(verb) > 24 {
				verb = ":other"
			}
		}
		switch {
		case i == 1 && segs[0] == "projects":
			sg = "{p}"
		case i >= 3 && i%2 == 1:
			sg = "{id}"
		case !knownWords[sg]:
			sg = "{x}"
		}
		out = append(out, sg+verb)
	}
	return "/v1/" + strings.Join(out, "/")
}

func statusClass(code int) string { return fmt.Sprintf("%dxx", code/100) }

func importOp(route, method string) string {
	switch {
	case method != http.MethodPost:
		return ""
	case route == "/v1/projects/{p}/importJobs":
		return "stage"
	case strings.HasSuffix(route, ":apply"):
		return "apply"
	case strings.HasSuffix(route, ":rollback"):
		return "rollback"
	}
	return ""
}

func (m *Metrics) record(route, method string, code int, d time.Duration, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := httpKey{route, method, statusClass(code)}
	h := m.http[k]
	if h == nil {
		h = newHist(latencyBuckets)
		m.http[k] = h
	}
	h.observe(d.Seconds())
	if op := importOp(route, method); op != "" && code < 400 {
		ih := m.imports[op]
		if ih == nil {
			ih = newHist(importBuckets)
			m.imports[op] = ih
		}
		ih.observe(d.Seconds())
	}
	if code == http.StatusTooManyRequests && strings.HasPrefix(reason, "QUERY_") {
		m.limiter[reason]++
	}
}

func newRequestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 64 {
		ok := true
		for _, c := range v {
			if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				ok = false
				break
			}
		}
		if ok {
			return v
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ServeHTTP wraps the router with request metrics, a request id and the JSON access log, and serves
// GET /metrics.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mxOnce.Do(func() { s.metrics = newMetrics() })
	if r.URL.Path == "/metrics" {
		s.serveMetrics(w, r)
		return
	}
	start := time.Now()
	ri := &reqInfo{id: newRequestID(r)}
	r = r.WithContext(withInfo(r, ri))
	w.Header().Set("X-Request-Id", ri.id)
	rec := &statusRecorder{ResponseWriter: w}
	s.serveHTTP(rec, r)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	d := time.Since(start)
	route := routeLabel(r.URL.Path)
	if rec.status == http.StatusNotFound && route != "/healthz" {
		route = "unmatched"
	}
	s.metrics.record(route, r.Method, rec.status, d, rec.reason)
	if s.AccessLog != nil && route != "/healthz" {
		line, _ := json.Marshal(map[string]any{
			"ts": start.UTC().Format(time.RFC3339Nano), "request_id": ri.id, "method": r.Method, "route": route,
			"status": rec.status, "duration_ms": float64(d.Microseconds()) / 1000, "account": ri.account,
		})
		s.logMu.Lock()
		_, _ = s.AccessLog.Write(append(line, '\n'))
		s.logMu.Unlock()
	}
}

// metricsAllowed: a direct loopback connection (no proxy headers), or the bearer token OPENETL_METRICS_TOKEN.
func metricsAllowed(r *http.Request) bool {
	if tok := os.Getenv(EnvMetricsToken); tok != "" {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
			return true
		}
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-Ip") != "" || r.Header.Get("Forwarded") != "" {
		return false // came through a proxy: the TCP peer is the proxy, not the caller
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !metricsAllowed(r) {
		http.Error(w, "forbidden: /metrics is loopback-only unless "+EnvMetricsToken+" is set and sent as a bearer token", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	s.metrics.write(&b)
	if s.Store != nil {
		poolStats(&b, "main", s.Store.DB)
		poolStats(&b, "query", s.Store.QueryDB)
	}
	_, _ = io.WriteString(w, b.String())
}

func poolStats(w io.Writer, pool string, db *sql.DB) {
	if db == nil {
		return
	}
	st := db.Stats()
	l := fmt.Sprintf("{pool=%q}", pool)
	fmt.Fprintf(w, "openetl_db_pool_open_connections%s %d\nopenetl_db_pool_in_use%s %d\nopenetl_db_pool_idle%s %d\n", l, st.OpenConnections, l, st.InUse, l, st.Idle)
	fmt.Fprintf(w, "openetl_db_pool_max_open%s %d\nopenetl_db_pool_wait_total%s %d\nopenetl_db_pool_wait_seconds_total%s %g\n", l, st.MaxOpenConnections, l, st.WaitCount, l, st.WaitDuration.Seconds())
}

func (m *Metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintf(w, "# HELP openetl_uptime_seconds Seconds since the process started.\n# TYPE openetl_uptime_seconds gauge\nopenetl_uptime_seconds %g\n", time.Since(m.started).Seconds())
	keys := make([]httpKey, 0, len(m.http))
	for k := range m.http {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.class < b.class
	})
	fmt.Fprintln(w, "# HELP openetl_http_requests_total Requests by route template, method and status class.\n# TYPE openetl_http_requests_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "openetl_http_requests_total{route=%q,method=%q,class=%q} %d\n", k.route, k.method, k.class, m.http[k].n)
	}
	fmt.Fprintln(w, "# HELP openetl_http_request_duration_seconds Request latency by route template, method and status class.\n# TYPE openetl_http_request_duration_seconds histogram")
	for _, k := range keys {
		m.http[k].write(w, "openetl_http_request_duration_seconds", fmt.Sprintf("route=%q,method=%q,class=%q,", k.route, k.method, k.class))
	}
	fmt.Fprintln(w, "# HELP openetl_import_duration_seconds Successful import operations (stage, apply, rollback), wall time.\n# TYPE openetl_import_duration_seconds histogram")
	ops := make([]string, 0, len(m.imports))
	for op := range m.imports {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		m.imports[op].write(w, "openetl_import_duration_seconds", fmt.Sprintf("op=%q,", op))
	}
	fmt.Fprintln(w, "# HELP openetl_query_limiter_rejections_total Query requests refused by the concurrency limiter.\n# TYPE openetl_query_limiter_rejections_total counter")
	rs := make([]string, 0, len(m.limiter))
	for r := range m.limiter {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	for _, r := range rs {
		fmt.Fprintf(w, "openetl_query_limiter_rejections_total{reason=%q} %d\n", r, m.limiter[r])
	}
}
