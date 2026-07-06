// Package transform rewrites classified .grsh source into one valid Go
// file plus a side table of parsed shell fragments.
//
// The rewrite is strictly line-preserving: physical line N of the script
// is physical line N of the emitted region, so with the //line directive
// every go/token position already reports .grsh coordinates.
//
// Rewrites:
//   - shell chunk            → __shell(n)          (n indexes the side table)
//   - $(...) inside Go lines → __capture(n)
//   - top-level `func f(...)` → `f := func(...)`   (interp pre-hoists these)
//   - import "x"             → __import("x")
//   - # comments / blanks    → empty lines
package transform

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/serr"
)

// Result is the transformed program.
type Result struct {
	GoSrc string
	Tab   []*shellparse.CmdList // side table; __shell(n)/__capture(n) index it
}

// HeaderLines is the number of lines emitted before script line 1.
const HeaderLines = 3 // package main / func __main() { / //line file:1

// File transforms classified chunks. baseTab offsets side-table indices so
// a Session can accumulate fragments across Eval calls (REPL seam).
func File(name string, chunks []classify.Chunk, baseTab int) (*Result, error) {
	res := &Result{}
	var out []string

	addTab := func(cl *shellparse.CmdList) int {
		res.Tab = append(res.Tab, cl)
		return baseTab + len(res.Tab) - 1
	}

	for _, ch := range chunks {
		switch ch.Kind {
		case classify.Blank:
			out = append(out, "")
		case classify.Shell:
			cl, err := shellparse.Parse(ch.Text)
			if err != nil {
				return nil, serr.Wrap(err, "loc", fmt.Sprintf("%s:%d", name, ch.StartLine))
			}
			out = append(out, fmt.Sprintf("__shell(%d)", addTab(cl)))
			for l := ch.StartLine + 1; l <= ch.EndLine; l++ {
				out = append(out, "")
			}
		case classify.Go:
			lines := strings.Split(ch.Text, "\n")
			lines, err := rewriteGoChunk(name, ch, lines, addTab)
			if err != nil {
				return nil, err
			}
			out = append(out, lines...)
		}
	}

	var b strings.Builder
	b.WriteString("package main\nfunc __main() {\n")
	fmt.Fprintf(&b, "//line %s:1\n", name)
	b.WriteString(strings.Join(out, "\n"))
	b.WriteString("\n}\n")
	res.GoSrc = b.String()
	return res, nil
}

var (
	topFuncRe    = regexp.MustCompile(`^(\s*)func\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	methodRe     = regexp.MustCompile(`^(\s*)func\s*\(\s*([A-Za-z_][A-Za-z0-9_]*)\s+(\*?)\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	importRe     = regexp.MustCompile(`^\s*import\s+(?:[A-Za-z_][A-Za-z0-9_]*\s+)?"([^"]+)"\s*$`)
	quotedPathRe = regexp.MustCompile(`"([^"]+)"`)
)

// MethodPrefix names the globals that top-level method declarations become:
// `func (p Point) Dist(...)` → `__m_Point_Dist := func(p Point, ...)`. The
// interpreter hoists them like other top-level funcs and dispatches
// sv.Dist(...) to __m_Point_Dist with the receiver prepended.
const MethodPrefix = "__m_"

func rewriteGoChunk(name string, ch classify.Chunk, lines []string, addTab func(*shellparse.CmdList) int) ([]string, error) {
	// import "x" (single-line or grouped) → __import calls, blank filler.
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "import") {
		return rewriteImports(name, ch, lines)
	}

	// Top-level `func name(` → `name := func(` (same line, positions keep).
	if ch.Depth == 0 {
		if m := topFuncRe.FindStringSubmatch(lines[0]); m != nil {
			lines[0] = m[1] + m[2] + " := func(" + lines[0][len(m[0]):]
		}
		// Method `func (p Point) Dist(` → `__m_Point_Dist := func(p Point, `
		// (receiver becomes the first parameter).
		if m := methodRe.FindStringSubmatch(lines[0]); m != nil {
			indent, recvName, star, recvType, name := m[1], m[2], m[3], m[4], m[5]
			rest := lines[0][len(m[0]):]
			head := indent + MethodPrefix + recvType + "_" + name +
				" := func(" + recvName + " " + star + recvType
			if strings.HasPrefix(strings.TrimLeft(rest, " \t"), ")") {
				lines[0] = head + rest
			} else {
				lines[0] = head + ", " + rest
			}
		}
	}

	// $(...) captures, one physical line at a time.
	for i, ln := range lines {
		rewritten, err := rewriteCaptures(name, ch.StartLine+i, ln, addTab)
		if err != nil {
			return nil, err
		}
		lines[i] = rewritten
	}
	return lines, nil
}

func rewriteImports(name string, ch classify.Chunk, lines []string) ([]string, error) {
	var paths []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "import" || t == "import (" || t == "(" || t == ")" || t == "" {
			continue
		}
		if m := importRe.FindStringSubmatch(ln); m != nil {
			paths = append(paths, m[1])
			continue
		}
		// inside an import block: bare "path" or alias "path"
		if m := quotedPathRe.FindStringSubmatch(ln); m != nil {
			paths = append(paths, m[1])
			continue
		}
		return nil, serr.New("unsupported import form", "loc",
			fmt.Sprintf("%s:%d", name, ch.StartLine), "line", strings.TrimSpace(ln))
	}
	var calls []string
	for _, p := range paths {
		calls = append(calls, fmt.Sprintf("__import(%q)", p))
	}
	out := []string{strings.Join(calls, "; ")}
	for i := 1; i < len(lines); i++ {
		out = append(out, "")
	}
	return out, nil
}

// rewriteCaptures replaces $(...) with __capture(n) on one Go source line,
// skipping Go string literals and // comments.
func rewriteCaptures(name string, lineNum int, ln string, addTab func(*shellparse.CmdList) int) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(ln) {
		c := ln[i]
		switch {
		case c == '/' && i+1 < len(ln) && ln[i+1] == '/':
			b.WriteString(ln[i:])
			return b.String(), nil
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(ln) && ln[j] != c {
				if ln[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(ln) {
				j++
			}
			b.WriteString(ln[i:j])
			i = j
		case c == '`':
			j := strings.IndexByte(ln[i+1:], '`')
			if j < 0 {
				// Raw string continues past this line; emit rest verbatim.
				b.WriteString(ln[i:])
				return b.String(), nil
			}
			b.WriteString(ln[i : i+j+2])
			i += j + 2
		case c == '$' && i+1 < len(ln) && ln[i+1] == '(':
			inner, end, err := scanShellParen(ln, i+1)
			if err != nil {
				return "", serr.Wrap(err, "loc", fmt.Sprintf("%s:%d", name, lineNum))
			}
			cl, err := shellparse.Parse(inner)
			if err != nil {
				return "", serr.Wrap(err, "loc", fmt.Sprintf("%s:%d", name, lineNum))
			}
			fmt.Fprintf(&b, "__capture(%d)", addTab(cl))
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// scanShellParen scans a shell-quoted balanced (...) starting at the '('.
// Returns the interior and the index just past the ')'.
func scanShellParen(s string, open int) (string, int, error) {
	depth := 0
	i := open
	for i < len(s) {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", 0, serr.New("unterminated single quote in $(...)")
			}
			i += j + 1
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(s) {
				return "", 0, serr.New("unterminated double quote in $(...)")
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i + 1, nil
			}
		}
		i++
	}
	return "", 0, serr.New("unclosed $(...) capture (must be on one line)")
}
