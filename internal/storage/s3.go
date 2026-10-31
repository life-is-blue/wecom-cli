package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config configures the S3-compatible backend. Secrets are never logged or returned.
type S3Config struct {
	Bucket, Region, Endpoint, AccessKeyID, SecretAccessKey, Prefix string
	PathStyle                                                      bool
}

// S3 implements Storage with SigV4 presigned URLs.
type S3 struct {
	bucket, prefix string
	cli            *s3.Client
	ps             *s3.PresignClient
}

// NewS3 builds the backend; no network access happens here.
func NewS3(c S3Config) (*S3, error) {
	if c.Bucket == "" || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("s3 storage needs OPENETL_S3_BUCKET, OPENETL_S3_ACCESS_KEY_ID and OPENETL_S3_SECRET_ACCESS_KEY")
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{Region: region, Credentials: credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""), UsePathStyle: c.PathStyle}
	if ep := strings.TrimSpace(c.Endpoint); ep != "" {
		opts.BaseEndpoint = aws.String(ep)
		opts.UsePathStyle = true
	}
	cli := s3.New(opts)
	return &S3{bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/"), cli: cli, ps: s3.NewPresignClient(cli)}, nil
}

func (s *S3) objKey(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if s.prefix != "" {
		return s.prefix + "/" + key, nil
	}
	return key, nil
}

// PresignPut implements Storage. The size is signed (Content-Length); a declared sha256 is
// signed as x-amz-meta-sha256 and must be sent by the client (see Presigned.Headers).
func (s *S3) PresignPut(ctx context.Context, key string, o PutOptions, ttl time.Duration) (*Presigned, error) {
	k, err := s.objKey(key)
	if err != nil {
		return nil, err
	}
	if o.Size <= 0 || o.Size > MaxObjectSize {
		return nil, errors.New("invalid size")
	}
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &k, ContentLength: aws.Int64(o.Size)}
	if o.SHA256 != "" {
		in.Metadata = map[string]string{"sha256": o.SHA256}
	}
	req, err := s.ps.PresignPutObject(ctx, in, func(po *s3.PresignOptions) { po.Expires = ttl })
	if err != nil {
		return nil, err
	}
	return &Presigned{URL: req.URL, Headers: clientHeaders(req.SignedHeader), Expires: time.Now().Add(ttl).UTC()}, nil
}

// PresignGet implements Storage.
func (s *S3) PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (*Presigned, error) {
	k, err := s.objKey(key)
	if err != nil {
		return nil, err
	}
	if IsStaging(key) {
		return nil, errors.New("staging objects cannot be downloaded")
	}
	cd := `attachment; filename="` + SanitizeFilename(filename) + `"`
	req, err := s.ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &k, ResponseContentDisposition: &cd},
		func(po *s3.PresignOptions) { po.Expires = ttl })
	if err != nil {
		return nil, err
	}
	return &Presigned{URL: req.URL, Expires: time.Now().Add(ttl).UTC()}, nil
}

// Stat implements Storage (HeadObject).
func (s *S3) Stat(ctx context.Context, key string) (*Object, error) {
	k, err := s.objKey(key)
	if err != nil {
		return nil, err
	}
	out, err := s.cli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	o := &Object{}
	if out.ContentLength != nil {
		o.Size = *out.ContentLength
	}
	o.SHA256 = out.Metadata["sha256"]
	return o, nil
}

// headETag returns the object's size and ETag.
func (s *S3) headETag(ctx context.Context, k string) (int64, string, error) {
	out, err := s.cli.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isS3NotFound(err) {
			return 0, "", ErrNotFound
		}
		return 0, "", err
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
	}
	return size, etag, nil
}

// verifyETag hashes the object version identified by etag (reads are pinned with If-Match, so a
// concurrent overwrite makes the read fail instead of hashing different bytes).
func (s *S3) verifyETag(ctx context.Context, k, etag string, size int64, sha string) error {
	in := &s3.GetObjectInput{Bucket: &s.bucket, Key: &k}
	if etag != "" {
		in.IfMatch = &etag
	}
	out, err := s.cli.GetObject(ctx, in)
	if err != nil {
		if isS3NotFound(err) {
			return ErrNotFound
		}
		return err
	}
	defer out.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(out.Body, MaxObjectSize+1))
	if err != nil {
		return err
	}
	if n != size {
		return ErrSizeMismatch
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return ErrHashMismatch
	}
	return nil
}

// Verify implements Storage; the digest is computed here from the stored bytes.
func (s *S3) Verify(ctx context.Context, stagingKey string, size int64, sha string) error {
	k, err := s.objKey(stagingKey)
	if err != nil {
		return err
	}
	n, etag, err := s.headETag(ctx, k)
	if err != nil {
		return err
	}
	if n != size {
		return ErrSizeMismatch
	}
	return s.verifyETag(ctx, k, etag, size, sha)
}

// Commit implements Storage: verify the pinned version, copy exactly that version (CopySourceIfMatch)
// to the final key, then delete the staging object.
func (s *S3) Commit(ctx context.Context, stagingKey, finalKey string, size int64, sha string) error {
	sk, err := s.objKey(stagingKey)
	if err != nil {
		return err
	}
	fk, err := s.objKey(finalKey)
	if err != nil {
		return err
	}
	if _, _, err := s.headETag(ctx, fk); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	n, etag, err := s.headETag(ctx, sk)
	if err != nil {
		return err
	}
	if n != size {
		return ErrSizeMismatch
	}
	if err := s.verifyETag(ctx, sk, etag, size, sha); err != nil {
		return err
	}
	in := &s3.CopyObjectInput{Bucket: &s.bucket, Key: &fk, CopySource: aws.String(s.bucket + "/" + (&url.URL{Path: sk}).EscapedPath()),
		MetadataDirective: types.MetadataDirectiveCopy}
	if etag != "" {
		in.CopySourceIfMatch = &etag
	}
	if _, err := s.cli.CopyObject(ctx, in); err != nil {
		return err
	}
	_, err = s.cli.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &sk})
	return err
}

// ListStaging implements Storage.
func (s *S3) ListStaging(ctx context.Context, prefix string) ([]StagedObject, error) {
	base := ""
	if s.prefix != "" {
		base = s.prefix + "/"
	}
	p := base + StagingPrefix + prefix
	var out []StagedObject
	var tok *string
	for {
		r, err := s.cli.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &p, ContinuationToken: tok})
		if err != nil {
			return nil, err
		}
		for _, o := range r.Contents {
			if o.Key == nil {
				continue
			}
			so := StagedObject{Key: strings.TrimPrefix(*o.Key, base)}
			if o.Size != nil {
				so.Size = *o.Size
			}
			if o.LastModified != nil {
				so.ModTime = *o.LastModified
			}
			out = append(out, so)
		}
		if r.IsTruncated == nil || !*r.IsTruncated || r.NextContinuationToken == nil {
			return out, nil
		}
		tok = r.NextContinuationToken
	}
}

// Delete implements Storage.
func (s *S3) Delete(ctx context.Context, key string) error {
	k, err := s.objKey(key)
	if err != nil {
		return err
	}
	_, err = s.cli.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &k})
	return err
}

func isS3NotFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}

// clientHeaders drops headers the HTTP client sets itself.
func clientHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		switch strings.ToLower(k) {
		case "host", "content-length":
			continue
		}
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
