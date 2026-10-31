package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/storage"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

const (
	uploadTTL   = 15 * time.Minute
	downloadTTL = 5 * time.Minute
	// stagingMaxAge is how long an unregistered upload may live before the cleanup deletes it.
	stagingMaxAge = time.Hour
)

// Per-project caps on pending (issued but not yet registered) uploads.
var (
	MaxPendingUploads     = 50
	MaxPendingUploadBytes = int64(1 << 30)
	envPublicURL          = "OPENETL_PUBLIC_URL"
	envTrustProxy         = "OPENETL_TRUST_PROXY"
)

// DefaultProjectStorageQuota is the default cap on the total size of a project's registered attachments.
const DefaultProjectStorageQuota = int64(10) << 30

const envProjectQuota = "OPENETL_PROJECT_STORAGE_QUOTA_BYTES"

func projectStorageQuota() int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(envProjectQuota)), 10, 64); err == nil && v > 0 {
		return v
	}
	return DefaultProjectStorageQuota
}

type reservation struct {
	size    int64
	expires time.Time
}

// uploadQuota tracks upload URLs issued by this process (an issued URL may be used up to its expiry,
// before any object exists) so the pending cap also covers uploads that have not started yet.
type uploadQuota struct {
	mu   sync.Mutex
	proj map[int64]map[string]reservation
}

// reserve registers key unless the project is at its cap; staged are the objects already in staging.
func (u *uploadQuota) reserve(project int64, key string, size int64, staged []storage.StagedObject, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.proj == nil {
		u.proj = map[int64]map[string]reservation{}
	}
	m := u.proj[project]
	if m == nil {
		m = map[string]reservation{}
		u.proj[project] = m
	}
	pending := map[string]int64{}
	for k, r := range m {
		if now.After(r.expires) {
			delete(m, k)
			continue
		}
		pending[k] = r.size
	}
	for _, o := range staged {
		pending[o.Key] = o.Size
	}
	var bytes int64
	for _, n := range pending {
		bytes += n
	}
	if len(pending) >= MaxPendingUploads || bytes+size > MaxPendingUploadBytes {
		return false
	}
	m[key] = reservation{size: size, expires: now.Add(uploadTTL)}
	return true
}

func (u *uploadQuota) release(project int64, key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.proj[project], key)
}

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

// absURL turns the fs backend's path-only URLs into absolute ones. OPENETL_PUBLIC_URL, when set,
// is the base; otherwise the request's own scheme and host are used, and X-Forwarded-Proto/Host
// are honoured only behind a trusted proxy (OPENETL_TRUST_PROXY=1).
func absURL(r *http.Request, u string) string {
	if !strings.HasPrefix(u, "/") {
		return u
	}
	if base := strings.TrimRight(os.Getenv(envPublicURL), "/"); base != "" {
		return base + u
	}
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if os.Getenv(envTrustProxy) == "1" {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" && !strings.ContainsAny(h, "/\\@ ,") {
			host = h
		}
	}
	return scheme + "://" + host + u
}

// CleanupStaging deletes staging uploads older than stagingMaxAge and returns how many it removed.
func (s *Server) CleanupStaging(ctx context.Context) (int, error) {
	st, err := s.blobs()
	if err != nil {
		return 0, err
	}
	list, err := st.ListStaging(ctx, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range list {
		if time.Since(o.ModTime) < stagingMaxAge {
			continue
		}
		if err := st.Delete(ctx, o.Key); err == nil {
			n++
		}
	}
	return n, nil
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
	stg := storage.StagingKey(key)
	pid := q.pr.project().ID
	staged, err := st.ListStaging(q.r.Context(), strconv.FormatInt(pid, 10)+"/")
	if err != nil {
		return err
	}
	if !s.quota.reserve(pid, stg, body.Size, staged, time.Now()) {
		return core.NewError(core.StatusResourceExhausted, "TOO_MANY_PENDING_UPLOADS",
			"too many pending uploads for this project (limit %d uploads / %d bytes); register or wait for them to expire", MaxPendingUploads, MaxPendingUploadBytes)
	}
	// the client PUTs to the staging key; registration commits the verified bytes to `key`
	ps, err := st.PresignPut(q.r.Context(), stg, storage.PutOptions{Size: body.Size, SHA256: sha}, uploadTTL)
	if err != nil {
		s.quota.release(pid, stg)
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
	stg := storage.StagingKey(body.StorageKey)
	// storage_key is unique per project; only a replay with the same request_id may reuse it
	registered, byThis, err := s.Store.StorageKeyRegisteredBy(q.r.Context(), p, body.StorageKey, o.RequestID)
	if err != nil {
		return err
	}
	if registered && !byThis {
		return core.NewError(core.StatusAlreadyExists, "STORAGE_KEY_REGISTERED", "storage_key is already registered as an attachment")
	}
	committed := false
	if !registered {
		used, err := s.Store.ProjectAttachmentBytes(q.r.Context(), p)
		if err != nil {
			return err
		}
		if limit := projectStorageQuota(); used+body.Size > limit {
			return core.NewError(core.StatusResourceExhausted, "PROJECT_STORAGE_QUOTA_EXCEEDED",
				"the project's attachment storage quota (%d bytes) would be exceeded (%d bytes already registered)", limit, used)
		}
		// the server hashes the staged bytes itself; the client's sha256 is only compared
		verr := error(nil)
		if o.ValidateOnly {
			verr = st.Verify(q.r.Context(), stg, body.Size, sha)
		} else {
			verr = st.Commit(q.r.Context(), stg, body.StorageKey, body.Size, sha)
			committed = verr == nil
		}
		switch {
		case errors.Is(verr, storage.ErrNotFound):
			return core.NewError(core.StatusFailedPrecondition, "UPLOAD_NOT_FOUND", "no object found for storage_key; upload the bytes to the URL from initUpload first")
		case errors.Is(verr, storage.ErrSizeMismatch):
			return core.NewError(core.StatusFailedPrecondition, "SIZE_MISMATCH", "uploaded object does not have the size in the request (%d bytes)", body.Size)
		case errors.Is(verr, storage.ErrHashMismatch):
			return core.NewError(core.StatusFailedPrecondition, "SHA256_MISMATCH", "uploaded object does not match the supplied sha256")
		case errors.Is(verr, storage.ErrExists):
			return core.NewError(core.StatusAlreadyExists, "STORAGE_KEY_REGISTERED", "storage_key is already registered as an attachment")
		case verr != nil:
			return verr
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
		if committed { // not registered: do not leave an orphan blob behind
			_ = st.Delete(context.WithoutCancel(q.r.Context()), body.StorageKey)
		}
		return err
	}
	s.quota.release(p.ID, stg)
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
