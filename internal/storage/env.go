package storage

import (
	"fmt"
	"strings"
)

// FromEnv selects the backend from OPENETL_STORAGE=fs|s3 (default fs).
//
//	fs: OPENETL_STORAGE_DIR (default ./data/blobs), OPENETL_STORAGE_SECRET (HMAC key; random per
//	    process when unset), OPENETL_PUBLIC_URL (absolute base for signed URLs; default derived
//	    from the request)
//	s3: OPENETL_S3_BUCKET, OPENETL_S3_REGION, OPENETL_S3_ENDPOINT, OPENETL_S3_ACCESS_KEY_ID,
//	    OPENETL_S3_SECRET_ACCESS_KEY, OPENETL_S3_PREFIX, OPENETL_S3_PATH_STYLE=true
func FromEnv(getenv func(string) string) (Storage, error) {
	switch strings.ToLower(strings.TrimSpace(getenv("OPENETL_STORAGE"))) {
	case "", "fs":
		dir := getenv("OPENETL_STORAGE_DIR")
		if dir == "" {
			dir = "./data/blobs"
		}
		return NewFS(dir, []byte(getenv("OPENETL_STORAGE_SECRET")), getenv("OPENETL_PUBLIC_URL"))
	case "s3":
		return NewS3(S3Config{
			Bucket: getenv("OPENETL_S3_BUCKET"), Region: getenv("OPENETL_S3_REGION"), Endpoint: getenv("OPENETL_S3_ENDPOINT"),
			AccessKeyID: getenv("OPENETL_S3_ACCESS_KEY_ID"), SecretAccessKey: getenv("OPENETL_S3_SECRET_ACCESS_KEY"),
			Prefix: getenv("OPENETL_S3_PREFIX"), PathStyle: strings.EqualFold(getenv("OPENETL_S3_PATH_STYLE"), "true"),
		})
	}
	return nil, fmt.Errorf("OPENETL_STORAGE must be fs or s3")
}
