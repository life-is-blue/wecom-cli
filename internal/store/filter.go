package store

import (
	"strings"
	"unicode"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// resolver turns one restriction (field op value) into a SQL fragment with args.
type resolver func(field, op, value string) (string, []any, error)

func badFilter(format string, args ...any) error {
	return core.NewError(core.StatusInvalidArgument, "INVALID_FILTER", format, args...)
}

type filterParser struct {
	s       []rune
	i       int
	resolve resolver
}

// compileFilter parses an AIP-160 subset: restrictions joined by AND / OR (implicit AND), NOT, -, parentheses.
func compileFilter(filter string, r resolver) (string, []any, error) {
	if strings.TrimSpace(filter) == "" {
		return "", nil, nil
	}
	p := &filterParser{s: []rune(filter), resolve: r}
	sqlStr, args, err := p.parseOr()
	if err != nil {
		return "", nil, err
	}
	p.skipWS()
	if p.i < len(p.s) {
		return "", nil, badFilter("unexpected %q at position %d", string(p.s[p.i:]), p.i)
	}
	return sqlStr, args, nil
}

func (p *filterParser) skipWS() {
	for p.i < len(p.s) && unicode.IsSpace(p.s[p.i]) {
		p.i++
	}
}

func (p *filterParser) peekWord() string {
	p.skipWS()
	j := p.i
	for j < len(p.s) && !unicode.IsSpace(p.s[j]) && p.s[j] != '(' && p.s[j] != ')' {
		j++
	}
	return string(p.s[p.i:j])
}

func (p *filterParser) parseOr() (string, []any, error) {
	l, la, err := p.parseAnd()
	if err != nil {
		return "", nil, err
	}
	for strings.EqualFold(p.peekWord(), "OR") {
		p.i += 2
		r, ra, err := p.parseAnd()
		if err != nil {
			return "", nil, err
		}
		l, la = "("+l+" OR "+r+")", append(la, ra...)
	}
	return l, la, nil
}

func (p *filterParser) parseAnd() (string, []any, error) {
	l, la, err := p.parseUnary()
	if err != nil {
		return "", nil, err
	}
	for {
		w := p.peekWord()
		if w == "" || w == ")" || strings.EqualFold(w, "OR") {
			return l, la, nil
		}
		if strings.EqualFold(w, "AND") {
			p.skipWS()
			p.i += 3
		}
		r, ra, err := p.parseUnary()
		if err != nil {
			return "", nil, err
		}
		l, la = "("+l+" AND "+r+")", append(la, ra...)
	}
}

func (p *filterParser) parseUnary() (string, []any, error) {
	p.skipWS()
	if p.i >= len(p.s) {
		return "", nil, badFilter("unexpected end of filter")
	}
	w := p.peekWord()
	if strings.EqualFold(w, "NOT") {
		p.skipWS()
		p.i += 3
		sqlStr, args, err := p.parseUnary()
		return "NOT (" + sqlStr + ")", args, err
	}
	if p.s[p.i] == '-' {
		p.i++
		sqlStr, args, err := p.parseUnary()
		return "NOT (" + sqlStr + ")", args, err
	}
	if p.s[p.i] == '(' {
		p.i++
		sqlStr, args, err := p.parseOr()
		if err != nil {
			return "", nil, err
		}
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return "", nil, badFilter("missing closing parenthesis")
		}
		p.i++
		return "(" + sqlStr + ")", args, nil
	}
	return p.parseRestriction()
}

func (p *filterParser) parseRestriction() (string, []any, error) {
	p.skipWS()
	start := p.i
	for p.i < len(p.s) && !unicode.IsSpace(p.s[p.i]) && !strings.ContainsRune("=!<>:()", p.s[p.i]) {
		p.i++
	}
	field := string(p.s[start:p.i])
	if field == "" {
		return "", nil, badFilter("expected a field name at position %d", start)
	}
	p.skipWS()
	op := ""
	for _, cand := range []string{"<=", ">=", "!=", "=", "<", ">", ":"} {
		if strings.HasPrefix(string(p.s[p.i:min(p.i+2, len(p.s))]), cand) {
			op = cand
			break
		}
	}
	if op == "" {
		return "", nil, badFilter("expected an operator after %q", field)
	}
	p.i += len(op)
	p.skipWS()
	if p.i >= len(p.s) {
		return "", nil, badFilter("missing value after %q", field+" "+op)
	}
	var val string
	if q := p.s[p.i]; q == '"' || q == '\'' {
		p.i++
		var b strings.Builder
		for p.i < len(p.s) && p.s[p.i] != q {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			b.WriteRune(p.s[p.i])
			p.i++
		}
		if p.i >= len(p.s) {
			return "", nil, badFilter("unterminated string")
		}
		p.i++
		val = b.String()
	} else {
		st := p.i
		for p.i < len(p.s) && !unicode.IsSpace(p.s[p.i]) && p.s[p.i] != ')' {
			p.i++
		}
		val = string(p.s[st:p.i])
	}
	return p.resolve(field, op, val)
}

// cmpOp maps a filter operator to SQL for ordinary comparisons.
func cmpSQL(expr, op string) (string, error) {
	switch op {
	case "=":
		return expr + " = ?", nil
	case "!=":
		return "NOT (" + expr + " <=> ?)", nil
	case "<", "<=", ">", ">=":
		return expr + " " + op + " ?", nil
	}
	return "", badFilter("operator %q is not supported for this field", op)
}

func enumMap(v string, m map[string]string, field string) (string, error) {
	if out, ok := m[strings.ToUpper(v)]; ok {
		return out, nil
	}
	return "", badFilter("invalid value %q for %s", v, field)
}
