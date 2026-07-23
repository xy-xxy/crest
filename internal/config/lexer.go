package config

import "fmt"

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
	val    string
	line   int
	quoted bool
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
			toks = append(toks, token{tokOpenBrace, "{", line, false})
			i++
		case c == '}':
			toks = append(toks, token{tokCloseBrace, "}", line, false})
			i++
		case c == ';':
			toks = append(toks, token{tokSemicolon, ";", line, false})
			i++
		case c == '"' || c == '\'':
			quote := c
			start := line
			i++
			var sb []rune
			closed := false
			for i < len(rs) {
				ch := rs[i]
				if ch == '\\' && i+1 < len(rs) && (rs[i+1] == quote || rs[i+1] == '\\') {
					sb = append(sb, rs[i+1])
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
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated quoted string", start)
			}
			toks = append(toks, token{tokWord, string(sb), start, true})
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
			toks = append(toks, token{tokWord, string(rs[start:i]), line, false})
		}
	}
	toks = append(toks, token{tokEOF, "", line, false})
	return toks, nil
}
