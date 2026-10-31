package core

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Reason kinds.
const (
	KindGateOverride  = "gate_override"
	KindDiscard       = "discard"
	KindNotApplicable = "not_applicable"
	KindReject        = "reject"
)

// Sides and roles.
const (
	SideDelivery = "delivery"
	SideCustomer = "customer"
	SideAny      = "any"
	SideSystem   = "system"

	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
)

// Channels. legacy and import are historical facts and bypass validation.
const (
	ChannelCLI    = "cli"
	ChannelAPI    = "api"
	ChannelImport = "import"
	ChannelLegacy = "legacy"
)

// Stage is one row of stage_def.
type Stage struct {
	Key            string
	Seq            int
	NameZh, NameEn string
}

// Reason is one row of the reason table.
type Reason struct {
	Key          string
	Kind         string
	OwnerSide    string
	NoteRequired bool
	Active       bool
}

// Rule is one row of the rule table.
type Rule struct {
	KPattern   string
	ToValue    *string
	Side       string
	ReasonKind *string
}

// Config is the project configuration relevant to validation.
type Config struct {
	Stages  []Stage // ordered by Seq
	Reasons map[string]Reason
	Rules   []Rule
}

// StageSeq returns the sequence of a stage key (0 when unknown).
func (c Config) StageSeq(key string) int {
	for _, s := range c.Stages {
		if s.Key == key {
			return s.Seq
		}
	}
	return 0
}

// Actor is who performs the write.
type Actor struct {
	Account string
	Side    string // delivery|customer|system
	Role    string // viewer|editor|admin
	Channel string
}

// Change is one key change inside an event. nil Old/New means "absent".
type Change struct {
	Key string
	Old json.RawMessage
	New json.RawMessage
}

// Str decodes a JSON string value (ok=false when absent or not a string).
func Str(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Bool decodes a JSON bool.
func Bool(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// Scalar renders a JSON scalar the way rules compare it: strings unquoted, others as text.
func Scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if s, ok := Str(raw); ok {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// JSON marshals v to RawMessage (panics never; values are plain).
func JSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// LikeMatch reports whether s matches the SQL LIKE pattern (% and _ wildcards, backslash escape).
func LikeMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("(?s)^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			} else {
				b.WriteString(regexp.QuoteMeta("\\"))
			}
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

// EscapeLike escapes %, _ and \ so that s matches literally inside a LIKE pattern.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
