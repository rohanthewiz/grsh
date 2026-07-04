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
			text, end := joinShell(lines, i)
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
// starting at physical line i; returns joined text and last line index.
func joinShell(lines []string, i int) (string, int) {
	text := lines[i]
	for {
		trimmed := strings.TrimRight(text, " \t")
		joined, ok := shellContinues(trimmed)
		if !ok || i+1 >= len(lines) {
			return text, i
		}
		i++
		text = joined + " " + strings.TrimLeft(lines[i], " \t")
	}
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
