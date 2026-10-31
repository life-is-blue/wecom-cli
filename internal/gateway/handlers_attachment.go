package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/storage"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

const (
	uploadTTL   = 15 * time.Minute
	downloadTTL = 5 * time.Minute
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// blobs returns the storage backend, creating it from the environment on first use.
func (s *Server) blobs() (storage.Storage, error) {
	s.storOnce.Do(func() {
		if s.Storage == nil {
			s.Storage, s.storErr = storage.FromEnv(os.Getenv)
		}
	})
	if s.storErr != nil {
		return nil, s.storErr
	}
	return s.Storage, nil
}

// serveBlob serves the fs backend's signed /v1/blobs/ URLs (the signature is the credential).
func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	st, err := s.blobs()
	if err != nil {
		s.Log.Printf("storage unavailable: %v", err)
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	h, ok := st.(http.Handler)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

// absURL turns the fs backend's path-only URLs into absolute ones using the request's host.
func absURL(r *http.Request, u string) string {
	if !strings.HasPrefix(u, "/") {
		return u
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	return scheme + "://" + r.Host + u
}

func (s *Server) routeAttachments(q *request, wf int64) error {
	m, n := q.r.Method, len(q.segs)
	switch n {
	case 5:
		switch q.verb {
		case "initUpload":
			if m == http.MethodPost {
				return s.initUpload(q, wf)
			}
		case "":
			if m == http.MethodPost {
				return s.createAttachment(q, wf)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	case 6:
		id, ok := store.ParseID(store.PrefixAttach, q.segs[5])
		if !ok {
			return notFound("attachment")
		}
		switch q.verb {
		case "download":
			if m == http.MethodGet {
				return s.downloadAttachment(q, wf, id)
			}
		case "":
			if m == http.MethodGet {
				a, err := s.Store.GetAttachment(q.r.Context(), q.pr.project(), wf, id)
				if err != nil {
					return err
				}
				return s.respond(q, a)
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	}
	return notFound("route")
}

func (s *Server) liveWorkflow(q *request, wf int64) error {
	ok, err := s.Store.WorkflowExists(q.r.Context(), q.pr.project(), wf)
	if err != nil {
		return err
	}
	if !ok {
		return notFound("workflow")
	}
	return nil
}

func checkSize(size int64) error {
	if size < 1 {
		return invalid("INVALID_SIZE", "size must be at least 1 byte")
	}
	if size > storage.MaxObjectSize {
		return invalid("FILE_TOO_LARGE", "size %d exceeds the limit of %d bytes (100 MiB)", size, storage.MaxObjectSize)
	}
	return nil
}

func cleanFilename(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 255 {
		return "", invalid("INVALID_FILENAME", "filename must be 1-255 characters")
	}
	return storage.SanitizeFilename(name), nil
}

func (s *Server) initUpload(q *request, wf int64) error {
	var body api.InitUploadRequest
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	if _, err := cleanFilename(body.Filename); err != nil {
		return err
	}
	if err := checkSize(body.Size); err != nil {
		return err
	}
	sha := ""
	if body.Sha256 != nil && *body.Sha256 != "" {
		sha = strings.ToLower(*body.Sha256)
		if !sha256Re.MatchString(sha) {
			return invalid("INVALID_SHA256", "sha256 must be 64 lowercase hex characters")
		}
	}
	if err := s.liveWorkflow(q, wf); err != nil {
		return err
	}
	st, err := s.blobs()
	if err != nil {
		return err
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	key := q.pr.project().StorageKeyPrefix(wf) + hex.EncodeToString(rnd[:])
	ps, err := st.PresignPut(q.r.Context(), key, storage.PutOptions{Size: body.Size, SHA256: sha}, uploadTTL)
	if err != nil {
		return err
	}
	resp := api.InitUploadResponse{StorageKey: &key, UploadUrl: ptrOf(absURL(q.r, ps.URL)), ExpireTime: &ps.Expires}
	if len(ps.Headers) > 0 {
		h := ps.Headers
		resp.UploadHeaders = &h
	}
	return s.respond(q, resp)
}

func (s *Server) createAttachment(q *request, wf int64) error {
	var body api.Attachment
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	o, err := q.mutOpts(nil, nil, nil, nil, nil)
	if err != nil {
		return err
	}
	name, err := cleanFilename(body.Filename)
	if err != nil {
		return err
	}
	if err := checkSize(body.Size); err != nil {
		return err
	}
	sha := strings.ToLower(body.Sha256)
	if !sha256Re.MatchString(sha) {
		return invalid("INVALID_SHA256", "sha256 must be 64 lowercase hex characters")
	}
	p := q.pr.project()
	if !strings.HasPrefix(body.StorageKey, p.StorageKeyPrefix(wf)) || storage.ValidKey(body.StorageKey) != nil {
		return invalid("INVALID_STORAGE_KEY", "storage_key was not issued for this workflow; call initUpload first")
	}
	if err := s.liveWorkflow(q, wf); err != nil {
		return err
	}
	st, err := s.blobs()
	if err != nil {
		return err
	}
	obj, err := st.Stat(q.r.Context(), body.StorageKey)
	if errors.Is(err, storage.ErrNotFound) {
		return core.NewError(core.StatusFailedPrecondition, "UPLOAD_NOT_FOUND", "no object found for storage_key; upload the bytes to the URL from initUpload first")
	}
	if err != nil {
		return err
	}
	if obj.Size != body.Size {
		return core.NewError(core.StatusFailedPrecondition, "SIZE_MISMATCH", "uploaded object has %d bytes, request says %d", obj.Size, body.Size)
	}
	if obj.SHA256 != "" && obj.SHA256 != sha {
		return core.NewError(core.StatusFailedPrecondition, "SHA256_MISMATCH", "uploaded object does not match the supplied sha256")
	}
	if o.RequestID == "" { // a retried registration with a request_id replays instead
		if used, err := s.Store.StorageKeyRegistered(q.r.Context(), p, body.StorageKey); err != nil {
			return err
		} else if used {
			return core.NewError(core.StatusAlreadyExists, "STORAGE_KEY_REGISTERED", "storage_key is already registered as an attachment")
		}
	}
	plan := func(*store.Entity, core.Env) ([]store.PlannedChange, error) {
		return []store.PlannedChange{
			{Key: "on", New: core.JSON(wf)},
			{Key: "filename", New: strRaw(name)},
			{Key: "size", New: core.JSON(body.Size)},
			{Key: "sha256", New: strRaw(sha)},
			{Key: "storage_key", New: strRaw(body.StorageKey)},
		}, nil
	}
	res, err := s.apply(q, "attachment", 0, o, plan, timeZero)
	if err != nil {
		return err
	}
	return s.respond(q, s.Store.PreviewAttachment(p, wf, res.After))
}

func (s *Server) downloadAttachment(q *request, wf, id int64) error {
	a, err := s.Store.GetAttachment(q.r.Context(), q.pr.project(), wf, id)
	if err != nil {
		return err
	}
	st, err := s.blobs()
	if err != nil {
		return err
	}
	ps, err := st.PresignGet(q.r.Context(), a.StorageKey, a.Filename, downloadTTL)
	if err != nil {
		return err
	}
	return s.respond(q, api.DownloadAttachmentResponse{DownloadUrl: ptrOf(absURL(q.r, ps.URL)), ExpireTime: &ps.Expires})
}
