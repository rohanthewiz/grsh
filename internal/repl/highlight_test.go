package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// newTestHighlighter pins $PATH to a temp dir holding exactly one
// executable, `okcmd`, so known/unknown command coloring is deterministic
// regardless of the host system.
func newTestHighlighter(t *testing.T) (*highlighter, *runner.Session) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "okcmd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out, errB bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB})
	return newHighlighter(sess, newCompleter(sess.Idents)), sess
}

var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]+m`)

func stripSGR(s string) string { return sgrRe.ReplaceAllString(s, "") }

// TestHighlightPreservesText is the load-bearing invariant: the display
// engine repaints whatever the highlighter returns, so stripping the SGR
// sequences must always give back the input byte for byte — across
// languages, unicode, and the half-typed input the REPL lives on.
func TestHighlightPreservesText(t *testing.T) {
	h, _ := newTestHighlighter(t)
	corpus := []string{
		"",
		"okcmd -la --color=auto /tmp",
		"x := 42",
		"func f(a int) string {\n  return \"héllo — 世界\"\n}",
		"okcmd $HOME ${X}y 'sq' \"dq $V\" `bt`",
		"cat <<EOF\nliteral $body line\nEOF",
		"cat <<EOF\nunterminated body",
		"echo {name} and {strings.ToUpper(s)}",
		"a | okcmd && c || d ; e",
		"x := (1 +",
		"# comment\n// go comment\nokcmd",
		"echo \"unterminated",
		"FOO=1 BAR=2 okcmd -v",
		"if true {\n  y := 1 // trailing note\n}",
		"okcmd a\\ b \\",
		"echo $(okcmd inner)",
		"okcmd 2>/dev/null <in >out",
	}
	for _, src := range corpus {
		if got := stripSGR(h.render(src)); got != src {
			t.Errorf("render altered visible text:\n src=%q\n got=%q", src, got)
		}
	}
}

func TestHighlightGoTokens(t *testing.T) {
	h, _ := newTestHighlighter(t)

	got := h.render("if x := 42; x > 0 { // note")
	for _, want := range []string{
		hlKeyword + "if" + hlReset,
		hlNumber + "42" + hlReset,
		hlComment + "// note" + hlReset,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}

	got = h.render("s := \"hi\"")
	if !strings.Contains(got, hlString+`"hi"`+hlReset) {
		t.Errorf("string literal not colored: %q", got)
	}
	// `true` is a predeclared identifier, not a keyword — no color.
	if strings.Contains(h.render("x := true"), hlKeyword+"true") {
		t.Error("identifiers must not take the keyword color")
	}
}

func TestHighlightShellCommands(t *testing.T) {
	h, sess := newTestHighlighter(t)

	if got := h.render("okcmd -v file"); !strings.Contains(got, hlKnown+"okcmd"+hlReset) ||
		!strings.Contains(got, hlFlag+"-v"+hlReset) {
		t.Errorf("known command + flag: %q", got)
	}
	if got := h.render("nosuch file"); !strings.Contains(got, hlUnknown+"nosuch"+hlReset) {
		t.Errorf("unknown command: %q", got)
	}
	// cd resolves as a builtin even though $PATH holds only okcmd.
	if got := h.render("cd /tmp"); !strings.Contains(got, hlKnown+"cd"+hlReset) {
		t.Errorf("builtin: %q", got)
	}
	// Aliases defined in the session count as known.
	if err := sess.Eval("alias gg='okcmd -v'"); err != nil {
		t.Fatalf("alias: %v", err)
	}
	if got := h.render("gg now"); !strings.Contains(got, hlKnown+"gg"+hlReset) {
		t.Errorf("alias: %q", got)
	}

	// Command position re-arms after pipes/logicals — on the same line and
	// across a continuation.
	got := h.render("nosuch | okcmd")
	if !strings.Contains(got, hlUnknown+"nosuch"+hlReset) || !strings.Contains(got, hlKnown+"okcmd"+hlReset) {
		t.Errorf("pipe command position: %q", got)
	}
	got = h.render("okcmd a &&\nokcmd b")
	if strings.Count(got, hlKnown+"okcmd"+hlReset) != 2 {
		t.Errorf("continuation command position: %q", got)
	}

	// FOO=bar prefixes stay plain and don't consume the command position.
	got = h.render("FOO=1 okcmd")
	if strings.Contains(got, hlUnknown+"FOO=1") || !strings.Contains(got, hlKnown+"okcmd"+hlReset) {
		t.Errorf("assignment prefix: %q", got)
	}

	// Strings, variables, comments.
	got = h.render("okcmd 'sq' \"d $q\" $HOME ${X} # tail")
	for _, want := range []string{
		hlString + "'sq'" + hlReset,
		hlString + `"d $q"` + hlReset,
		hlVar + "$HOME" + hlReset,
		hlVar + "${X}" + hlReset,
		hlComment + "# tail" + hlReset,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}

	// {go interpolation} stays uncolored — a shell lexer shouldn't guess Go.
	got = h.render("okcmd {strings.ToUpper(\"x | y\")}")
	if !strings.Contains(got, "{strings.ToUpper(\"x | y\")}") {
		t.Errorf("interpolation must pass through untouched: %q", got)
	}
}

// TestHighlightMemo: same buffer, same string instance back — the display
// refreshes on cursor movement without edits, and render must not rerun.
func TestHighlightMemo(t *testing.T) {
	h, _ := newTestHighlighter(t)
	a := h.highlight([]rune("okcmd -v"))
	b := h.highlight([]rune("okcmd -v"))
	if a != b {
		t.Error("memoized render differs")
	}
	if c := h.highlight([]rune("okcmd -x")); c == a {
		t.Error("changed buffer must re-render")
	}
}
