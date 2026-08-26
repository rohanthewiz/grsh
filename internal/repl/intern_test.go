package repl

import (
	"strings"
	"testing"
	"unsafe"
)

// The intern's contract is two-part, and both halves are load-bearing:
// the string it returns must always equal string(line), and an unchanged
// buffer must cost nothing. A bug in the first half would paint, hint and
// suggest against text the user did not type; a regression in the second
// half is invisible except in the frame budget, which is why the identity
// and allocation checks are pinned here rather than left to a benchmark.

func TestRuneInternMatchesConversion(t *testing.T) {
	// Each step feeds the buffer that follows the previous one, the way a
	// session actually drives it — grow, shrink, replace in place, empty.
	steps := []string{
		"", "e", "ec", "echo", "echo hi", "echo h", "echo", "",
		"héllo wörld", "héllo wörld!", "héllo wxrld!", // multi-byte, same rune count
		"日本語のテキスト", "x",
		strings.Repeat("long ", 200),
	}
	var ri runeIntern
	for _, s := range steps {
		line := []rune(s)
		if got := ri.str(line); got != s {
			t.Fatalf("str(%q) = %q, want the same text", s, got)
		}
		if ri.n != len(line) {
			t.Errorf("after %q: cached rune count = %d, want %d", s, ri.n, len(line))
		}
	}
}

// An unchanged buffer must reuse the very same string, not an equal one:
// that is what turns the downstream memos' compares into pointer hits.
func TestRuneInternReusesTheSameString(t *testing.T) {
	var ri runeIntern
	line := []rune("func report(items []string) error {")

	first := ri.str(line)
	second := ri.str(line)
	if unsafe.StringData(first) != unsafe.StringData(second) {
		t.Error("a repeat conversion returned a different string: the frame would pay a memcmp")
	}

	// Equal content in a different rune slice must hit too — the editor
	// hands back its own buffer, and nothing promises it is the same array.
	if third := ri.str([]rune("func report(items []string) error {")); unsafe.StringData(third) != unsafe.StringData(first) {
		t.Error("equal content in a fresh slice missed the intern")
	}

	// ...and one changed rune, same length, must NOT.
	changed := []rune("func report(items []string) error }")
	if got := ri.str(changed); got != string(changed) {
		t.Errorf("str = %q, want the changed text %q", got, string(changed))
	}
}

func TestRuneInternDoesNotAllocateOnAHit(t *testing.T) {
	var ri runeIntern
	line := []rune(pendingBlockPrefix() + "\tfmt.Println(step0)")
	ri.str(line) // prime

	if n := testing.AllocsPerRun(50, func() { ri.str(line) }); n != 0 {
		t.Errorf("an unchanged buffer allocated %v times per call, want 0", n)
	}
}

// A rune the UTF-8 encoder cannot represent folds to U+FFFD in the string,
// so the runes can never match it back. The requirement is only that the
// answer stays correct — such a buffer simply re-converts every frame.
func TestRuneInternHandlesAnInvalidRune(t *testing.T) {
	var ri runeIntern
	line := []rune{'a', 0xD800, 'b'} // a lone surrogate: invalid UTF-8
	want := string(line)             // "a�b"

	if got := ri.str(line); got != want {
		t.Fatalf("str = %q, want %q", got, want)
	}
	if got := ri.str(line); got != want {
		t.Fatalf("second str = %q, want %q", got, want)
	}
	// The replacement char itself must not be mistaken for the surrogate.
	if runesEqual(line, want) {
		t.Error("runesEqual accepted U+FFFD as a match for the invalid rune it replaced")
	}
}

func TestRunesEqual(t *testing.T) {
	cases := []struct {
		line []rune
		s    string
		want bool
	}{
		{[]rune(""), "", true},
		{[]rune("abc"), "abc", true},
		{[]rune("abc"), "abd", false},
		{[]rune("abc"), "ab", false}, // s shorter
		{[]rune("ab"), "abc", false}, // s longer: trailing bytes
		{[]rune("héllo"), "héllo", true},
		{[]rune("héllo"), "hello", false}, // differs only in a multi-byte rune
		{[]rune("日本"), "日本", true},
		{[]rune("日本"), "日木", false},
	}
	for _, tc := range cases {
		if got := runesEqual(tc.line, tc.s); got != tc.want {
			t.Errorf("runesEqual(%q, %q) = %v, want %v", string(tc.line), tc.s, got, tc.want)
		}
	}
}

// runePrefix replaced a string(line[:pos]) conversion, so it has to agree
// with it on every cursor position — including the multi-byte ones, where
// a byte-indexed slice would split a rune.
func TestRunePrefixMatchesTheSliceItReplaced(t *testing.T) {
	for _, src := range []string{"", "echo hi", "héllo wörld", "日本語 ls"} {
		line := []rune(src)
		for pos := 0; pos <= len(line); pos++ {
			if got, want := runePrefix(src, pos), string(line[:pos]); got != want {
				t.Errorf("runePrefix(%q, %d) = %q, want %q", src, pos, got, want)
			}
		}
		// Past the end: the whole buffer, matching render's old clamp.
		if got := runePrefix(src, len(line)+7); got != src {
			t.Errorf("runePrefix(%q, past end) = %q, want the whole buffer", src, got)
		}
	}
	if got := runePrefix("echo", 0); got != "" {
		t.Errorf("runePrefix at 0 = %q, want empty", got)
	}
}
