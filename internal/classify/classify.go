// Package classify decides, per logical line, whether source is a shell
// command or Go code, and groups physical lines into chunks for the
// transform stage.
//
// The decision order (documented in docs/LANGUAGE.md):
//  1. blank / # comment / whole-line // comment → Blank
//  2. leading `sh ` → Shell (escape hatch; prefix stripped)
//  3. leading '(', '{' or '}' → Go (escape hatch for bare expressions)
//  4. leading Go keyword (if, for, func, var, ...) → Go; `go` is
//     deliberately NOT in the set — `go build` is a command
//  5. `:=` outside quotes/parens → Go
//  6. leading declared identifier followed by Go-ish punctuation → Go;
//     leading registered package name followed by '.' → Go
//  7. default → Shell
package classify

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/serr"
)

type Kind int

const (
	Blank Kind = iota
	Shell
	Go
)

func (k Kind) String() string {
	switch k {
	case Shell:
		return "shell"
	case Go:
		return "go"
	}
	return "blank"
}

// Chunk is a contiguous run of physical lines with one classification.
type Chunk struct {
	Kind      Kind
	Text      string // Shell: joined logical text; Go: verbatim lines joined by \n
	StartLine int    // 1-based physical line
	EndLine   int    // inclusive
	Depth     int    // brace depth at the start of the chunk
	Rule      string // which rule decided (--explain)
}

// goKeywords are leading tokens that force Go classification.
// `go` is excluded on purpose: `go build` etc. must stay shell (rule 4).
var goKeywords = map[string]bool{
	"if": true, "for": true, "func": true, "var": true, "const": true,
	"type": true, "return": true, "switch": true, "select": true,
	"defer": true, "break": true, "continue": true, "fallthrough": true,
	"case": true, "default": true, "else": true, "struct": true,
	"interface": true, "map": true, "chan": true, "range": true,
	"import": true,
}

// Scope tracks declared identifiers for rule 6a.
type Scope struct {
	parent *Scope
	names  map[string]bool
}

func NewScope(parent *Scope) *Scope {
	return &Scope{parent: parent, names: map[string]bool{}}
}

func (s *Scope) Add(name string) {
	if name != "" && name != "_" {
		s.names[name] = true
	}
}

func (s *Scope) Has(name string) bool {
	for sc := s; sc != nil; sc = sc.parent {
		if sc.names[name] {
			return true
		}
	}
	return false
}

// Classifier is incremental: a REPL can feed source chunk by chunk and
// scope/depth state carries over.
type Classifier struct {
	scope *Scope
	pkgs  map[string]bool
	depth int
}

func New(pkgNames []string) *Classifier {
	pkgs := map[string]bool{}
	for _, n := range pkgNames {
		pkgs[n] = true
	}
	return &Classifier{scope: NewScope(nil), pkgs: pkgs}
}

// Predeclare seeds global identifiers (the interpreter's builtins like
// glob, status, errexit) so rule 6a recognizes calls to them.
func (c *Classifier) Predeclare(names ...string) {
	root := c.scope
	for root.parent != nil {
		root = root.parent
	}
	for _, n := range names {
		root.Add(n)
	}
}

// File classifies a source chunk into contiguous classified chunks.
func (c *Classifier) File(src string) ([]Chunk, error) {
	lines := strings.Split(src, "\n")
	c.predeclare(lines)

	var chunks []Chunk
	i := 0
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			chunks = append(chunks, Chunk{Kind: Blank, StartLine: i + 1, EndLine: i + 1, Depth: c.depth})
			i++
			continue
		}
		kind, rule := c.classifyLine(t)
		if kind == Shell {
			text, end, err := joinShell(lines, i)
			if err != nil {
				return nil, err
			}
			if rule == "sh-prefix" {
				text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "sh"))
			}
			chunks = append(chunks, Chunk{Kind: Shell, Text: text, StartLine: i + 1, EndLine: end + 1, Depth: c.depth, Rule: rule})
			i = end + 1
			continue
		}
		text, end, err := consumeGo(lines, i)
		if err != nil {
			return nil, serr.Wrap(err, "line", strings.TrimSpace(lines[i]))
		}
		startDepth := c.depth
		c.trackGoLine(text)
		chunks = append(chunks, Chunk{Kind: Go, Text: text, StartLine: i + 1, EndLine: end + 1, Depth: startDepth, Rule: rule})
		i = end + 1
	}
	return chunks, nil
}

func (c *Classifier) classifyLine(t string) (Kind, string) {
	// Rule 2: explicit shell escape.
	if t == "sh" || strings.HasPrefix(t, "sh ") || strings.HasPrefix(t, "sh\t") {
		return Shell, "sh-prefix"
	}
	// Rule 3: explicit Go escapes.
	switch t[0] {
	case '(', '{', '}':
		return Go, "go-punct"
	}
	tok := firstToken(t)
	// Rule 4: leading Go keyword.
	if goKeywords[tok] {
		return Go, "keyword"
	}
	// Rule 5: := outside quotes at paren depth 0.
	if hasTopLevelDefine(t) {
		return Go, "define"
	}
	// Rule 6: leading identifier known to Go. The blank identifier is
	// always "declared" (`_ = expr` discards a value).
	if tok != "" {
		rest := strings.TrimLeft(t[len(tok):], " \t")
		if (tok == "_" || c.scope.Has(tok)) && startsGoOp(rest) {
			return Go, "declared-ident"
		}
		// Package selector must be immediate: `fmt.Println` yes, `time ls` no.
		if c.pkgs[tok] && strings.HasPrefix(t[len(tok):], ".") {
			return Go, "package-selector"
		}
	}
	// Rule 7: default.
	return Shell, "default"
}

// firstToken returns the leading identifier-shaped token, or "".
func firstToken(t string) string {
	i := 0
	for i < len(t) {
		c := t[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (i > 0 && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return t[:i]
}

// startsGoOp reports whether rest begins with punctuation that marks the
// preceding declared identifier as Go usage. A dot only counts when it is
// selector-shaped (`x.field`), so `cd ..` and `cd ./dir` stay shell.
func startsGoOp(rest string) bool {
	for _, op := range []string{"++", "--", "+=", "-=", "*=", "/=", "%=", "=", ",", "(", "["} {
		if strings.HasPrefix(rest, op) {
			return true
		}
	}
	if len(rest) >= 2 && rest[0] == '.' {
		c := rest[1]
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	return false
}

// hasTopLevelDefine reports whether t contains := outside quotes and
// outside parens/brackets (so `awk '{x := 1}'` and `dd if=x` stay shell).
func hasTopLevelDefine(t string) bool {
	depth := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '\'', '"', '`':
			q := t[i]
			i++
			for i < len(t) && t[i] != q {
				if t[i] == '\\' && q != '\'' {
					i++
				}
				i++
			}
			if i >= len(t) {
				return false // unbalanced quote: not Go-looking
			}
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ':':
			if depth == 0 && i+1 < len(t) && t[i+1] == '=' {
				return true
			}
		}
	}
	return false
}

// joinShell applies shell continuation rules (trailing \, |, &&, ||)
// starting at physical line i, then collects raw heredoc body lines
// (newlines preserved) for any << operators on the logical line; returns
// the chunk text and last line index. Running out of lines mid-heredoc
// reports ErrIncomplete so the REPL reads more and a script run fails.
func joinShell(lines []string, i int) (string, int, error) {
	text := lines[i]
	for {
		trimmed := strings.TrimRight(text, " \t")
		joined, ok := shellContinues(trimmed)
		if !ok || i+1 >= len(lines) {
			break
		}
		i++
		text = joined + " " + strings.TrimLeft(lines[i], " \t")
	}
	for _, hd := range scanHeredocs(text) {
		for {
			if i+1 >= len(lines) {
				return "", 0, fmt.Errorf("unterminated heredoc <<%s: %w", hd.delim, ErrIncomplete)
			}
			i++
			text += "\n" + lines[i]
			cmp := lines[i]
			if hd.stripTabs {
				cmp = strings.TrimLeft(cmp, "\t")
			}
			if cmp == hd.delim {
				break
			}
		}
	}
	return text, i, nil
}

// heredocRef is one << operator found on a logical shell line.
type heredocRef struct {
	delim     string
	stripTabs bool
}

// scanHeredocs finds heredoc operators on one logical shell line, skipping
// quoted regions, {expr} Go interpolations, and word-start # comments. It
// mirrors the shellparse rules closely enough to know how many body blocks
// follow; the parser remains the authority on validity.
func scanHeredocs(s string) []heredocRef {
	var out []heredocRef
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\'':
			end := strings.IndexByte(s[j+1:], '\'')
			if end < 0 {
				return out
			}
			j += end + 1
		case '"':
			j++
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return out
			}
		case '{':
			j = skipGoBrace(s, j)
		case '#':
			if j == 0 || s[j-1] == ' ' || s[j-1] == '\t' {
				return out // comment at word start: rest of line ignored
			}
		case '<':
			if j+1 >= len(s) || s[j+1] != '<' {
				continue
			}
			if j+2 < len(s) && s[j+2] == '<' {
				j += 2 // <<<: not a heredoc
				continue
			}
			j += 2
			strip := false
			if j < len(s) && s[j] == '-' {
				strip = true
				j++
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			delim, end := scanHeredocDelim(s, j)
			if delim == "" {
				return out // malformed; the parser reports it
			}
			out = append(out, heredocRef{delim: delim, stripTabs: strip})
			j = end - 1
		}
	}
	return out
}

// scanHeredocDelim reads a heredoc delimiter (quotes stripped) starting at
// j, returning it and the index just past it.
func scanHeredocDelim(s string, j int) (string, int) {
	var b strings.Builder
	for j < len(s) {
		switch c := s[j]; c {
		case '\'', '"':
			end := strings.IndexByte(s[j+1:], c)
			if end < 0 {
				return "", j
			}
			b.WriteString(s[j+1 : j+1+end])
			j += end + 2
		case '\\':
			if j+1 >= len(s) {
				return "", j
			}
			b.WriteByte(s[j+1])
			j += 2
		case ' ', '\t', '|', '&', ';', '<', '>':
			return b.String(), j
		default:
			b.WriteByte(c)
			j++
		}
	}
	return b.String(), j
}

// skipGoBrace advances past a balanced {expr} region (Go string literals
// respected), returning the index of the closing brace — or len(s) if it
// never closes (the parser reports that).
func skipGoBrace(s string, j int) int {
	depth := 0
	for ; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			q := s[j]
			j++
			for j < len(s) && s[j] != q {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return j
			}
		case '`':
			end := strings.IndexByte(s[j+1:], '`')
			if end < 0 {
				return len(s)
			}
			j += end + 1
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return j
}

func shellContinues(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if j, ok := strings.CutSuffix(s, "\\"); ok {
		return strings.TrimRight(j, " \t"), true
	}
	for _, op := range []string{"&&", "||", "|"} {
		if strings.HasSuffix(s, op) {
			return s, true
		}
	}
	return "", false
}
