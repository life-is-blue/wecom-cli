// Package storage abstracts the blob store behind attachments. Bytes never pass through the API
// handlers: clients PUT to a presigned URL and GET from another one. Two backends exist:
// fs (local directory, URLs signed with HMAC and served by the gateway under /v1/blobs/) for
// development, tests and demos, and s3 (S3-compatible presigned URLs, SigV4).
package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxObjectSize is the per-file cap (100 MiB).
const MaxObjectSize int64 = 100 << 20

// ErrNotFound is returned by Stat/Delete when the object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Presigned is a time-limited URL plus headers the client must send with it.
type Presigned struct {
	URL     string
	Headers map[string]string
	Expires time.Time
}

// PutOptions describe the upload being authorised.
type PutOptions struct {
	Size   int64  // exact byte length (required)
	SHA256 string // optional lowercase hex; enforced by the backend where it can
}

// Object is what Stat reports. SHA256 is empty when the backend cannot tell.
type Object struct {
	Size   int64
	SHA256 string
}

// Storage is the blob backend.
type Storage interface {
	PresignPut(ctx context.Context, key string, o PutOptions, ttl time.Duration) (*Presigned, error)
	// PresignGet returns a download URL; filename is the suggested download name.
	PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (*Presigned, error)
	Stat(ctx context.Context, key string) (*Object, error)
	Delete(ctx context.Context, key string) error
}

var segRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidKey reports whether key is a safe relative object key: slash separated segments of
// [A-Za-z0-9._-], no empty, dot-leading or ".." segments, at most 8 segments.
func ValidKey(key string) error {
	if key == "" || len(key) > 512 {
		return fmt.Errorf("invalid storage key")
	}
	segs := strings.Split(key, "/")
	if len(segs) > 8 {
		return fmt.Errorf("invalid storage key")
	}
	for _, s := range segs {
		if !segRe.MatchString(s) || s == ".." {
			return fmt.Errorf("invalid storage key")
		}
	}
	return nil
}

// SanitizeFilename reduces a client filename to a safe download name.
func SanitizeFilename(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || r == '"' || r == ';' || r == '%':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 200 {
		out = out[:200]
	}
	if out == "" || out == "." || out == ".." {
		return "download"
	}
	return out
}
