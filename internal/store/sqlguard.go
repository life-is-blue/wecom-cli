package store

import (
	"strconv"
	"strings"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// PublicViews is the query surface: the only relations a user query may read.
var PublicViews = map[string]bool{
	"workflows": true, "tasks": true, "events": true, "comments": true,
	"workflow_stages": true, "stage_distribution": true, "open_items": true,
}

// TODO(M2): replace this hand-rolled conservative checker with a real SQL parser
// (e.g. vitess sqlparser / TiDB parser) and do the project rewrite on the AST.

type sqlTok struct {
	kind       byte // 'w' word, 'q' backtick ident, 's' string, 'n' number, 'p' punct
	text       string
	start, end int
}

var deniedWords = map[string]bool{
	"INTO": true, "OUTFILE": true, "DUMPFILE": true, "LOAD_FILE": true, "FOR": true, "LOCK": true,
	"SLEEP": true, "BENCHMARK": true, "GET_LOCK": true, "RELEASE_LOCK": true, "IS_FREE_LOCK": true,
	"IS_USED_LOCK": true, "MASTER_POS_WAIT": true, "INFORMATION_SCHEMA": true, "PERFORMANCE_SCHEMA": true,
	"MYSQL": true, "SYS": true, "HANDLER": true, "PROCEDURE": true, "LOAD": true, "SET": true, "WITH": true,
	"EXEC": true, "EXECUTE": true, "PREPARE": true, "CALL": true, "DO": true,
}

var fromEnders = map[string]bool{
	"WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "UNION": true,
	"WINDOW": true, "OFFSET": true, "INTERSECT": true, "EXCEPT": true,
}

var aliasStoppers = map[string]bool{
	"WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "UNION": true, "JOIN": true,
	"INNER": true, "LEFT": true, "RIGHT": true, "CROSS": true, "NATURAL": true, "STRAIGHT_JOIN": true,
	"FULL": true, "OUTER": true, "ON": true, "USING": true, "WINDOW": true, "OFFSET": true, "INTERSECT": true,
	"EXCEPT": true, "FOR": true, "USE": true, "IGNORE": true, "FORCE": true, "PARTITION": true,
}

func invalidSQL(format string, args ...any) error {
	return core.NewError(core.StatusInvalidArgument, "INVALID_QUERY", format, args...)
}

func tokenizeSQL(s string) ([]sqlTok, error) {
	var out []sqlTok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-') || (c == '/' && i+1 < len(s) && s[i+1] == '*'):
			return nil, invalidSQL("comments are not allowed")
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c { // doubled quote
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(s) {
				return nil, invalidSQL("unterminated string literal")
			}
			out = append(out, sqlTok{'s', s[i : j+1], i, j + 1})
			i = j + 1
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				return nil, invalidSQL("unterminated quoted identifier")
			}
			out = append(out, sqlTok{'q', s[i+1 : i+1+j], i, i + j + 2})
			i = i + j + 2
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E') {
				j++
			}
			out = append(out, sqlTok{'n', s[i:j], i, j})
			i = j
		case c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '$' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= '0' && s[j] <= '9') || s[j] >= 0x80) {
				j++
			}
			out = append(out, sqlTok{'w', s[i:j], i, j})
			i = j
		case c == '@':
			return nil, invalidSQL("variables are not allowed")
		case c == '\\':
			return nil, invalidSQL("unexpected backslash")
		case c == ';':
			// only a single trailing ';' is tolerated
			rest := strings.TrimSpace(s[i+1:])
			if rest != "" {
				return nil, invalidSQL("only one statement is allowed")
			}
			i = len(s)
		default:
			out = append(out, sqlTok{'p', string(c), i, i + 1})
			i++
		}
	}
	return out, nil
}

// GuardQuery verifies that sqlText is exactly one read-only SELECT over the public views and returns
// it rewritten so every view reference is restricted to the project (derived table + project_id filter).
func GuardQuery(sqlText string, projectID int64) (string, error) {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return "", invalidSQL("sql is empty")
	}
	if len(sqlText) > 20000 {
		return "", invalidSQL("sql is too long")
	}
	toks, err := tokenizeSQL(sqlText)
	if err != nil {
		return "", err
	}
	if len(toks) == 0 || toks[0].kind != 'w' || !strings.EqualFold(toks[0].text, "SELECT") {
		return "", invalidSQL("only a single SELECT statement is allowed")
	}
	type repl struct {
		start, end int
		text       string
	}
	var repls []repl
	inFrom := []bool{false}
	expect := false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		up := strings.ToUpper(t.text)
		d := len(inFrom) - 1
		if t.kind == 'w' && deniedWords[up] {
			return "", invalidSQL("keyword %s is not allowed", up)
		}
		if expect && (t.kind == 'w' || t.kind == 'q') {
			expect = false
			name := strings.ToLower(t.text)
			if i+1 < len(toks) && toks[i+1].kind == 'p' && toks[i+1].text == "." {
				return "", invalidSQL("qualified table names are not allowed; query the public views only")
			}
			if !PublicViews[name] {
				return "", invalidSQL("unknown view %q; available: %s", t.text, strings.Join(sortedKeys(PublicViews), ", "))
			}
			hasAlias := false
			if i+1 < len(toks) {
				n := toks[i+1]
				if n.kind == 'w' && (strings.EqualFold(n.text, "AS") || !aliasStoppers[strings.ToUpper(n.text)]) {
					hasAlias = true
				} else if n.kind == 'q' {
					hasAlias = true
				}
			}
			sub := "(SELECT * FROM `" + name + "` WHERE project_id = " + strconv.FormatInt(projectID, 10) + ")"
			if !hasAlias {
				sub += " AS `" + name + "`"
			}
			repls = append(repls, repl{t.start, t.end, sub})
			continue
		}
		expect = false
		switch {
		case t.kind == 'w' && up == "FROM":
			inFrom[d] = true
			expect = true
		case t.kind == 'w' && (up == "JOIN" || up == "STRAIGHT_JOIN"):
			expect = true
		case t.kind == 'p' && t.text == "," && inFrom[d]:
			expect = true
		case t.kind == 'w' && fromEnders[up]:
			inFrom[d] = false
		case t.kind == 'p' && t.text == "(":
			inFrom = append(inFrom, false)
		case t.kind == 'p' && t.text == ")":
			if len(inFrom) == 1 {
				return "", invalidSQL("unbalanced parentheses")
			}
			inFrom = inFrom[:len(inFrom)-1]
		}
	}
	if len(inFrom) != 1 {
		return "", invalidSQL("unbalanced parentheses")
	}
	if expect {
		return "", invalidSQL("expected a view name")
	}
	var b strings.Builder
	last := 0
	for _, r := range repls {
		b.WriteString(sqlText[last:r.start])
		b.WriteString(r.text)
		last = r.end
	}
	b.WriteString(sqlText[last:])
	return strings.TrimRight(strings.TrimSpace(b.String()), "; \n\t"), nil
}
