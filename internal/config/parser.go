package config

import "fmt"

// Parse parses nginx-style configuration text into a directive tree.
func Parse(src string) ([]Directive, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	ds, err := p.parseBlock(false)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		ds = []Directive{}
	}
	return ds, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.typ != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) parseBlock(inBlock bool) ([]Directive, error) {
	var out []Directive
	var words []token
	for {
		t := p.next()
		switch t.typ {
		case tokEOF:
			if len(words) > 0 {
				return nil, fmt.Errorf(`line %d: unexpected end of file, expecting ";" or "{"`, words[0].line)
			}
			if inBlock {
				return nil, fmt.Errorf(`unexpected end of file, expecting "}"`)
			}
			return out, nil
		case tokSemicolon:
			if len(words) == 0 {
				return nil, fmt.Errorf(`line %d: unexpected ";"`, t.line)
			}
			out = append(out, mkDirective(words, nil))
			words = nil
		case tokOpenBrace:
			if len(words) == 0 {
				return nil, fmt.Errorf(`line %d: unexpected "{"`, t.line)
			}
			block, err := p.parseBlock(true)
			if err != nil {
				return nil, err
			}
			if block == nil {
				block = []Directive{}
			}
			out = append(out, mkDirective(words, block))
			words = nil
		case tokCloseBrace:
			if !inBlock {
				return nil, fmt.Errorf(`line %d: unexpected "}"`, t.line)
			}
			if len(words) > 0 {
				return nil, fmt.Errorf(`line %d: unexpected "}", expecting ";"`, t.line)
			}
			return out, nil
		case tokWord:
			words = append(words, t)
		}
	}
}

func mkDirective(words []token, block []Directive) Directive {
	args := make([]string, 0, len(words)-1)
	for _, w := range words[1:] {
		args = append(args, w.val)
	}
	return Directive{Name: words[0].val, Args: args, Block: block, Line: words[0].line}
}
