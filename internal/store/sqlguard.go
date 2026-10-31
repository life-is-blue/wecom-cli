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

// The guard is an ALLOWLIST checker, not a blacklist: a statement is accepted only if it is a single
// SELECT whose FROM/JOIN items are bare public view names (optionally aliased), whose function calls
// are on a fixed list, and whose tokens belong to a small known alphabet. Anything it does not
// understand is rejected. Defence in depth: run queries over a SELECT-only DB user (OPENETL_QUERY_DSN,
// scripts/create-query-user.sql).

type sqlTok struct {
	kind       byte // 'w' word, 'q' backtick ident, 's' string, 'n' number, 'p' punct
	text       string
	start, end int
}

// deniedWords may not appear anywhere (also as column names; quote-free).
var deniedWords = map[string]bool{
	"INTO": true, "OUTFILE": true, "DUMPFILE": true, "FOR": true, "LOCK": true, "SET": true, "WITH": true,
	"INFORMATION_SCHEMA": true, "PERFORMANCE_SCHEMA": true, "MYSQL": true, "SYS": true,
	"HANDLER": true, "PROCEDURE": true, "LOAD": true, "EXEC": true, "EXECUTE": true, "PREPARE": true,
	"CALL": true, "DO": true, "UNION": true, "INTERSECT": true, "EXCEPT": true, "TABLE": true, "VALUES": true,
	"VALUE": true, "ROW": true, "LATERAL": true, "WINDOW": true, "OVER": true, "PARTITION": true,
	"USE": true, "IGNORE": true, "FORCE": true, "STRAIGHT_JOIN": true, "DUAL": true, "EXISTS": true,
	"ANY": true, "SOME": true, "RETURNING": true,
	// system information, even without parentheses
	"CURRENT_USER": true, "SESSION_USER": true, "SYSTEM_USER": true, "USER": true, "DATABASE": true, "SCHEMA": true,
	"VERSION": true, "CONNECTION_ID": true, "LAST_INSERT_ID": true, "ROW_COUNT": true, "FOUND_ROWS": true,
	"CURRENT_ROLE": true, "CATALOG": true,
}

// allowedFuncs are the only names that may be followed by '('. All are deterministic and side-effect free.
var allowedFuncs = map[string]bool{
	"COUNT": true, "SUM": true, "MIN": true, "MAX": true, "AVG": true, "COALESCE": true, "IFNULL": true, "IF": true,
	"CONCAT": true, "CONCAT_WS": true, "LOWER": true, "UPPER": true, "LENGTH": true, "SUBSTRING": true, "SUBSTR": true,
	"TRIM": true, "LTRIM": true, "RTRIM": true, "ROUND": true, "DATE": true, "DATE_FORMAT": true,
	"JSON_EXTRACT": true, "JSON_UNQUOTE": true, "JSON_VALUE": true, "JSON_LENGTH": true, "JSON_CONTAINS": true,
	"CAST": true, "NULLIF": true, "ABS": true, "FLOOR": true, "CEIL": true, "CEILING": true, "LEFT": true,
	"RIGHT": true, "REPLACE": true, "GREATEST": true, "LEAST": true, "YEAR": true, "MONTH": true, "DAY": true,
	"HOUR": true, "DATEDIFF": true, "GROUP_CONCAT": true,
}

// keywordsBeforeParen may be followed by '(' without being function calls.
var keywordsBeforeParen = map[string]bool{
	"IN": true, "AND": true, "OR": true, "NOT": true, "XOR": true, "ON": true, "USING": true, "WHERE": true,
	"HAVING": true, "WHEN": true, "THEN": true, "ELSE": true, "CASE": true, "BY": true, "AS": true, "LIKE": true,
	"REGEXP": true, "RLIKE": true, "BETWEEN": true, "IS": true, "DISTINCT": true, "ALL": true, "SELECT": true,
	"CHAR": true, "DECIMAL": true, "DATETIME": true, "TIME": true, "DIV": true, "MOD": true, "LIMIT": true,
	"OFFSET": true, "ASC": true, "DESC": true, "END": true,
}

// fromEnders end the FROM clause (depth 0).
var fromEnders = map[string]bool{"WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "OFFSET": true}

var aliasStoppers = map[string]bool{
	"WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "JOIN": true,
	"INNER": true, "LEFT": true, "RIGHT": true, "CROSS": true, "NATURAL": true, "FULL": true, "OUTER": true,
	"ON": true, "USING": true, "OFFSET": true,
}

const allowedPunct = "(),.*+-/%=<>!|&^~"

func invalidSQL(format string, args ...any) error {
	return core.NewError(core.StatusInvalidArgument, "INVALID_QUERY", format, args...)
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

func tokenizeSQL(s string) ([]sqlTok, error) {
	var out []sqlTok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-') || (c == '/' && i+1 < len(s) && s[i+1] == '*') || (c == '*' && i+1 < len(s) && s[i+1] == '/'):
			return nil, invalidSQL("comments are not allowed")
		case c == '"':
			return nil, invalidSQL("double-quoted strings are not allowed; use single quotes")
		case c == '\'':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' {
					return nil, invalidSQL("backslashes are not allowed in string literals")
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
			end := i + j + 2
			if end < len(s) && s[end] == '`' {
				return nil, invalidSQL("escaped backticks are not allowed")
			}
			out = append(out, sqlTok{'q', s[i+1 : i+1+j], i, end})
			i = end
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E') {
				j++
			}
			if j < len(s) && isWordByte(s[j]) {
				return nil, invalidSQL("malformed number")
			}
			out = append(out, sqlTok{'n', s[i:j], i, j})
			i = j
		case isWordByte(c):
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			out = append(out, sqlTok{'w', s[i:j], i, j})
			i = j
		case c == '@':
			return nil, invalidSQL("variables are not allowed")
		case c == ';':
			// only a single trailing ';' is tolerated
			if strings.TrimSpace(s[i+1:]) != "" {
				return nil, invalidSQL("only one statement is allowed")
			}
			i = len(s)
		case strings.IndexByte(allowedPunct, c) >= 0:
			out = append(out, sqlTok{'p', string(c), i, i + 1})
			i++
		default:
			return nil, invalidSQL("unexpected character %q", string(c))
		}
	}
	return out, nil
}

func isP(t sqlTok, p string) bool { return t.kind == 'p' && t.text == p }

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
	depth := 0
	seenFrom, inFrom := false, false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		up := strings.ToUpper(t.text)
		var prev, next sqlTok
		if i > 0 {
			prev = toks[i-1]
		}
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		hasNext := i+1 < len(toks)
		switch t.kind {
		case 'w':
			if deniedWords[up] {
				return "", invalidSQL("keyword %s is not allowed", up)
			}
			if i > 0 && up == "SELECT" {
				return "", invalidSQL("subqueries are not allowed")
			}
			if hasNext && isP(next, "(") {
				if isP(prev, ".") {
					return "", invalidSQL("qualified function calls are not allowed")
				}
				if !allowedFuncs[up] && !keywordsBeforeParen[up] {
					return "", invalidSQL("function %s is not allowed", up)
				}
			}
			if up == "FROM" || up == "JOIN" {
				if depth != 0 {
					return "", invalidSQL("%s is only allowed at the top level", up)
				}
				if up == "FROM" {
					if seenFrom {
						return "", invalidSQL("only one FROM clause is allowed")
					}
					seenFrom, inFrom = true, true
				} else if !seenFrom {
					return "", invalidSQL("JOIN without FROM")
				}
				// the next token must be a bare public view name
				if !hasNext || (next.kind != 'w' && next.kind != 'q') {
					return "", invalidSQL("expected a view name after %s (no parentheses or subqueries)", up)
				}
				name := strings.ToLower(next.text)
				if next.kind == 'w' && (deniedWords[strings.ToUpper(next.text)] || aliasStoppers[strings.ToUpper(next.text)]) {
					return "", invalidSQL("expected a view name after %s", up)
				}
				if i+2 < len(toks) && (isP(toks[i+2], ".") || isP(toks[i+2], "(")) {
					return "", invalidSQL("qualified table names are not allowed; query the public views only")
				}
				if !PublicViews[name] {
					return "", invalidSQL("unknown view %q; available: %s", next.text, strings.Join(sortedKeys(PublicViews), ", "))
				}
				// optional alias: [AS] ident
				j := i + 2
				hasAlias := false
				if j < len(toks) && toks[j].kind == 'w' && strings.EqualFold(toks[j].text, "AS") {
					if j+1 >= len(toks) || (toks[j+1].kind != 'w' && toks[j+1].kind != 'q') {
						return "", invalidSQL("expected an alias after AS")
					}
					hasAlias = true
					j += 2
				} else if j < len(toks) && toks[j].kind == 'q' {
					hasAlias = true
					j++
				} else if j < len(toks) && toks[j].kind == 'w' && !aliasStoppers[strings.ToUpper(toks[j].text)] {
					hasAlias = true
					j++
				}
				if hasAlias {
					a := toks[j-1]
					if a.kind == 'w' && (deniedWords[strings.ToUpper(a.text)] || strings.EqualFold(a.text, "SELECT") || strings.EqualFold(a.text, "FROM")) {
						return "", invalidSQL("invalid alias %q", a.text)
					}
					if j < len(toks) && (isP(toks[j], "(") || isP(toks[j], ".")) {
						return "", invalidSQL("unexpected token after table alias")
					}
				}
				sub := "(SELECT * FROM `" + name + "` WHERE project_id = " + strconv.FormatInt(projectID, 10) + ")"
				if !hasAlias {
					sub += " AS `" + name + "`"
				}
				if up == "JOIN" {
					if err := checkJoinCondition(toks, j); err != nil {
						return "", err
					}
				}
				repls = append(repls, repl{next.start, next.end, sub})
				i++ // the view name token is consumed (alias tokens are plain identifiers)
				continue
			}
			if fromEnders[up] && depth == 0 {
				inFrom = false
			}
		case 'q':
			if hasNext && isP(next, "(") {
				return "", invalidSQL("quoted function names are not allowed")
			}
		case 'p':
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth < 0 {
					return "", invalidSQL("unbalanced parentheses")
				}
			case ",":
				if inFrom && depth == 0 {
					return "", invalidSQL("comma joins are not allowed; use JOIN ... ON")
				}
			}
		}
	}
	if depth != 0 {
		return "", invalidSQL("unbalanced parentheses")
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

// joinEnders end the ON condition of a JOIN (top level).
var joinEnders = map[string]bool{
	"JOIN": true, "INNER": true, "LEFT": true, "RIGHT": true, "CROSS": true, "NATURAL": true, "FULL": true, "OUTER": true,
	"WHERE": true, "GROUP": true, "HAVING": true, "ORDER": true, "LIMIT": true, "OFFSET": true,
}

var cmpOps = map[string]bool{"=": true, "<": true, ">": true, "!": true}

func isConstTok(t sqlTok) bool {
	if t.kind == 'n' || t.kind == 's' {
		return true
	}
	if t.kind == 'w' {
		switch strings.ToUpper(t.text) {
		case "TRUE", "FALSE", "NULL":
			return true
		}
	}
	return false
}

// checkJoinCondition requires the JOIN whose view and alias end before toks[j] to carry a real
// condition: ON/USING is mandatory, and an ON that is constant-true (ON 1, ON true, ON 1=1,
// ... OR 1) would make a cartesian product that can exhaust the database.
func checkJoinCondition(toks []sqlTok, j int) error {
	if j >= len(toks) || toks[j].kind != 'w' {
		return invalidSQL("a JOIN needs an ON or USING clause on real columns (cartesian joins are not allowed)")
	}
	kw := strings.ToUpper(toks[j].text)
	if kw == "USING" {
		return nil
	}
	if kw != "ON" {
		return invalidSQL("a JOIN needs an ON or USING clause on real columns (cartesian joins are not allowed)")
	}
	k := j + 1
	depth := 0
	var cond []sqlTok
	for ; k < len(toks); k++ {
		t := toks[k]
		if t.kind == 'p' && t.text == "(" {
			depth++
		} else if t.kind == 'p' && t.text == ")" {
			depth--
		} else if depth == 0 && t.kind == 'w' && joinEnders[strings.ToUpper(t.text)] {
			break
		}
		cond = append(cond, t)
	}
	hasCol := false
	for i, t := range cond {
		if t.kind == 'q' || (t.kind == 'w' && !isConstTok(t) && !keywordsBeforeParen[strings.ToUpper(t.text)] && !allowedFuncs[strings.ToUpper(t.text)]) {
			hasCol = true
		}
		// constant comparison: const op const
		if isConstTok(t) && i+2 < len(cond) {
			o := i + 1
			if cond[o].kind == 'p' && cmpOps[cond[o].text] {
				o++
				if cond[o].kind == 'p' && cond[o].text == "=" {
					o++
				}
				if o < len(cond) && isConstTok(cond[o]) {
					return invalidSQL("constant ON conditions are not allowed; join on real columns")
				}
			}
		}
		// a constant standing alone as a boolean operand (ON 1, ... OR TRUE)
		if t.kind == 'n' || (t.kind == 'w' && (strings.EqualFold(t.text, "TRUE"))) {
			prevOK := i == 0 || (cond[i-1].kind == 'w' && (strings.EqualFold(cond[i-1].text, "OR") || strings.EqualFold(cond[i-1].text, "AND"))) || (cond[i-1].kind == 'p' && cond[i-1].text == "(")
			nextOK := i+1 >= len(cond) || (cond[i+1].kind == 'w' && (strings.EqualFold(cond[i+1].text, "OR") || strings.EqualFold(cond[i+1].text, "AND"))) || (cond[i+1].kind == 'p' && cond[i+1].text == ")")
			if prevOK && nextOK {
				return invalidSQL("constant ON conditions are not allowed; join on real columns")
			}
		}
	}
	if !hasCol {
		return invalidSQL("the ON condition must compare real columns")
	}
	return nil
}
