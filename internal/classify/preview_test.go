package classify

import (
	"errors"
	"testing"
)

// TestPreview: the display chunk map must cover every line — including the
// unfinished tail that is the norm while typing — and never mutate the
// classifier (it runs per keystroke under the syntax highlighter).
func TestPreview(t *testing.T) {
	type span struct {
		kind       Kind
		start, end int
	}
	tests := []struct {
		name string
		src  string
		want []span
	}{
		{"mixed complete", "echo hi\nx := 1",
			[]span{{Shell, 1, 1}, {Go, 2, 2}}},
		{"go tail incomplete", "func f() {\nx := (1 +",
			[]span{{Go, 1, 1}, {Go, 2, 2}}},
		{"heredoc tail incomplete", "cat <<EOF\nhello $name",
			[]span{{Shell, 1, 2}}},
		{"comment and shell", "# note\nls -la",
			[]span{{Blank, 1, 1}, {Shell, 2, 2}}},
		{"shell continuation spans lines", "echo a &&\necho b",
			[]span{{Shell, 1, 2}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New(nil)
			before := c.Pending("")
			chunks := c.Preview(tc.src)
			if len(chunks) != len(tc.want) {
				t.Fatalf("Preview(%q) = %d chunks %+v, want %d", tc.src, len(chunks), chunks, len(tc.want))
			}
			for i, w := range tc.want {
				ch := chunks[i]
				if ch.Kind != w.kind || ch.StartLine != w.start || ch.EndLine != w.end {
					t.Errorf("chunk %d = {%v %d-%d}, want {%v %d-%d}",
						i, ch.Kind, ch.StartLine, ch.EndLine, w.kind, w.start, w.end)
				}
			}
			after := c.Pending("")
			if after.Depth != before.Depth {
				t.Error("Preview mutated classifier state")
			}
		})
	}
}

// TestPendingHeredoc: the Heredoc flag must single out heredoc
// incompleteness — the one continuation state where auto-indent would
// corrupt input (spaces become literal body text and an indented
// delimiter line never matches).
func TestPendingHeredoc(t *testing.T) {
	c := New(nil)

	if info := c.Pending("cat <<EOF\nbody"); !info.NeedsMore || !info.Heredoc {
		t.Errorf("open heredoc: NeedsMore=%v Heredoc=%v, want true/true", info.NeedsMore, info.Heredoc)
	}
	if info := c.Pending("func f() {"); !info.NeedsMore || info.Heredoc {
		t.Errorf("open block: NeedsMore=%v Heredoc=%v, want true/false", info.NeedsMore, info.Heredoc)
	}
	if info := c.Pending("x := (1 +"); !info.NeedsMore || info.Heredoc {
		t.Errorf("mid-statement Go: NeedsMore=%v Heredoc=%v, want true/false", info.NeedsMore, info.Heredoc)
	}
	if info := c.Pending("cat <<EOF\nbody\nEOF"); info.NeedsMore || info.Heredoc {
		t.Errorf("closed heredoc: NeedsMore=%v Heredoc=%v, want false/false", info.NeedsMore, info.Heredoc)
	}
}

// TestFileIncompleteTail: File's error contract now includes best-effort
// chunks — the classified prefix plus a tail chunk of the failing kind —
// while still reporting ErrIncomplete for error-checking callers.
func TestFileIncompleteTail(t *testing.T) {
	c := New(nil)
	chunks, err := c.File("echo ok\ncat <<EOF\nbody")
	if !errors.Is(err, ErrIncomplete) || !errors.Is(err, ErrHeredoc) {
		t.Fatalf("want ErrIncomplete+ErrHeredoc, got %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("want prefix chunk + tail chunk, got %+v", chunks)
	}
	tail := chunks[1]
	if tail.Kind != Shell || tail.StartLine != 2 || tail.EndLine != 3 || tail.Rule != "incomplete" {
		t.Errorf("tail chunk = %+v, want Shell lines 2-3 rule=incomplete", tail)
	}
}
