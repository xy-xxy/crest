package config

import (
	"fmt"
	"strings"
)

type tokenType int

const (
	tokWord tokenType = iota
	tokOpenBrace
	tokCloseBrace
	tokSemicolon
	tokEOF
)

type token struct {
	typ    tokenType
	val    string // with nginx escapes resolved (nginxUnescape)
	line   int
	quoted bool
	// literal keeps backslashes as written, except \\ and an escaped closing
	// quote inside quotes. Path directives use it (see pathDirectives).
	literal string
}

// nginxUnescape resolves the escapes nginx's config reader handles in every
// argument, quoted or not (ngx_conf_read_token): \" \' and \\ yield the
// character, \t \r \n the control character. Any other backslash is kept, so
// regexes such as \d or \. reach the regex engine unchanged.
func nginxUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\'', '\\':
				b.WriteByte(s[i+1])
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func lex(src string) ([]token, error) {
	var toks []token
	line := 1
	rs := []rune(src)
	i := 0
	for i < len(rs) {
		c := rs[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '{':
			toks = append(toks, token{typ: tokOpenBrace, val: "{", line: line})
			i++
		case c == '}':
			toks = append(toks, token{typ: tokCloseBrace, val: "}", line: line})
			i++
		case c == ';':
			toks = append(toks, token{typ: tokSemicolon, val: ";", line: line})
			i++
		case c == '"' || c == '\'':
			quote := c
			start := line
			i++
			var sb, raw []rune // raw: the text between the quotes, as written
			closed := false
			for i < len(rs) {
				ch := rs[i]
				if ch == '\\' && i+1 < len(rs) && (rs[i+1] == quote || rs[i+1] == '\\') {
					sb = append(sb, rs[i+1])
					raw = append(raw, ch, rs[i+1])
					i += 2
					continue
				}
				if ch == quote {
					closed = true
					i++
					break
				}
				if ch == '\n' {
					line++
				}
				sb = append(sb, ch)
				raw = append(raw, ch)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated quoted string", start)
			}
			toks = append(toks, token{typ: tokWord, val: nginxUnescape(string(raw)), line: start, quoted: true, literal: string(sb)})
		default:
			start := i
			for i < len(rs) {
				ch := rs[i]
				if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' ||
					ch == ';' || ch == '{' || ch == '}' || ch == '#' {
					break
				}
				i++
			}
			word := string(rs[start:i])
			toks = append(toks, token{typ: tokWord, val: nginxUnescape(word), line: line, literal: word})
		}
	}
	toks = append(toks, token{typ: tokEOF, line: line})
	return toks, nil
}
