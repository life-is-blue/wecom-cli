package importsrc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Row is one source row: column name to raw value (string, nil, json.Number,
// bool, or nested JSON for NDJSON sources).
type Row map[string]any

// text renders a raw value as the string form used for field mapping and the
// hash. ok is false for NULL.
func text(v any) (s string, ok bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case []byte:
		return string(x), true
	case json.Number:
		return x.String(), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprint(x), true
	default: // nested object / array: canonical JSON
		return canonicalJSON(x), true
	}
}

func canonicalJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // maps are encoded with sorted keys
	return strings.TrimRight(b.String(), "\n")
}

// Hash is the deterministic hash of the whole source row: sha256 over the
// canonical JSON object {column: string|null} with sorted keys, after
// removing excluded columns. Column order, value types (3 vs "3") and the
// source kind (CSV, NDJSON, MySQL) do not change the result.
func Hash(r Row, exclude []string) string {
	skip := map[string]bool{}
	for _, c := range exclude {
		skip[c] = true
	}
	norm := make(map[string]*string, len(r))
	keys := make([]string, 0, len(r))
	for k, v := range r {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
		if s, ok := text(v); ok {
			s := s
			norm[k] = &s
		} else {
			norm[k] = nil
		}
	}
	sort.Strings(keys)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(norm)
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}
