package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

const maxAttachment = 100 << 20

func (a *App) commentCmd() *cobra.Command {
	return group("comment", "Add, list, resolve, reopen and delete workflow comments",
		a.commentAdd(), a.commentList(), a.commentAction("resolve", "Mark a comment resolved"),
		a.commentAction("reopen", "Mark a resolved comment unresolved"), a.commentDelete())
}

// commentID accepts cm-000031 or a full comment resource name.
func commentID(in string) (string, error) {
	in = strings.TrimSpace(in)
	if i := strings.LastIndex(in, "comments/"); i >= 0 {
		in = in[i+len("comments/"):]
	}
	in = strings.ToLower(in)
	if !idRe.MatchString(in) {
		return "", usagef("invalid comment id %q", in)
	}
	return in, nil
}

func (a *App) commentPath(wf, cm string) (string, error) {
	base, err := a.itemPath("workflows", wf)
	if err != nil {
		return "", err
	}
	if cm == "" {
		return base + "/comments", nil
	}
	id, err := commentID(cm)
	if err != nil {
		return "", err
	}
	return base + "/comments/" + id, nil
}

func (a *App) commentAdd() *cobra.Command {
	var w writeFlags
	var kind, body string
	var attach []string
	cmd := &cobra.Command{
		Use: "add WF --body TEXT [--kind note|question|issue|risk] [--attach FILE]...", Short: "Add a comment, optionally with file attachments as evidence",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if body == "" {
				return usagef("--body is required")
			}
			k := api.CommentKind(strings.ToUpper(kind))
			switch k {
			case "NOTE", "QUESTION", "ISSUE", "RISK":
			default:
				return usagef("--kind must be note, question, issue or risk")
			}
			if w.etag != "" || w.reason != "" || w.note != "" {
				return usagef("--etag, --reason and --note are not supported by comment add (put the text in --body)")
			}
			base, err := a.commentPath(args[0], "")
			if err != nil {
				return err
			}
			rid, err := w.uuid()
			if err != nil {
				return err
			}
			files := make([]fileInfo, 0, len(attach))
			for _, p := range attach {
				fi, err := statAttachment(p)
				if err != nil {
					return err
				}
				files = append(files, fi)
			}
			var names []string
			if w.dryRun && len(files) > 0 {
				// Uploading writes data, so a dry run only validates the comment itself.
				a.warnf("--dry-run: %d attachment(s) not uploaded", len(files))
			} else {
				wfBase, _ := a.itemPath("workflows", args[0])
				for i, fi := range files {
					n, err := a.uploadAttachment(wfBase, fi, uuid.NewSHA1(rid, []byte("attachment:"+strconv.Itoa(i)+":"+fi.sha)))
					if err != nil {
						return err
					}
					names = append(names, n)
				}
			}
			c := api.Comment{Kind: k, Body: body}
			if len(names) > 0 {
				c.Attachments = &names
			}
			q := url.Values{"request_id": {rid.String()}}
			if w.dryRun {
				q.Set("validate_only", "true")
			}
			out, err := a.call("POST", base, q, c)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "note", "note, question, issue or risk")
	cmd.Flags().StringVar(&body, "body", "", "comment text (immutable after creation)")
	cmd.Flags().StringArrayVar(&attach, "attach", nil, "file to upload as an attachment (repeatable, max 100 MiB)")
	w.bind(cmd)
	return cmd
}

func (a *App) warnf(format string, v ...any) {
	b, _ := json.Marshal(map[string]string{"warning": strings.TrimSpace(fmt.Sprintf(format, v...))})
	_, _ = a.Stderr.Write(append(b, '\n'))
}

type fileInfo struct {
	path, name, sha string
	size            int64
}

func statAttachment(p string) (fileInfo, error) {
	f, err := os.Open(p)
	if err != nil {
		return fileInfo{}, usagef("--attach %s: %v", p, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return fileInfo{}, usagef("--attach %s: not a regular file", p)
	}
	if st.Size() < 1 || st.Size() > maxAttachment {
		return fileInfo{}, usagef("--attach %s: size must be 1 byte to 100 MiB", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileInfo{}, runtimef("read %s: %v", p, err)
	}
	return fileInfo{path: p, name: filepath.Base(p), sha: hex.EncodeToString(h.Sum(nil)), size: st.Size()}, nil
}

// uploadAttachment runs initUpload -> PUT bytes -> register metadata and
// returns the attachment resource name.
func (a *App) uploadAttachment(wfBase string, fi fileInfo, rid uuid.UUID) (string, error) {
	raw, err := a.call("POST", wfBase+"/attachments:initUpload", nil,
		api.InitUploadRequest{Filename: fi.name, Size: fi.size, Sha256: &fi.sha})
	if err != nil {
		return "", err
	}
	var init api.InitUploadResponse
	if err := json.Unmarshal(raw, &init); err != nil || init.UploadUrl == nil || init.StorageKey == nil {
		return "", runtimef("initUpload returned an unusable response")
	}
	if err := a.putFile(*init.UploadUrl, init.UploadHeaders, fi); err != nil {
		return "", err
	}
	raw, err = a.call("POST", wfBase+"/attachments", url.Values{"request_id": {rid.String()}},
		api.Attachment{Filename: fi.name, Size: fi.size, Sha256: fi.sha, StorageKey: *init.StorageKey})
	if err != nil {
		return "", err
	}
	var at api.Attachment
	if err := json.Unmarshal(raw, &at); err != nil || at.Name == nil {
		return "", runtimef("createAttachment returned no attachment name")
	}
	return *at.Name, nil
}

func (a *App) putFile(target string, headers *map[string]string, fi fileInfo) error {
	r, err := a.resolve()
	if err != nil {
		return err
	}
	u, err := url.Parse(target)
	if err != nil {
		return runtimef("bad upload_url: %v", err)
	}
	base, _ := url.Parse(r.base)
	if !u.IsAbs() && base != nil {
		u = base.ResolveReference(u)
	}
	if err := a.checkTransport(u); err != nil {
		return err
	}
	// the API token goes only to the gateway's own origin (scheme, host and port all equal)
	sameHost := sameOrigin(u, base)
	f, err := os.Open(fi.path)
	if err != nil {
		return runtimef("open %s: %v", fi.path, err)
	}
	defer f.Close()
	req, err := http.NewRequest(http.MethodPut, u.String(), f)
	if err != nil {
		return runtimef("upload: %v", err)
	}
	req.ContentLength = fi.size
	if headers != nil {
		for k, v := range *headers {
			req.Header.Set(k, v)
		}
	}
	// Presigned URLs on a foreign host must not receive the API token.
	if sameHost && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := a.httpFor(r.base).Do(req)
	if err != nil {
		return runtimef("upload %s: %v", fi.name, scrub(err, r.token))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return &cliError{Exit: 1, Code: resp.StatusCode, Status: "UPLOAD_FAILED",
			Message: "upload of " + fi.name + " failed: " + strings.TrimSpace(string(data))}
	}
	return nil
}

func (a *App) commentList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use: "list WF [--filter 'resolved = false'] [--page-count N]", Short: "List comments of a workflow",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.commentPath(args[0], "")
			if err != nil {
				return err
			}
			return a.listCall(path, &l, "")
		},
	}
	l.bind(cmd, false)
	return cmd
}

func (a *App) commentAction(verb, short string) *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: verb + " WF CM", Short: short, Args: exactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.commentPath(args[0], args[1])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			out, err := a.call("POST", path+":"+verb, nil, o)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	w.bind(cmd)
	return cmd
}

func (a *App) commentDelete() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "delete WF CM", Short: "Soft-delete a comment", Args: exactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if w.etag != "" || w.reason != "" || w.note != "" {
				return usagef("--etag, --reason and --note are not supported by comment delete")
			}
			path, err := a.commentPath(args[0], args[1])
			if err != nil {
				return err
			}
			q, err := w.query(false)
			if err != nil {
				return err
			}
			out, err := a.call("DELETE", path, q, nil)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	w.bind(cmd)
	return cmd
}
