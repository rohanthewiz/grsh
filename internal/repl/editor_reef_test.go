package repl

import (
	"testing"

	"github.com/rohanthewiz/grsh/internal/runner"
)

func newTestReefReader(t *testing.T) *reefReader {
	t.Helper()
	sess := runner.NewSession(runner.Options{})
	return newReefReader(sess, newCompleter(sess.Idents), openHistory(""))
}

// TestReefConfigGuards locks in the inputrc settings the adapter depends
// on: UTF-8 handling (the library's byte-era defaults corrupt non-ASCII
// input — found the hard way via the pty e2e), bracketed paste, and the
// ^C/^Z binds in every typing keymap.
func TestReefConfigGuards(t *testing.T) {
	rd := newTestReefReader(t)
	cfg := rd.rl.Config

	if cfg.GetBool("convert-meta") {
		t.Error("convert-meta must be off: high-bit input bytes would become ESC chords")
	}
	if !cfg.GetBool("output-meta") {
		t.Error("output-meta must be on: é would self-insert as a quoted meta sequence")
	}
	if !cfg.GetBool("enable-bracketed-paste") {
		t.Error("bracketed paste must be on for multiline paste-as-one-buffer")
	}
	for _, km := range []string{"emacs", "vi-insert", "vi-command"} {
		if got := cfg.Binds[km]["\x03"].Action; got != "grsh-interrupt" {
			t.Errorf("%s ^C bound to %q, want grsh-interrupt", km, got)
		}
		if got := cfg.Binds[km]["\x1a"].Action; got != "grsh-noop" {
			t.Errorf("%s ^Z bound to %q, want grsh-noop", km, got)
		}
	}
	// } dedents where it self-inserts; vi-command keeps its paragraph motion.
	for _, km := range []string{"emacs", "vi-insert"} {
		if got := cfg.Binds[km]["}"].Action; got != "grsh-electric-brace" {
			t.Errorf("%s } bound to %q, want grsh-electric-brace", km, got)
		}
	}
	if got := cfg.Binds["vi-command"]["}"].Action; got == "grsh-electric-brace" {
		t.Error("vi-command } must stay a motion, not the electric brace")
	}
}

// TestUnitSource checks the history adapter contract: reads come from the
// shared unit store (index 0 = oldest), Write is a no-op (the loop owns
// persistence), and out-of-range reads error instead of panicking.
func TestUnitSource(t *testing.T) {
	store := openHistory("")
	store.Append("first")
	store.Append("func f() {\n\treturn\n}") // multi-line unit stays one entry
	src := unitSource{store}

	if src.Len() != 2 {
		t.Fatalf("Len = %d, want 2", src.Len())
	}
	if got, err := src.GetLine(0); err != nil || got != "first" {
		t.Errorf("GetLine(0) = %q, %v", got, err)
	}
	if got, err := src.GetLine(1); err != nil || got != "func f() {\n\treturn\n}" {
		t.Errorf("GetLine(1) = %q, %v", got, err)
	}
	if _, err := src.GetLine(2); err == nil {
		t.Error("GetLine(2) should error out of range")
	}
	if _, err := src.GetLine(-1); err == nil {
		t.Error("GetLine(-1) should error")
	}
	if n, err := src.Write("ignored"); err != nil || n != 2 {
		t.Errorf("Write should be a no-op returning current length; got %d, %v", n, err)
	}
	if src.Len() != 2 {
		t.Error("Write must not append (the loop owns persistence)")
	}
}

// TestReefAcceptMultiline checks the classifier drives acceptance: an
// open construct keeps reading, a complete unit submits.
func TestReefAcceptMultiline(t *testing.T) {
	rd := newTestReefReader(t)
	if rd.rl.AcceptMultiline([]rune("func f() {")) {
		t.Error("open brace should keep the buffer pending")
	}
	if !rd.rl.AcceptMultiline([]rune("func f() {\n\treturn\n}")) {
		t.Error("balanced unit should be accepted")
	}
	if !rd.rl.AcceptMultiline([]rune("echo hi")) {
		t.Error("plain shell line should be accepted")
	}
}

// setBuffer loads the shell's real line buffer and puts the cursor at pos
// — AcceptMultiline and the electric-brace command read both, not just
// their argument.
func setBuffer(rd *reefReader, text string, pos int) {
	line, cur := rd.rl.Line(), rd.rl.Cursor()
	if n := line.Len(); n > 0 {
		line.Cut(0, n)
	}
	line.Insert(0, []rune(text)...)
	cur.Set(pos)
}

// TestReefAutoIndent: Enter (the overridden accept-line command) inside
// an open block must insert the newline plus depth×2 spaces as one buffer
// edit — and must NOT indent inside a heredoc body, where seeded spaces
// would become literal content and an indented delimiter line would never
// terminate the unit.
func TestReefAutoIndent(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want string // expected buffer after the override's own path
	}{
		{"depth 1", "func f() {", "func f() {\n  "},
		{"depth 2", "func f() {\nif true {", "func f() {\nif true {\n    "},
		{"heredoc: newline only", "cat <<EOF", "cat <<EOF\n"},
		{"heredoc inside block: newline only", "func f() {\nsh cat <<EOF", "func f() {\nsh cat <<EOF\n"},
		{"shell continuation: no depth", "echo a &&", "echo a &&\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := newTestReefReader(t)
			setBuffer(rd, tc.buf, len([]rune(tc.buf)))
			rd.rl.Keymap.Commands()["accept-line"]()
			if got := string(*rd.rl.Line()); got != tc.want {
				t.Errorf("buffer = %q, want %q", got, tc.want)
			}
			if pos, want := rd.rl.Cursor().Pos(), len([]rune(tc.want)); pos != want {
				t.Errorf("cursor = %d, want %d (after the indent)", pos, want)
			}
		})
	}

	// Enter mid-buffer indents for the depth at the cursor, not the end.
	rd := newTestReefReader(t)
	buf := "func f() {\nif true {\n}"
	setBuffer(rd, buf, len("func f() {")) // cursor right after the first {
	rd.rl.Keymap.Commands()["accept-line"]()
	want := "func f() {\n  \nif true {\n}"
	if got := string(*rd.rl.Line()); got != want {
		t.Errorf("mid-buffer = %q, want %q (depth-1 indent at the cursor)", got, want)
	}
}

// TestReefElectricBrace: } typed on a line of pure indentation closes the
// block one level back (gofmt style); anywhere else, and inside heredoc
// bodies, it is a plain insert.
func TestReefElectricBrace(t *testing.T) {
	cases := []struct {
		name     string
		buf      string
		wantLine string
	}{
		{"dedents seeded indent", "func f() {\n  ", "func f() {\n}"},
		{"dedents one level only", "func f() {\nif true {\n    ", "func f() {\nif true {\n  }"},
		{"plain insert mid-line", "x := map[string]int{", "x := map[string]int{}"},
		{"plain insert at column 0", "func f() {\n", "func f() {\n}"},
		{"heredoc body keeps its spaces", "cat <<EOF\n  ", "cat <<EOF\n  }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := newTestReefReader(t)
			setBuffer(rd, tc.buf, len([]rune(tc.buf)))
			rd.rl.Keymap.Commands()["grsh-electric-brace"]()
			if got := string(*rd.rl.Line()); got != tc.wantLine {
				t.Errorf("line = %q, want %q", got, tc.wantLine)
			}
			if pos, want := rd.rl.Cursor().Pos(), len([]rune(tc.wantLine)); pos != want {
				t.Errorf("cursor = %d, want %d (after the brace)", pos, want)
			}
		})
	}
}

// withGhost enables ghost text on a test reader. newReefReader only builds
// the suggester when colorEnabled(), which is false under `go test` (stdout
// is not a terminal), so the tests wire it up explicitly.
func withGhost(rd *reefReader, units ...string) *reefReader {
	store := openHistory("")
	for _, u := range units {
		store.Append(u)
	}
	rd.ghost = newSuggester(store)
	return rd
}

// TestReefGhostText drives the hint provider — grsh's per-refresh hook — and
// checks what lands in the display engine's inline-suggestion slot.
func TestReefGhostText(t *testing.T) {
	provide := func(rd *reefReader, buf string, pos int) {
		setBuffer(rd, buf, pos)
		rd.hintProvider([]rune(buf), pos)
	}

	t.Run("suggests at end of line", func(t *testing.T) {
		rd := withGhost(newTestReefReader(t), "echo hello world")
		provide(rd, "echo", 4)
		if got := rd.rl.GetInlineSuggestion(); got != "echo hello world" {
			t.Errorf("ghost = %q, want the matching unit", got)
		}
	})

	t.Run("cleared away from the end", func(t *testing.T) {
		rd := withGhost(newTestReefReader(t), "echo hello world")
		provide(rd, "echo", 2)
		if got := rd.rl.GetInlineSuggestion(); got != "" {
			t.Errorf("ghost = %q, want none with the cursor mid-buffer", got)
		}
	})

	t.Run("cleared once the match is gone", func(t *testing.T) {
		rd := withGhost(newTestReefReader(t), "echo hello world")
		provide(rd, "echo", 4)
		provide(rd, "cargo", 5)
		if got := rd.rl.GetInlineSuggestion(); got != "" {
			t.Errorf("ghost = %q, want it cleared when nothing matches", got)
		}
	})

	t.Run("held while the line is accepted", func(t *testing.T) {
		rd := withGhost(newTestReefReader(t), "echo hello world")
		provide(rd, "echo", 4)
		// AcceptMultiline is the library's last step before the display
		// engine walks past the accepted line: it must take the ghost down,
		// or the engine measures (and leaves printed) the suggested line.
		if !rd.rl.AcceptMultiline([]rune("echo")) {
			t.Fatal("a complete shell line should be accepted")
		}
		provide(rd, "echo", 4)
		if got := rd.rl.GetInlineSuggestion(); got != "" {
			t.Errorf("ghost = %q, want none while accepting", got)
		}
		// A pending unit is not being accepted, so the hold lifts.
		rd.rl.AcceptMultiline([]rune("func f() {"))
		provide(rd, "echo", 4)
		if got := rd.rl.GetInlineSuggestion(); got != "echo hello world" {
			t.Errorf("ghost = %q, want it back after a continuation", got)
		}
	})

	t.Run("disabled when color is off", func(t *testing.T) {
		rd := newTestReefReader(t) // no ghost wired: the colorEnabled() gate
		provide(rd, "echo", 4)
		if got := rd.rl.GetInlineSuggestion(); got != "" {
			t.Errorf("ghost = %q, want none without color", got)
		}
	})

	t.Run("composes with the breadcrumb", func(t *testing.T) {
		// The two features share one callback; neither may swallow the other.
		rd := withGhost(newTestReefReader(t), "func hi() string {\n  return \"x\"\n}")
		buf := "func hi() string {"
		setBuffer(rd, buf, len(buf))
		hint := string(rd.hintProvider([]rune(buf), len(buf)))
		if hint == "" {
			t.Error("breadcrumb lost: the ghost update must compose, not replace")
		}
		// ...and the multi-line unit is still refused as ghost text.
		if got := rd.rl.GetInlineSuggestion(); got != "" {
			t.Errorf("ghost = %q, want none for a multi-line unit", got)
		}
	})
}

// TestReefHintProvider checks the wiring: one callback feeds the hint lane
// AND refreshes the ghost, and the hint lane carries all three of its
// sources. (What each source renders is hint.go's business — hint_test.go
// covers that; this is about the seam.)
func TestReefHintProvider(t *testing.T) {
	sess := runner.NewSession(runner.Options{})
	if err := sess.Eval("alias ll='ls -la'"); err != nil {
		t.Fatalf("defining the alias: %v", err)
	}
	rd := withGhost(newReefReader(sess, newCompleter(sess.Idents), openHistory("")),
		"ll --color")

	// Signature help from the registry.
	buf := "strings.Split("
	setBuffer(rd, buf, len(buf))
	if got := string(rd.hintProvider([]rune(buf), len(buf))); got != "strings.Split(string, string) []string" {
		t.Errorf("hint = %q, want the signature", got)
	}

	// Alias expansion — and the ghost still updated on the same call.
	buf = "ll"
	setBuffer(rd, buf, len(buf))
	if got := string(rd.hintProvider([]rune(buf), len(buf))); got != "ll → ls -la" {
		t.Errorf("hint = %q, want the alias expansion", got)
	}
	if got := rd.rl.GetInlineSuggestion(); got != "ll --color" {
		t.Errorf("ghost = %q, want the hint provider to have refreshed it", got)
	}

	// Nothing to say: the lane collapses rather than reserving a row.
	buf = "echo hi"
	setBuffer(rd, buf, len(buf))
	if got := rd.hintProvider([]rune(buf), len(buf)); got != nil {
		t.Errorf("hint = %q, want nil for a plain complete line", string(got))
	}

	// The memo is keyed on the buffer alone, so it must not outlive the
	// prompt: an alias defined by the command just run has to hint at the
	// next one. Readline drops it; here that step is made explicit.
	buf = "gs"
	setBuffer(rd, buf, len(buf))
	rd.hintProvider([]rune(buf), len(buf)) // memoizes "no hint" for this buffer
	if err := sess.Eval("alias gs='git status'"); err != nil {
		t.Fatalf("defining the second alias: %v", err)
	}
	if got := rd.hintProvider([]rune(buf), len(buf)); got != nil {
		t.Errorf("hint = %q, want the memoized answer within the prompt", string(got))
	}
	rd.hints.reset() // what Readline does at a fresh prompt
	if got := string(rd.hintProvider([]rune(buf), len(buf))); got != "gs → git status" {
		t.Errorf("hint = %q, want the newly defined alias after the reset", got)
	}
}

// TestReefForwardWordAcceptsGhost: a forward-word key takes the next word of
// the suggestion when one applies, and is a plain motion otherwise.
func TestReefForwardWordAcceptsGhost(t *testing.T) {
	rd := withGhost(newTestReefReader(t), "echo hello world")
	setBuffer(rd, "echo", 4)
	rd.rl.SetInlineSuggestion("echo hello world")

	fwd := rd.rl.Keymap.Commands()["forward-word"]
	fwd()
	if got := string(*rd.rl.Line()); got != "echo hello" {
		t.Errorf("buffer = %q, want one accepted word", got)
	}
	fwd()
	if got := string(*rd.rl.Line()); got != "echo hello world" {
		t.Errorf("buffer = %q, want the rest accepted", got)
	}

	// Nothing suggested: fall through to the stock motion, buffer untouched.
	rd2 := withGhost(newTestReefReader(t))
	setBuffer(rd2, "echo hi", 0)
	rd2.rl.Keymap.Commands()["forward-word"]()
	if got := string(*rd2.rl.Line()); got != "echo hi" {
		t.Errorf("buffer = %q, want it unchanged by the motion", got)
	}
	if rd2.rl.Cursor().Pos() == 0 {
		t.Error("with no suggestion, forward-word must still move the cursor")
	}
}
