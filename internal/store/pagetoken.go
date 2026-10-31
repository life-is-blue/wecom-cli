package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// EnvPageTokenKey names the secret that signs page tokens. When unset a random per-process key is
// used, so tokens do not survive a restart and are not valid across instances: multi-instance
// deployments must set it to the same value everywhere.
const EnvPageTokenKey = "OPENETL_PAGE_TOKEN_KEY"

var (
	pageKeyOnce sync.Once
	pageKeyVal  []byte
)

func pageKey() []byte {
	pageKeyOnce.Do(func() {
		if v := os.Getenv(EnvPageTokenKey); v != "" {
			pageKeyVal = []byte(v)
			return
		}
		pageKeyVal = make([]byte, 32)
		if _, err := rand.Read(pageKeyVal); err != nil {
			panic(err)
		}
	})
	return pageKeyVal
}

func badPageToken(msg string) error {
	return core.NewError(core.StatusInvalidArgument, "INVALID_PAGE_TOKEN", "%s", msg)
}

func bindHash(binding string) string {
	h := sha256.Sum256([]byte(binding))
	return hex.EncodeToString(h[:4])
}

func tokenMAC(payload string) string {
	m := hmac.New(sha256.New, pageKey())
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// sealPageToken builds a token "kind:n:hash(binding)" signed with an HMAC. The binding is a string
// describing the query (project, filter, order, SQL) the token may continue.
func sealPageToken(kind string, n int64, binding string) string {
	payload := kind + ":" + strconv.FormatInt(n, 10) + ":" + bindHash(binding)
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "." + tokenMAC(payload)))
}

// openPageToken verifies signature and binding and returns the kind and position.
func openPageToken(tok, binding string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", 0, badPageToken("invalid page_token")
	}
	payload, mac, ok := strings.Cut(string(raw), ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(tokenMAC(payload))) {
		return "", 0, badPageToken("invalid page_token")
	}
	parts := strings.Split(payload, ":")
	if len(parts) != 3 {
		return "", 0, badPageToken("invalid page_token")
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || n < 0 {
		return "", 0, badPageToken("invalid page_token")
	}
	if parts[2] != bindHash(binding) {
		return "", 0, badPageToken("page_token was issued for a different query (filter, order_by or parent); start again without it")
	}
	return parts[0], n, nil
}

func listBinding(o ListOpts, where []string, args []any) string {
	return fmt.Sprintf("list\x00%s\x00%s\x00%s\x00%v", o.Filter, o.OrderBy, strings.Join(where, " AND "), args)
}

func queryBinding(p Project, sqlText string) string {
	return fmt.Sprintf("query\x00%d\x00%s", p.ID, sqlText)
}
