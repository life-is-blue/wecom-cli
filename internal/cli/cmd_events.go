package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

func (a *App) eventsCmd() *cobra.Command {
	return group("events", "Read the append-only audit events of a workflow", a.eventsList(), a.eventsGet())
}

func (a *App) eventsList() *cobra.Command {
	var l listFlags
	var asOf string
	cmd := &cobra.Command{
		Use: "list WF [--filter EXPR] [--as-of RFC3339] [--page-count N]", Short: "List audit events of a workflow",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			q := l.values("")
			if asOf != "" {
				t, err := time.Parse(time.RFC3339, asOf)
				if err != nil {
					return usagef("--as-of must be RFC 3339, e.g. 2026-09-30T00:00:00Z")
				}
				q.Set("as_of", t.UTC().Format(time.RFC3339))
			}
			return a.paginate(func(tok string) ([]byte, error) {
				if tok != "" {
					q.Set("page_token", tok)
				}
				return a.call("GET", base+"/events", q, nil)
			}, l.pageToken)
		},
	}
	l.bind(cmd, false)
	cmd.Flags().StringVar(&asOf, "as-of", "", "only events recorded (create_time) up to this instant")
	return cmd
}

func (a *App) eventsGet() *cobra.Command {
	return &cobra.Command{
		Use: "get WF EV", Short: "Get one audit event", Args: exactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			base, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			id := strings.ToLower(lastSegment(args[1]))
			if !idRe.MatchString(id) {
				return usagef("invalid event id %q", args[1])
			}
			out, err := a.call("GET", base+"/events/"+id, nil, nil)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
}

// ---------------------------------------------------------------- attachment

func (a *App) attachmentCmd() *cobra.Command {
	return group("attachment", "Download attachments (upload happens through `comment add --attach`)", a.attachmentGet())
}

func (a *App) attachmentGet() *cobra.Command {
	cmd := &cobra.Command{
		Use: "get WF AT --output FILE", Short: "Download an attachment to --output (the file bytes; stdout gets a JSON summary)",
		Args: exactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if a.output == "" {
				return usagef("--output FILE is required: attachment bytes are written to that file")
			}
			base, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			id := strings.ToLower(lastSegment(args[1]))
			if !idRe.MatchString(id) {
				return usagef("invalid attachment id %q", args[1])
			}
			raw, err := a.call("GET", base+"/attachments/"+id+":download", nil, nil)
			if err != nil {
				return err
			}
			var dl api.DownloadAttachmentResponse
			if json.Unmarshal(raw, &dl) != nil || dl.DownloadUrl == nil || *dl.DownloadUrl == "" {
				return runtimef("downloadAttachment returned no download_url")
			}
			n, sum, err := a.fetchTo(*dl.DownloadUrl, a.output)
			if err != nil {
				return err
			}
			return a.emitDirect(map[string]any{"output": a.output, "bytes": n, "sha256": sum})
		},
	}
	return cmd
}

// emitDirect prints JSON to stdout regardless of --output (which names a data file here).
func (a *App) emitDirect(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return runtimef("encode output: %v", err)
	}
	_, err = a.Stdout.Write(append(b, '\n'))
	return err
}

// fetchTo downloads a (presigned) URL into dst. The API token is only sent to
// the gateway's own host, never to a foreign storage host.
func (a *App) fetchTo(target, dst string) (int64, string, error) {
	r, err := a.resolve()
	if err != nil {
		return 0, "", err
	}
	u, err := url.Parse(target)
	if err != nil {
		return 0, "", runtimef("bad download_url: %v", err)
	}
	base, _ := url.Parse(r.base)
	if !u.IsAbs() && base != nil {
		u = base.ResolveReference(u)
	}
	if err := a.checkTransport(u); err != nil {
		return 0, "", err
	}
	// the API token goes only to the gateway's own origin (scheme, host and port all equal)
	sameHost := sameOrigin(u, base)
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", runtimef("download: %v", err)
	}
	req.Header.Set("User-Agent", "openetl-cli/"+a.Version)
	if sameHost && r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := a.httpFor(r.base).Do(req)
	if err != nil {
		return 0, "", runtimef("download failed: %v", scrub(err, r.token))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return 0, "", &cliError{Exit: 1, Code: resp.StatusCode, Status: "DOWNLOAD_FAILED",
			Message: "download failed: " + strings.TrimSpace(string(data))}
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", runtimef("write %s: %v", dst, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAttachment+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n > maxAttachment {
		err = runtimef("attachment exceeds %d bytes", maxAttachment)
	}
	if err != nil {
		os.Remove(tmp)
		if _, ok := err.(*cliError); ok {
			return 0, "", err
		}
		return 0, "", runtimef("download: %v", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 0, "", runtimef("write %s: %v", dst, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
