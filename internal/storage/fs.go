package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BlobPrefix is the URL path under which FS serves objects.
const BlobPrefix = "/v1/blobs/"

// FS stores objects in a local directory. URLs are signed with HMAC-SHA256 over method, key,
// expiry and (for PUT) size and sha256; the server serves them itself via ServeHTTP.
type FS struct {
	Root    string
	Secret  []byte
	BaseURL string // optional absolute base (scheme://host); empty yields path-only URLs
	Now     func() time.Time
}

// NewFS creates the root directory. An empty secret is replaced by a random per-process one.
func NewFS(root string, secret []byte, baseURL string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, err
	}
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	return &FS{Root: abs, Secret: secret, BaseURL: strings.TrimRight(baseURL, "/")}, nil
}

func (f *FS) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *FS) sign(method, key string, exp int64, size int64, sha, fn string) string {
	m := hmac.New(sha256.New, f.Secret)
	fmt.Fprintf(m, "%s\n%s\n%d\n%d\n%s\n%s", method, key, exp, size, sha, fn)
	return hex.EncodeToString(m.Sum(nil))
}

func (f *FS) path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	p := filepath.Join(f.Root, filepath.FromSlash(key))
	rel, err := filepath.Rel(f.Root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid storage key")
	}
	return p, nil
}

func (f *FS) urlFor(key string, q url.Values) string {
	u := url.URL{Path: BlobPrefix + key, RawQuery: q.Encode()}
	return f.BaseURL + u.EscapedPath() + "?" + u.RawQuery
}

// PresignPut implements Storage.
func (f *FS) PresignPut(_ context.Context, key string, o PutOptions, ttl time.Duration) (*Presigned, error) {
	if _, err := f.path(key); err != nil {
		return nil, err
	}
	if o.Size <= 0 || o.Size > MaxObjectSize {
		return nil, errors.New("invalid size")
	}
	exp := f.now().Add(ttl).Unix()
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "size": {strconv.FormatInt(o.Size, 10)}}
	if o.SHA256 != "" {
		q.Set("sha256", o.SHA256)
	}
	q.Set("sig", f.sign(http.MethodPut, key, exp, o.Size, o.SHA256, ""))
	return &Presigned{URL: f.urlFor(key, q), Expires: time.Unix(exp, 0).UTC()}, nil
}

// PresignGet implements Storage.
func (f *FS) PresignGet(_ context.Context, key, filename string, ttl time.Duration) (*Presigned, error) {
	if _, err := f.path(key); err != nil {
		return nil, err
	}
	if IsStaging(key) {
		return nil, errors.New("staging objects cannot be downloaded")
	}
	fn := SanitizeFilename(filename)
	exp := f.now().Add(ttl).Unix()
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "fn": {fn}}
	q.Set("sig", f.sign(http.MethodGet, key, exp, 0, "", fn))
	return &Presigned{URL: f.urlFor(key, q), Expires: time.Unix(exp, 0).UTC()}, nil
}

// Stat implements Storage; it hashes the file (the digest is computed by the server).
func (f *FS) Stat(_ context.Context, key string) (*Object, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	return statHash(p)
}

func statHash(p string) (*Object, error) {
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return nil, err
	}
	return &Object{Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func checkObject(o *Object, size int64, sha string) error {
	if o.Size != size {
		return ErrSizeMismatch
	}
	if o.SHA256 != sha {
		return ErrHashMismatch
	}
	return nil
}

// Verify implements Storage.
func (f *FS) Verify(_ context.Context, stagingKey string, size int64, sha string) error {
	p, err := f.path(stagingKey)
	if err != nil {
		return err
	}
	o, err := statHash(p)
	if err != nil {
		return err
	}
	return checkObject(o, size, sha)
}

// Commit implements Storage: the staged file is first renamed to a private name (so a concurrent
// upload to the staging key creates a new file instead of changing the one being verified), then
// verified and hard-linked to the final key (link fails if it exists: committed blobs are immutable).
func (f *FS) Commit(_ context.Context, stagingKey, finalKey string, size int64, sha string) error {
	sp, err := f.path(stagingKey)
	if err != nil {
		return err
	}
	fp, err := f.path(finalKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(fp), ".commit-*")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := os.Rename(sp, tmp.Name()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	o, err := statHash(tmp.Name())
	if err == nil {
		err = checkObject(o, size, sha)
	}
	if err != nil {
		_ = os.Link(tmp.Name(), sp) // give the client its upload back (best effort; no overwrite)
		return err
	}
	if err := os.Link(tmp.Name(), fp); err != nil {
		_ = os.Link(tmp.Name(), sp)
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	return nil
}

// ListStaging implements Storage.
func (f *FS) ListStaging(_ context.Context, prefix string) ([]StagedObject, error) {
	root := filepath.Join(f.Root, "staging")
	var out []StagedObject
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(f.Root, p)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(strings.TrimPrefix(key, StagingPrefix), prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, StagedObject{Key: key, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	return out, err
}

// Delete implements Storage.
func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func blobErr(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// ServeHTTP serves PUT and GET on BlobPrefix after verifying the signature and expiry.
func (f *FS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, BlobPrefix)
	if !ok {
		blobErr(w, http.StatusNotFound, "not found")
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		blobErr(w, http.StatusForbidden, "invalid signature")
		return
	}
	var size int64
	var sha, fn string
	switch r.Method {
	case http.MethodPut:
		size, err = strconv.ParseInt(q.Get("size"), 10, 64)
		if err != nil || size <= 0 || size > MaxObjectSize {
			blobErr(w, http.StatusForbidden, "invalid signature")
			return
		}
		sha = q.Get("sha256")
	case http.MethodGet, http.MethodHead:
		fn = q.Get("fn")
	default:
		blobErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	want := f.sign(method, key, exp, size, sha, fn)
	if !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		blobErr(w, http.StatusForbidden, "invalid signature")
		return
	}
	if f.now().Unix() > exp {
		blobErr(w, http.StatusForbidden, "url expired")
		return
	}
	p, err := f.path(key)
	if err != nil {
		blobErr(w, http.StatusBadRequest, "invalid key")
		return
	}
	if method == http.MethodPut {
		f.put(w, r, p, size, sha)
		return
	}
	if IsStaging(key) {
		blobErr(w, http.StatusForbidden, "staging objects cannot be downloaded")
		return
	}
	fh, err := os.Open(p)
	if err != nil {
		blobErr(w, http.StatusNotFound, "not found")
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		blobErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+SanitizeFilename(fn)+`"; filename*=UTF-8''`+url.PathEscape(SanitizeFilename(fn)))
	http.ServeContent(w, r, "", st.ModTime(), fh)
}

func (f *FS) put(w http.ResponseWriter, r *http.Request, p string, size int64, sha string) {
	if r.ContentLength > size {
		blobErr(w, http.StatusRequestEntityTooLarge, "body larger than the signed size")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		blobErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		blobErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, size))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		blobErr(w, http.StatusRequestEntityTooLarge, "upload failed or body larger than the signed size")
		return
	}
	if n != size {
		blobErr(w, http.StatusBadRequest, "body size does not match the signed size")
		return
	}
	if sha != "" && hex.EncodeToString(h.Sum(nil)) != sha {
		blobErr(w, http.StatusBadRequest, "sha256 does not match")
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		blobErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	w.WriteHeader(http.StatusOK)
}
