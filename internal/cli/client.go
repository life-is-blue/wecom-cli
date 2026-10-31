package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/cli/credentials"
)

type resolved struct {
	base, token, tokenSource, project string
	// tokenErr is set when the saved token must not be sent to the effective base URL (see resolve).
	tokenErr error
}

func (a *App) store() *credentials.Store {
	home := a.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return credentials.New(credentials.DefaultDir(home), a.Keyring)
}

// resolve merges flags, environment and the saved credentials, in that order.
// Missing pieces are left empty; callers decide what they require.
func (a *App) resolve() (*resolved, error) {
	if a.resolved != nil {
		return a.resolved, nil
	}
	r := &resolved{
		base:    firstNonEmpty(a.flagBase, a.Getenv(EnvBaseURL)),
		project: firstNonEmpty(a.flagProject, a.Getenv(EnvProject)),
	}
	if t := a.Getenv(EnvToken); t != "" {
		r.token, r.tokenSource = t, "env"
	}
	if r.base == "" || r.project == "" || r.token == "" {
		c, err := a.store().Load()
		switch {
		case err == nil:
			r.base = firstNonEmpty(r.base, c.BaseURL)
			r.project = firstNonEmpty(r.project, c.Project)
			if r.token == "" && c.Token != "" {
				r.token, r.tokenSource = c.Token, "store"
				r.base = strings.TrimRight(r.base, "/")
				switch {
				case c.BaseURL == "":
					fmt.Fprintf(a.Stderr, "warning: the saved token is not bound to a base URL (saved by an older version); run `openetl auth login` again to bind it\n")
				case !sameBase(r.base, c.BaseURL):
					// never send the saved token to a URL it was not issued for (e.g. a redirected --base-url)
					r.token = ""
					r.tokenErr = usagef("the saved token belongs to %s, not %s; run `openetl auth login --base-url %s` for that URL (or pass the token explicitly via %s)",
						c.BaseURL, r.base, r.base, EnvToken)
				}
			}
		case errors.Is(err, credentials.ErrNotFound):
		case r.token == "":
			return nil, runtimef("%v", err)
		}
	}
	r.base = strings.TrimRight(r.base, "/")
	if r.tokenSource == "" {
		r.tokenSource = "none"
	}
	a.resolved = r
	return r, nil
}

// sameBase reports whether two base URLs have the same scheme, host and port.
func sameBase(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	return ea == nil && eb == nil && ua.Host != "" && ub.Host != "" && sameOrigin(ua, ub)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (a *App) projectID() (string, error) {
	r, err := a.resolve()
	if err != nil {
		return "", err
	}
	if r.project == "" {
		return "", usagef("project required: pass --project or set %s", EnvProject)
	}
	return r.project, nil
}

// call performs an authenticated JSON request and returns the response body.
func (a *App) call(method, path string, q url.Values, body any) ([]byte, error) {
	r, err := a.resolve()
	if err != nil {
		return nil, err
	}
	if r.tokenErr != nil {
		return nil, r.tokenErr
	}
	return a.callWith(r.base, r.token, method, path, q, body)
}

// EnvAllowHTTP=1 permits plain http to non-loopback hosts (bearer tokens would travel in clear text).
const EnvAllowHTTP = "OPENETL_ALLOW_HTTP"

func isLoopbackHost(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// checkTransport requires https for every non-loopback URL the CLI talks to, unless OPENETL_ALLOW_HTTP=1.
func (a *App) checkTransport(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) || a.Getenv(EnvAllowHTTP) == "1" {
			return nil
		}
		return usagef("refusing to use plain http for %s: use an https URL (or set %s=1 to accept the risk)", u.Host, EnvAllowHTTP)
	}
	return usagef("unsupported URL scheme %q", u.Scheme)
}

func effPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// sameOrigin reports whether scheme, host and port of u and base are all equal.
func sameOrigin(u, base *url.URL) bool {
	return base != nil && u.Scheme == base.Scheme && strings.EqualFold(u.Hostname(), base.Hostname()) && effPort(u) == effPort(base)
}

// httpFor returns the client to use for requests that carry the token: redirects to another origin
// (scheme, host or port) drop the Authorization header.
func (a *App) httpFor(base string) *http.Client {
	bu, _ := url.Parse(base)
	c := *a.HTTP
	prev := a.HTTP.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, bu) {
			req.Header.Del("Authorization")
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

func (a *App) callWith(base, token, method, path string, q url.Values, body any) ([]byte, error) {
	if base == "" {
		return nil, usagef("base URL required: pass --base-url, set %s, or run `openetl auth login`", EnvBaseURL)
	}
	if bu, err := url.Parse(base); err != nil || bu.Host == "" {
		return nil, usagef("invalid base URL %q", base)
	} else if err := a.checkTransport(bu); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, &cliError{Exit: 1, Code: 401, Status: "UNAUTHENTICATED",
			Message: "no token: set " + EnvToken + " or run `openetl auth login`"}
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, runtimef("encode request: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, rd)
	if err != nil {
		return nil, runtimef("bad request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "openetl-cli/"+a.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpFor(base).Do(req)
	if err != nil {
		return nil, runtimef("request failed: %v", scrub(err, token))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, runtimef("read response: %v", err)
	}
	if resp.StatusCode >= 400 {
		return nil, apiError(resp.StatusCode, data)
	}
	return data, nil
}

func scrub(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

// apiError converts an error response into a cliError, keeping the server's
// status and details. INVALID_ARGUMENT is a usage error (exit 2); all others
// exit 1.
func apiError(code int, data []byte) *cliError {
	ce := &cliError{Exit: 1, Code: code}
	var env struct {
		Error struct {
			Code    int             `json:"code"`
			Status  string          `json:"status"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && env.Error.Status != "" {
		ce.Status, ce.Message, ce.Details = env.Error.Status, env.Error.Message, env.Error.Details
		if env.Error.Code != 0 {
			ce.Code = env.Error.Code
		}
	} else {
		ce.Status = httpStatusName(code)
		ce.Message = strings.TrimSpace(string(data))
		if ce.Message == "" {
			ce.Message = http.StatusText(code)
		}
	}
	if ce.Status == "INVALID_ARGUMENT" {
		ce.Exit = 2
	}
	return ce
}

func httpStatusName(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 409:
		return "ABORTED"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 501:
		return "UNIMPLEMENTED"
	case 503:
		return "UNAVAILABLE"
	}
	if code >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}
