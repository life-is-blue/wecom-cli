package storage

import (
	"context"
	"errors"
	"net/http"
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
