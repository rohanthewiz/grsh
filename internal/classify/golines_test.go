package classify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// runBoth drives the live consumeGo and the frozen reference over the same
// input and reports both results. Comparing (text, end, err) rather than
// just the end index matters: the text becomes Chunk.Text and is fed to
// trackGoLine, so a divergence there would corrupt scope and brace depth
// even when the line boundary happened to agree.
func runBoth(t *testing.T, src string, i int) (gotText string, gotEnd int, gotErr error,
	wantText string, wantEnd int, wantErr error,
) {
	t.Helper()
	lines := strings.Split(src, "\n")
	gotText, gotEnd, gotErr = newGoSrc(src).consumeGo(lines, i)
	wantText, wantEnd, wantErr = consumeGoRef(lines, i)
	return
}

// consumeGoCorpus is the shared corpus for the differential test and the
// fuzz seed. Every entry is source as the classifier would see it, with the
// Go logical line starting at line 0.
var consumeGoCorpus = []string{
	// Ordinary complete lines.
	"x := 1",
	"x := 1\ny := 2",
	"fmt.Println(x)",
	"return 5",

	// Continuations: the line cannot end where it looks like it might.
	"x := 1 +\n2",
	"fmt.Println(\n\tx,\n\ty,\n)",
	"xs := []int{\n1,\n2,\n}",
	"m := map[string]int{\n\"a\": 1,\n\"b\": 2,\n}",
	"x := foo(\nbar(\nbaz,\n),\n)",
	"v := []map[string]any{\n{\"k\": 1},\n{\"k\": 2},\n}",

	// Block openers: classification continues per line INSIDE them, so
	// these must stop at line 0.
	"func f() {\nreturn 1\n}",
	"if x {\ny()\n}",
	"for i := range xs {\nf(i)\n}",
	"switch x {\ncase 1:\n}",
	"select {\ncase <-c:\n}",
	"} else {\nx()\n}",
	"f := func(a int) {\nb()\n}",
	"f := func(a int) int {\nreturn a\n}",
	"{\nx()\n}",
	"}",

	// case/default end in a colon but are complete.
	"case 1:\nx()",
	"default:\nx()",
	"case \"a\", \"b\":\nx()",
	// A label also ends in a colon but is NOT case/default.
	"loop:\nfor {\n}",

	// Struct and type declarations — braces that are not statement blocks.
	"type T struct {\nA int\n}",
	"var x = struct{\nA int\n}{1}",

	// Multi-line tokens: the documented start-line attribution.
	"s := `raw\nstring`\nx := 1",
	"s := `raw\nstring`",
	"x := 1 /* c\nont */ + 2\ny := 2",
	"x := /* c\nont */ 1\ny := 2",
	"x := `a`\ny := 2",

	// Malformed / unterminated input: the REPL sees this on every
	// keystroke, so agreement here is not a corner case.
	"s := \"unterminated\nx := 1",
	"x := (",
	"x := [",
	"x := {",
	"xs := []int{",
	"func f() {",
	"x :=",
	"@#$%",
	"x := 'a",

	// Blank and comment lines inside a pending fragment.
	"xs := []int{\n\n1,\n\n}",
	"xs := []int{\n// comment\n1,\n}",
	"x := foo(\n\n)",
	"{\n\n}",

	// Shell text following an incomplete Go line: consumeGo lexes straight
	// through it, and both versions must do so identically.
	"x := 1 +\necho `date`",
	"x := (\nls -la | grep x",

	// Trailing blank line, which strings.Split always produces for input
	// ending in a newline.
	"x := 1\n",
	"xs := []int{\n1,\n}\n",
	"func f() {\n",
}

// TestConsumeGoMatchesRef is the correctness argument for the incremental
// rewrite: same answers as the version it replaced, on everything.
func TestConsumeGoMatchesRef(t *testing.T) {
	for _, src := range consumeGoCorpus {
		t.Run(fmt.Sprintf("%q", src), func(t *testing.T) {
			gotText, gotEnd, gotErr, wantText, wantEnd, wantErr := runBoth(t, src, 0)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("err: got %v, ref %v", gotErr, wantErr)
			}
			if gotEnd != wantEnd {
				t.Errorf("end line: got %d, ref %d", gotEnd, wantEnd)
			}
			if gotText != wantText {
				t.Errorf("text:\n got %q\n ref %q", gotText, wantText)
			}
		})
	}
}

// TestConsumeGoMatchesRefAtOffset repeats the sweep starting from every
// non-blank line of every corpus entry, not just line 0.
//
// It exists because the incremental version's head facts (headCloses,
// headTok, bareBrace) are derived from lines[i] and then held constant, and
// its scanner is initialized at gs.offs[i]. Both are exactly the kind of
// thing that works at i=0 and is off by one line everywhere else — starting
// only from the top would never notice.
func TestConsumeGoMatchesRefAtOffset(t *testing.T) {
	for _, src := range consumeGoCorpus {
		lines := strings.Split(src, "\n")
		for i := range lines {
			if strings.TrimSpace(lines[i]) == "" {
				continue // File never calls consumeGo on a blank line
			}
			t.Run(fmt.Sprintf("%q@%d", src, i), func(t *testing.T) {
				gotText, gotEnd, gotErr, wantText, wantEnd, wantErr := runBoth(t, src, i)
				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("err: got %v, ref %v", gotErr, wantErr)
				}
				if gotEnd != wantEnd || gotText != wantText {
					t.Errorf("got (%q, %d), ref (%q, %d)", gotText, gotEnd, wantText, wantEnd)
				}
			})
		}
	}
}

// TestConsumeGoLinear is the complexity guard, asserted rather than left to
// a benchmark someone has to remember to read.
//
// It counts scanner work indirectly: a quadratic implementation re-lexes
// the accumulated fragment per line, so its total scanned bytes grow as
// n^2, and the wall-clock ratio between 64 and 512 lines lands near 64x
// rather than near 8x. Timing in a test is normally a bad idea, so the
// threshold is deliberately loose — 16x, twice the linear ideal and four
// times below the quadratic reality (measured at ~63x before the rewrite).
// It is there to catch a reintroduced O(n^2), not to police constant
// factors.
func TestConsumeGoLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}
	elapsed := func(n int) float64 {
		src := compositeSrc(n)
		lines := strings.Split(src, "\n")
		gs := newGoSrc(src)
		// Repeat enough that the small case is not measuring clock noise.
		const reps = 200
		start := time.Now()
		for range reps {
			if _, _, err := gs.consumeGo(lines, 0); err != nil {
				t.Fatal(err)
			}
		}
		return float64(time.Since(start).Nanoseconds()) / 1e6 // ms
	}
	small, large := elapsed(64), elapsed(512)
	if ratio := large / small; ratio > 16 {
		t.Errorf("8x the lines cost %.1fx the time — consumeGo looks super-linear again "+
			"(64 lines: %.3fms, 512 lines: %.3fms)", ratio, small, large)
	}
}

// compositeSrc builds an n-element composite literal spanning n+2 physical
// lines — one logical line, the shape the quadratic lived on.
func compositeSrc(n int) string {
	var b strings.Builder
	b.WriteString("xs := []int{\n")
	for i := range n {
		fmt.Fprintf(&b, "\t%d,\n", i)
	}
	b.WriteString("}")
	return b.String()
}

// FuzzConsumeGoMatchesRef attacks the equivalence directly. The corpus
// above is what a person thought to write down; this is for what they did
// not — the lexer edge cases around raw strings, comments and unterminated
// literals, which is exactly where lexing the whole source could diverge
// from lexing a truncated prefix.
func FuzzConsumeGoMatchesRef(f *testing.F) {
	for _, src := range consumeGoCorpus {
		f.Add(src)
	}
	// Fragments chosen to be mixed and mangled by the fuzzer.
	for _, extra := range []string{"`", "/*", "*/", "\"", "'", "{}", "()", "[]", ":", ";"} {
		f.Add(extra)
	}
	f.Fuzz(func(t *testing.T, src string) {
		lines := strings.Split(src, "\n")
		if strings.TrimSpace(lines[0]) == "" {
			return // File never calls consumeGo on a blank first line
		}
		gotText, gotEnd, gotErr := newGoSrc(src).consumeGo(lines, 0)
		wantText, wantEnd, wantErr := consumeGoRef(lines, 0)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("err mismatch on %q: got %v, ref %v", src, gotErr, wantErr)
		}
		if gotErr != nil {
			return // both incomplete: text and end are not meaningful
		}
		if gotEnd != wantEnd || gotText != wantText {
			t.Fatalf("mismatch on %q:\n got (%q, %d)\n ref (%q, %d)",
				src, gotText, gotEnd, wantText, wantEnd)
		}
	})
}
