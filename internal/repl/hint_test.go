package repl

import (
	"io"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// hintAt runs the hinter over a buffer with the cursor at a marker so the
// tests read like what the user sees. The marker is '‸' (caret U+2038):
// spelling the cursor inline beats counting runes, and it has to be a rune no
// shell or Go buffer would contain — '|' would collide with a pipeline.
func hintAt(t *testing.T, h *hinter, marked string) string {
	t.Helper()
	pos := strings.Index(marked, cur)
	if pos < 0 {
		t.Fatalf("buffer %q has no %s cursor marker", marked, cur)
	}
	src := marked[:pos] + marked[pos+len(cur):]
	// Marker index is a byte offset; the display engine passes a RUNE index.
	pos = len([]rune(src[:pos]))
	h.reset() // each call is an independent frame, not a memo hit
	return h.hint([]rune(src), pos)
}

// cur is the cursor marker used by hintAt.
const cur = "‸"

func newTestHinter(t *testing.T) *hinter {
	t.Helper()
	return newHinter(runner.NewSession(runner.Options{}))
}

// TestHintSignature: the Go lane, driven from the registry by reflection.
func TestHintSignature(t *testing.T) {
	h := newTestHinter(t)
	const splitSig = "strings.Split(string, string) []string"

	cases := []struct {
		buf, want string
	}{
		// The completed word at the cursor, before any paren is typed.
		{"strings.Split‸", splitSig},
		{"x := strings.Split‸", splitSig},
		// Inside the call: the cursor is past the name but still in its parens.
		{"strings.Split(‸", splitSig},
		{`strings.Split(s, "," ‸`, splitSig},
		{"strings.Split(s, sep)‸", ""}, // call closed: nothing to help with
		// Innermost open call wins over the outer one.
		{"fmt.Sprintf(strings.Split(s, ‸", splitSig},
		{"strings.Split(fmt.Sprintf(‸", "fmt.Sprintf(string, ...any) string"},
		// A completed word beats the call it sits inside: it is what the user
		// is typing right now.
		{"fmt.Sprintf(strings.Split‸", splitSig},
		// Half-typed symbols resolve to nothing rather than guessing.
		{"strings.Spl‸", ""},
		{"strings.‸", ""},
		{"nosuchpkg.Split(‸", ""},
		// Parens inside literals and comments must not open a phantom call.
		{`echo "strings.Split(" ‸`, ""},
		{"x := '(' ‸", ""},
		{"// strings.Split(\nx := ‸", ""},
		{"/* strings.Split( */ x := ‸", ""},
		// ...but a literal INSIDE the call does not close it either.
		{`strings.Split(") ", ‸`, splitSig},
		// Calls spanning physical lines still resolve.
		{"strings.Split(\n  s,\n  ‸", splitSig},
		// Plain shell gets no signature lane.
		{"ls -la ‸", ""},
		{"go build ./...‸", ""},
	}
	for _, c := range cases {
		if got := hintAt(t, h, c.buf); got != c.want {
			t.Errorf("hint(%q) = %q, want %q", c.buf, got, c.want)
		}
	}
}

// TestHintAlias: the shell lane. Only a command-position word that actually
// names an alias hints, and only on a line the classifier reads as shell.
func TestHintAlias(t *testing.T) {
	sess := runner.NewSession(runner.Options{})
	if err := sess.Eval("alias ll='ls -la'"); err != nil {
		t.Fatalf("defining the alias: %v", err)
	}
	h := newHinter(sess)

	cases := []struct {
		buf, want string
	}{
		{"ll‸", "ll → ls -la"},
		{"ll ‸", "ll → ls -la"},       // still the command word with a space typed
		{"ll -x foo‸", "ll → ls -la"}, // ...and it stays up while args are typed
		{"echo hi | ll‸", "ll → ls -la"},
		{"echo hi && ll‸", "ll → ls -la"},
		{"l‸", ""},          // a prefix of the name is not the name
		{"echo ll‸", ""},    // argument position: not a command
		{"lll‸", ""},        // unrelated word
		{"echo hi‸", ""},    // no alias in sight
		{"‸", ""},           // empty buffer
		{"var ll = 3‸", ""}, // Go line: `ll` is an identifier here, not a command
		{"ll := 3‸", ""},    // ...and here the alias name IS the identifier
		// A separator inside a quoted run does not start a new segment.
		{`echo "a | b" ‸`, ""},
	}
	for _, c := range cases {
		if got := hintAt(t, h, c.buf); got != c.want {
			t.Errorf("hint(%q) = %q, want %q", c.buf, got, c.want)
		}
	}

	// An alias defined inside an open block is still shell on its own line.
	if got := hintAt(t, h, "func f() {\n  ll‸"); !strings.HasPrefix(got, "ll → ls -la") {
		t.Errorf("hint on a continuation line = %q, want the alias lane", got)
	}
}

// TestHintAliasSanitized: alias values are arbitrary user text. A newline
// would cost a screen row and an ESC would corrupt the dim run the display
// engine measures around, so both are neutralized, and long values are cut.
func TestHintAliasSanitized(t *testing.T) {
	sess := runner.NewSession(runner.Options{})
	long := strings.Repeat("y", maxAliasRunes+20)
	if err := sess.Eval("alias weird='a\tb'\nalias big='" + long + "'"); err != nil {
		t.Fatalf("defining aliases: %v", err)
	}
	h := newHinter(sess)

	got := hintAt(t, h, "weird‸")
	if strings.ContainsAny(got, "\t\n\x1b") {
		t.Errorf("hint = %q, want control characters neutralized", got)
	}
	if got != "weird → a b" {
		t.Errorf("hint = %q, want the tab rendered as a space", got)
	}
	if got := hintAt(t, h, "big‸"); len([]rune(got)) > len("big → ")+maxAliasRunes+1 {
		t.Errorf("hint = %q, want the expansion capped at %d runes", got, maxAliasRunes)
	}
}

// TestHintComposition: the cursor-local lane and the breadcrumb share the
// line — neither may swallow the other.
func TestHintComposition(t *testing.T) {
	sess := runner.NewSession(runner.Options{})
	if err := sess.Eval("alias ll='ls -la'"); err != nil {
		t.Fatalf("defining the alias: %v", err)
	}
	h := newHinter(sess)

	// Breadcrumb alone (Round 1's behavior, unchanged).
	if got := hintAt(t, h, "func hi() string {‸"); got != "… func hi" {
		t.Errorf("hint = %q, want the breadcrumb alone", got)
	}
	// Signature plus breadcrumb: cursor-local segment first.
	got := hintAt(t, h, "func hi() string {\n  return strings.Split(‸")
	want := "strings.Split(string, string) []string" + hintSep + "… func hi"
	if got != want {
		t.Errorf("hint =\n  %q\nwant\n  %q", got, want)
	}
	// Alias plus breadcrumb.
	got = hintAt(t, h, "for i := 0; i < 3; i++ {\n  ll‸")
	if !strings.HasPrefix(got, "ll → ls -la"+hintSep) || !strings.HasSuffix(got, "… for") {
		t.Errorf("hint = %q, want both the alias and the breadcrumb", got)
	}
}

// TestHintMemo: the hint is recomputed per (buffer, cursor) and cached in
// between — the display engine refreshes on cursor-only movement too, and
// both cursor-local lanes depend on the cursor. The memo is dropped at each
// new prompt, since the alias table can change between them.
func TestHintMemo(t *testing.T) {
	sess := runner.NewSession(runner.Options{})
	h := newHinter(sess)

	line := []rune("ll")
	if got := h.hint(line, 2); got != "" {
		t.Fatalf("hint = %q, want none before the alias exists", got)
	}
	if err := sess.Eval("alias ll='ls -la'"); err != nil {
		t.Fatalf("defining the alias: %v", err)
	}
	if got := h.hint(line, 2); got != "" {
		t.Errorf("hint = %q, want the memoized answer within one prompt", got)
	}
	// A cursor move is a different key, so it recomputes...
	if got := h.hint(line, 1); got != "" {
		t.Errorf("hint = %q, want none with the cursor mid-word", got)
	}
	// ...and a fresh prompt drops the memo entirely.
	h.reset()
	if got := h.hint(line, 2); got != "ll → ls -la" {
		t.Errorf("hint = %q, want the alias after the memo is dropped", got)
	}
}

// TestCallee covers the bracket matcher directly, including the shapes that
// only show up mid-edit.
func TestCallee(t *testing.T) {
	cases := []struct{ prefix, want string }{
		{"", ""},
		{"foo(", "foo"},
		{"foo()", ""},
		{"foo(bar(", "bar"},
		{"foo(bar(),", "foo"},
		{"pkg.Fn(a, b", "pkg.Fn"},
		{"(", ""},         // a grouping paren has no name in front
		{"if (x + 1", ""}, // ...nor does a parenthesized expression
		{`f("(")`, ""},    // paren in a string: balanced, closed
		{`f("(" `, "f"},   // ...the real one is still open
		{`f('\'', `, "f"}, // escaped quote inside a char literal
		{"f(`)`, ", "f"},  // raw string: no escapes, closes on the backtick
		{"f(\"unterminated", "f"},
		{")))", ""}, // stray closers must not underflow the stack
	}
	for _, c := range cases {
		if got := callee(c.prefix); got != c.want {
			t.Errorf("callee(%q) = %q, want %q", c.prefix, got, c.want)
		}
	}
}

// TestTrailingSelector: the dotted word ending exactly at the cursor.
func TestTrailingSelector(t *testing.T) {
	cases := []struct{ prefix, want string }{
		{"strings.Split", "strings.Split"},
		{"x := strings.Split", "strings.Split"},
		{"strings.Split(", ""},
		{"strings.Split(s", "s"},
		{"", ""},
		{"  ", ""},
		{"a.b.C_2", "a.b.C_2"},
	}
	for _, c := range cases {
		if got := trailingSelector(c.prefix); got != c.want {
			t.Errorf("trailingSelector(%q) = %q, want %q", c.prefix, got, c.want)
		}
	}
}

// TestHintBadCursor: a cursor outside the buffer must not panic the display.
func TestHintBadCursor(t *testing.T) {
	h := newTestHinter(t)
	h.hint([]rune("ls"), 99)
	h.reset()
	h.hint([]rune("ls"), -1)
}

// TestHintExplain: the interactive half of --explain. The verdict shown is
// the classifier's own — same Kind and rule name a script's --explain
// prints — for the physical line the cursor is on, and it appears only when
// the flag asked for it.
func TestHintExplain(t *testing.T) {
	sess := runner.NewSession(runner.Options{Explain: io.Discard})
	h := newHinter(sess)
	if err := sess.Eval("count := 0"); err != nil { // for the declared-ident rule
		t.Fatalf("seeding an identifier: %v", err)
	}

	cases := []struct{ buf, want string }{
		{"ls -la‸", "shell · rule=default"},
		{"sh ls‸", "shell · rule=sh-prefix"},
		{"x := 1‸", "go · rule=define"},
		{"count++‸", "go · rule=declared-ident"},
		{"# a comment‸", ""}, // no rule decided it: skipped, as in batch output
		{"‸", ""},            // nothing typed yet
	}
	for _, c := range cases {
		if got := hintAt(t, h, c.buf); got != c.want {
			t.Errorf("hint(%q) = %q, want %q", c.buf, got, c.want)
		}
	}

	// A Go logical line spanning physical lines reports its span — the one
	// classifier decision the buffer itself does not show.
	if got := hintAt(t, h, "hosts := map[string]int{\n\t\"a\": 1,\n}‸"); got != "go 1-3 · rule=define" {
		t.Errorf("hint on a multi-line chunk = %q, want the span", got)
	}
	// A shell line continued with a backslash is likewise one chunk, and
	// the span is the only place that shows.
	if got := hintAt(t, h, "echo one \\\n  two‸"); got != "shell 1-2 · rule=default" {
		t.Errorf("hint on a continued shell line = %q, want the span", got)
	}
	// An unfinished unit reports the best-effort tail chunk rather than
	// nothing: mid-typing is when the question gets asked. An open BLOCK is
	// not that case — it classified fine and is merely deep — so the two
	// shapes report differently, which is the distinction worth seeing.
	if got := hintAt(t, h, "hosts := map[string]int{‸"); got != "go · rule=incomplete" {
		t.Errorf("hint on an open literal = %q, want the tail chunk's rule", got)
	}
	if got := hintAt(t, h, "func f() {‸"); got != "… func f"+hintSep+"go · rule=keyword" {
		t.Errorf("hint on an open block = %q, want the rule that decided it", got)
	}

	// Composed last, after the lanes that were already there.
	got := hintAt(t, h, "func f() {\n\tfmt.Println(‸")
	want := "fmt.Println(...any) (int, error)" + hintSep + "… func f" + hintSep + "go · rule=incomplete"
	if got != want {
		t.Errorf("hint =\n  %q\nwant\n  %q", got, want)
	}
}

// TestHintExplainOffByDefault: a session that was not asked to explain shows
// no verdict — the lane is a debugging aid, not ambient decoration.
func TestHintExplainOffByDefault(t *testing.T) {
	h := newTestHinter(t)
	if got := hintAt(t, h, "ls -la‸"); got != "" {
		t.Errorf("hint = %q, want none without --explain", got)
	}
	if h.sess.Explaining() {
		t.Error("a session with no Explain writer reports Explaining()")
	}
}
