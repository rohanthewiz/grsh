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
