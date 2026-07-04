package shellparse

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Parse parses one logical line of shell into a CmdList.
func Parse(src string) (*CmdList, error) {
	p := &parser{s: src}
	list, err := p.parseCmdList()
	if err != nil {
		return nil, err
	}
	p.skipSpaces()
	if !p.eof() {
		return nil, p.errf("unexpected %q", p.rest(8))
	}
	return list, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) eof() bool  { return p.i >= len(p.s) }
func (p *parser) peek() byte { return p.s[p.i] }

func (p *parser) at(tok string) bool { return strings.HasPrefix(p.s[p.i:], tok) }

func (p *parser) rest(n int) string {
	r := p.s[p.i:]
	if len(r) > n {
		r = r[:n] + "..."
	}
	return r
}

func (p *parser) skipSpaces() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t' || p.peek() == '\n') {
		p.i++
	}
}

func (p *parser) errf(format string, args ...any) error {
	return serr.New(fmt.Sprintf(format, args...), "col", fmt.Sprint(p.i+1), "input", p.s)
}

func (p *parser) parseCmdList() (*CmdList, error) {
	list := &CmdList{}
	for {
		p.skipSpaces()
		if p.eof() {
			break
		}
		ao, err := p.parseAndOr()
		if err != nil {
			return nil, err
		}
		if ao != nil {
			list.Items = append(list.Items, ao)
		}
		p.skipSpaces()
		if !p.eof() && p.peek() == ';' {
			p.i++
			continue
		}
		break
	}
	return list, nil
}

func (p *parser) parseAndOr() (*AndOr, error) {
	first, err := p.parsePipeline()
	if err != nil {
		return nil, err
	}
	if first == nil {
		p.skipSpaces()
		if p.at("&&") || p.at("||") {
			return nil, p.errf("missing command before %q", p.rest(2))
		}
		return nil, nil
	}
	ao := &AndOr{Pipes: []*Pipeline{first}}
	for {
		p.skipSpaces()
		var op LogicOp
		switch {
		case p.at("&&"):
			op = AndOp
		case p.at("||"):
			op = OrOp
		default:
			return ao, nil
		}
		p.i += 2
		next, err := p.parsePipeline()
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, p.errf("missing command after && or ||")
		}
		ao.Ops = append(ao.Ops, op)
		ao.Pipes = append(ao.Pipes, next)
	}
}

func (p *parser) parsePipeline() (*Pipeline, error) {
	first, err := p.parseCommand()
	if err != nil {
		return nil, err
	}
	if first == nil {
		p.skipSpaces()
		if !p.eof() && p.peek() == '|' && !p.at("||") {
			return nil, p.errf("missing command before |")
		}
		return nil, nil
	}
	pl := &Pipeline{Cmds: []*Command{first}}
	for {
		p.skipSpaces()
		if p.eof() || p.peek() != '|' || p.at("||") {
			return pl, nil
		}
		p.i++
		next, err := p.parseCommand()
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, p.errf("missing command after |")
		}
		pl.Cmds = append(pl.Cmds, next)
	}
}

// parseCommand parses words and redirections until a control operator.
// Returns nil (no error) if there is nothing here.
func (p *parser) parseCommand() (*Command, error) {
	cmd := &Command{}
	for {
		p.skipSpaces()
		if p.eof() {
			break
		}
		c := p.peek()
		if c == ';' {
			break
		}
		if c == '|' {
			break // both | and ||
		}
		if c == '&' {
			if p.at("&>>") || p.at("&>") {
				r, err := p.parseRedir()
				if err != nil {
					return nil, err
				}
				cmd.Redirs = append(cmd.Redirs, *r)
				continue
			}
			if p.at("&&") {
				break
			}
			return nil, p.errf("background jobs (&) are not supported yet")
		}
		if c == '#' {
			// Comment at word start: rest of line is ignored.
			p.i = len(p.s)
			break
		}
		if c == '<' || c == '>' || p.atFDRedir() {
			r, err := p.parseRedir()
			if err != nil {
				return nil, err
			}
			cmd.Redirs = append(cmd.Redirs, *r)
			continue
		}
		w, err := p.parseWord()
		if err != nil {
			return nil, err
		}
		cmd.Words = append(cmd.Words, w)
	}
	if len(cmd.Words) == 0 && len(cmd.Redirs) == 0 {
		return nil, nil
	}
	return cmd, nil
}

// atFDRedir reports whether the parser is at a digit-prefixed redirection
// like 2> or 2>>. We are always at a word boundary when called.
func (p *parser) atFDRedir() bool {
	j := p.i
	for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
		j++
	}
	return j > p.i && j < len(p.s) && (p.s[j] == '>' || p.s[j] == '<')
}

func (p *parser) parseRedir() (*Redir, error) {
	r := &Redir{FD: -1}

	// Optional fd prefix.
	start := p.i
	for !p.eof() && p.peek() >= '0' && p.peek() <= '9' {
		p.i++
	}
	if p.i > start {
		fd := 0
		for _, c := range p.s[start:p.i] {
			fd = fd*10 + int(c-'0')
		}
		r.FD = fd
	}

	switch {
	case p.at("&>>"):
		p.i += 3
		r.Op = RedirOutErrApp
	case p.at("&>"):
		p.i += 2
		r.Op = RedirOutErr
	case p.at(">>"):
		p.i += 2
		r.Op = RedirAppend
		if r.FD < 0 {
			r.FD = 1
		}
	case p.at(">"):
		p.i++
		r.Op = RedirOut
		if r.FD < 0 {
			r.FD = 1
		}
		// N>&M dup, e.g. 2>&1
		if !p.eof() && p.peek() == '&' {
			p.i++
			dstart := p.i
			for !p.eof() && p.peek() >= '0' && p.peek() <= '9' {
				p.i++
			}
			if p.i == dstart {
				return nil, p.errf("expected file descriptor after >&")
			}
			d := 0
			for _, c := range p.s[dstart:p.i] {
				d = d*10 + int(c-'0')
			}
			r.Op = RedirDup
			r.DupTo = d
			return r, nil
		}
	case p.at("<"):
		p.i++
		r.Op = RedirIn
		if r.FD < 0 {
			r.FD = 0
		}
	default:
		return nil, p.errf("expected redirection operator")
	}

	p.skipSpaces()
	w, err := p.parseWord()
	if err != nil {
		return nil, err
	}
	if len(w.Segs) == 0 {
		return nil, p.errf("missing redirection target")
	}
	r.Target = w
	return r, nil
}

// isWordDelim reports whether an unquoted byte ends a word.
func isWordDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '|', '&', ';', '<', '>':
		return true
	}
	return false
}

func (p *parser) parseWord() (*Word, error) {
	w := &Word{}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			w.Segs = append(w.Segs, Lit{Text: lit.String()})
			lit.Reset()
		}
	}
	for !p.eof() {
		c := p.peek()
		if isWordDelim(c) {
			break
		}
		switch c {
		case '\'':
			flush()
			p.i++
			end := strings.IndexByte(p.s[p.i:], '\'')
			if end < 0 {
				return nil, p.errf("unterminated single quote")
			}
			w.Segs = append(w.Segs, Lit{Text: p.s[p.i : p.i+end], Quoted: true})
			p.i += end + 1
		case '"':
			flush()
			p.i++
			segs, err := p.parseDoubleQuoted()
			if err != nil {
				return nil, err
			}
			w.Segs = append(w.Segs, segs...)
		case '\\':
			flush()
			p.i++
			if p.eof() {
				return nil, p.errf("trailing backslash")
			}
			w.Segs = append(w.Segs, Lit{Text: string(p.peek()), Quoted: true})
			p.i++
		case '$':
			flush()
			seg, err := p.parseDollar(false)
			if err != nil {
				return nil, err
			}
			w.Segs = append(w.Segs, seg)
		case '{':
			flush()
			seg, err := p.parseBraceExpr(false)
			if err != nil {
				return nil, err
			}
			w.Segs = append(w.Segs, seg)
		default:
			lit.WriteByte(c)
			p.i++
		}
	}
	flush()
	return w, nil
}

// parseDoubleQuoted parses the interior of "..." (opening quote consumed).
// $VAR, ${VAR}, $(...), and {expr} still expand; glob and tilde do not.
func (p *parser) parseDoubleQuoted() ([]Segment, error) {
	var segs []Segment
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, Lit{Text: lit.String(), Quoted: true})
			lit.Reset()
		}
	}
	for {
		if p.eof() {
			return nil, p.errf("unterminated double quote")
		}
		c := p.peek()
		switch c {
		case '"':
			p.i++
			flush()
			if len(segs) == 0 {
				// "" is an explicit empty word segment.
				segs = append(segs, Lit{Text: "", Quoted: true})
			}
			return segs, nil
		case '\\':
			p.i++
			if p.eof() {
				return nil, p.errf("unterminated double quote")
			}
			n := p.peek()
			switch n {
			case '"', '\\', '$', '`', '{', '}':
				lit.WriteByte(n)
			default:
				lit.WriteByte('\\')
				lit.WriteByte(n)
			}
			p.i++
		case '$':
			flush()
			seg, err := p.parseDollar(true)
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
		case '{':
			flush()
			seg, err := p.parseBraceExpr(true)
			if err != nil {
				return nil, err
			}
			segs = append(segs, seg)
		default:
			lit.WriteByte(c)
			p.i++
		}
	}
}

// parseDollar parses $NAME, ${NAME}, $(...), $1..$9, $@, $#.
// The '$' has not been consumed yet.
func (p *parser) parseDollar(quoted bool) (Segment, error) {
	p.i++ // consume $
	if p.eof() {
		return Lit{Text: "$", Quoted: quoted}, nil
	}
	c := p.peek()
	switch {
	case c == '(':
		inner, err := p.scanBalanced('(', ')')
		if err != nil {
			return nil, err
		}
		sub, err := Parse(inner)
		if err != nil {
			return nil, serr.Wrap(err, "in", "command substitution $("+inner+")")
		}
		return CmdSub{List: sub, Src: inner, Quoted: quoted}, nil
	case c == '{':
		inner, err := p.scanBalanced('{', '}')
		if err != nil {
			return nil, err
		}
		if inner == "" {
			return nil, p.errf("empty ${}")
		}
		return EnvVar{Name: inner, Quoted: quoted}, nil
	case c == '@' || c == '#':
		p.i++
		return EnvVar{Name: string(c), Quoted: quoted}, nil
	case c >= '0' && c <= '9':
		p.i++
		return EnvVar{Name: string(c), Quoted: quoted}, nil
	case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		start := p.i
		for !p.eof() {
			c := p.peek()
			if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				p.i++
				continue
			}
			break
		}
		return EnvVar{Name: p.s[start:p.i], Quoted: quoted}, nil
	default:
		return Lit{Text: "$", Quoted: quoted}, nil
	}
}

// parseBraceExpr parses {expr} Go interpolation. The '{' has not been
// consumed. `{}` yields a literal (so `find -exec ... {} \;` works).
func (p *parser) parseBraceExpr(quoted bool) (Segment, error) {
	inner, err := p.scanBalancedGo()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(inner) == "" {
		return Lit{Text: "{" + inner + "}", Quoted: quoted}, nil
	}
	return GoExpr{Src: inner, Quoted: quoted}, nil
}

// scanBalanced consumes open..close (nesting-aware, shell-quote-aware)
// and returns the interior. Used for $(...) and ${...}.
func (p *parser) scanBalanced(open, close byte) (string, error) {
	if p.peek() != open {
		return "", p.errf("expected %q", string(open))
	}
	p.i++
	start := p.i
	depth := 1
	for !p.eof() {
		c := p.peek()
		switch c {
		case '\\':
			p.i++ // skip escaped char
		case '\'':
			p.i++
			end := strings.IndexByte(p.s[p.i:], '\'')
			if end < 0 {
				return "", p.errf("unterminated single quote")
			}
			p.i += end
		case '"':
			p.i++
			for !p.eof() && p.peek() != '"' {
				if p.peek() == '\\' {
					p.i++
				}
				p.i++
			}
			if p.eof() {
				return "", p.errf("unterminated double quote")
			}
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				inner := p.s[start:p.i]
				p.i++
				return inner, nil
			}
		}
		p.i++
	}
	return "", p.errf("missing closing %q", string(close))
}

// scanBalancedGo consumes {...} where the interior is a Go expression:
// braces nest, and Go string/rune literals ("", ”, “) are skipped.
func (p *parser) scanBalancedGo() (string, error) {
	if p.peek() != '{' {
		return "", p.errf("expected '{'")
	}
	p.i++
	start := p.i
	depth := 1
	for !p.eof() {
		c := p.peek()
		switch c {
		case '"', '\'':
			q := c
			p.i++
			for !p.eof() && p.peek() != q {
				if p.peek() == '\\' {
					p.i++
				}
				p.i++
			}
			if p.eof() {
				return "", p.errf("unterminated Go string in {expr}")
			}
		case '`':
			p.i++
			end := strings.IndexByte(p.s[p.i:], '`')
			if end < 0 {
				return "", p.errf("unterminated raw string in {expr}")
			}
			p.i += end
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				inner := p.s[start:p.i]
				p.i++
				return inner, nil
			}
		}
		p.i++
	}
	return "", p.errf("unmatched '{' (use \\{ for a literal brace)")
}
