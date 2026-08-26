package interp

import (
	"strings"
	"testing"
)

// ---- the REPL's ?name inspector ----
//
// Inspect reads the GLOBAL scope only: it answers the REPL's `?name` at a
// prompt, where every binding the user made is global by construction.
// These pin the rendering because it is a user-facing surface with no
// other coverage -- internal/repl tests the command dispatch, not what
// comes back.

// inspect runs a script and inspects one global, failing on a run error.
func inspect(t *testing.T, body, name string) string {
	t.Helper()
	in, out, err := evalKeep(t, body, nil)
	if err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, out)
	}
	s, ok := in.Inspect(name)
	if !ok {
		t.Fatalf("Inspect(%q) reported no such binding", name)
	}
	return s
}

func TestInspectUnknownNameReportsNotFound(t *testing.T) {
	in, _, err := evalKeep(t, `x := 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := in.Inspect("nope"); ok {
		t.Errorf("Inspect(nope) = %q, true; want not found", s)
	}
}

// Locals are invisible: only the global scope is reachable, which is the
// same scope a REPL user is typing into.
func TestInspectSeesOnlyGlobals(t *testing.T) {
	in, _, err := evalKeep(t, `g := 1
{
	local := 2
	_ = local
}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := in.Inspect("g"); !ok {
		t.Error("Inspect(g) did not find a global")
	}
	if _, ok := in.Inspect("local"); ok {
		t.Error("Inspect(local) reached into a block scope")
	}
}

// Scalars render as type and value on one line.
func TestInspectScalars(t *testing.T) {
	tests := []struct {
		name, src, binding, want string
	}{
		{"int", `n := 42`, "n", "n: int = 42"},
		{"float", `f := 1.5`, "f", "f: float64 = 1.5"},
		{"bool", `b := true`, "b", "b: bool = true"},
		// A nil binding has no type to report, so the form differs.
		{"nil", `z := nil`, "z", "z = nil"},
		// Strings carry a length and are quoted, so trailing whitespace
		// and empties are visible rather than invisible.
		{"string", `s := "abc"`, "s", `s: string (len 3) = "abc"`},
		{"empty string", `s := ""`, "s", `s: string (len 0) = ""`},
		{"string len is bytes", `s := "é"`, "s", `s: string (len 2) = "é"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspect(t, tc.src, tc.binding); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInspectError(t *testing.T) {
	in, _, err := evalKeep(t, `_, e := mayFail()`, map[string]any{
		"mayFail": func() (string, error) { return "", simpleErr("boom") },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := in.Inspect("e")
	if !ok {
		t.Fatal("Inspect(e) not found")
	}
	if got != "e: error = boom" {
		t.Errorf("got %q, want %q", got, "e: error = boom")
	}
}

// A closure renders its name and parameter list, with variadics marked.
// The signature comes off the AST, so it reflects what the user wrote.
func TestInspectClosureSignature(t *testing.T) {
	if got := inspect(t, `fn := func(a int, b string) int { return a }`, "fn"); got != "fn = func fn(a, b)" {
		t.Errorf("got %q", got)
	}
	if got := inspect(t, `fn := func(a int, rest ...string) int { return a }`, "fn"); got != "fn = func fn(a, ...rest)" {
		t.Errorf("got %q", got)
	}
	// Grouped parameters flatten to one entry each.
	if got := inspect(t, `fn := func(a, b, c int) int { return a }`, "fn"); got != "fn = func fn(a, b, c)" {
		t.Errorf("got %q", got)
	}
	// An unnamed parameter has nothing to show but its position.
	if got := inspect(t, `fn := func(int) int { return 0 }`, "fn"); got != "fn = func fn(_)" {
		t.Errorf("got %q", got)
	}
}

// A slice lists its elements with indices, right-aligned so the column
// stays put past nine.
func TestInspectSlice(t *testing.T) {
	got := inspect(t, `xs := []int{1, 2, 3}`, "xs")
	want := "xs: []int (len 3) [\n    0: 1\n    1: 2\n    2: 3\n]"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Long containers elide: the inspector is a prompt convenience and must
// not scroll the terminal away.
func TestInspectSliceElidesPastTheItemCap(t *testing.T) {
	got := inspect(t, `xs := make([]int, 25)`, "xs")
	if strings.Count(got, "\n") != inspectMaxItems+2 { // header + items + summary
		t.Errorf("expected %d item lines plus a summary, got:\n%s", inspectMaxItems, got)
	}
	if !strings.Contains(got, "... 5 more") {
		t.Errorf("missing the elision summary:\n%s", got)
	}
	if strings.Contains(got, "  20: ") {
		t.Errorf("item 20 was printed past the cap:\n%s", got)
	}
}

// Map entries are sorted by their RENDERED key and the keys are padded to
// a common width, so repeated inspections of the same map line up and
// read the same way every time.
func TestInspectMapIsSortedAndAligned(t *testing.T) {
	got := inspect(t, `m := map[string]int{"bb": 2, "a": 1}`, "m")
	want := "m: map[string]int (len 2) {\n  \"a\" : 1\n  \"bb\": 2\n}"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestInspectMapElidesPastTheItemCap(t *testing.T) {
	got := inspect(t, `m := map[string]int{}
for i := range 25 {
	m[fmt.Sprint(i)] = i
}`, "m")
	if !strings.Contains(got, "... 5 more") {
		t.Errorf("missing the elision summary:\n%s", got)
	}
}

// A struct shows its fields in declaration order, names padded, values
// rendered compactly -- strings quoted so an empty field is visible.
func TestInspectStruct(t *testing.T) {
	got := inspect(t, `type P struct {
	X    int
	Name string
}
p := P{7, "hi"}`, "p")
	want := "p: P {\n  X     7\n  Name  \"hi\"\n}"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Nested values inside a container collapse to one line each; a struct
// element uses its String form rather than expanding.
func TestInspectNestedValuesAreOneLine(t *testing.T) {
	got := inspect(t, `type P struct {
	X int
}
xs := []any{P{1}, "s", nil}`, "xs")
	for _, want := range []string{"P{X: 1}", `"s"`, "nil"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering %q missing from:\n%s", want, got)
		}
	}
	// One line per element and nothing more.
	if n := strings.Count(got, "\n"); n != 4 {
		t.Errorf("expected 3 item lines, got %d:\n%s", n-1, got)
	}
}

// Non-string values longer than the row budget are truncated with an
// ellipsis.
func TestInspectLongValuesAreTruncated(t *testing.T) {
	got := inspect(t, `xs := []any{[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25}}`, "xs")
	if !strings.Contains(got, "...") {
		t.Errorf("a long element was not truncated:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 80 {
			t.Errorf("row exceeds the budget (%d chars): %q", len(line), line)
		}
	}
}

// KNOWN GAP, pinned as it stands.
//
// oneLine quotes strings and returns before the 60-char truncation, so a
// string is never shortened -- only non-string values are. The item cap
// therefore bounds how many rows print but not how wide one row gets,
// which undercuts the "never dump megabytes" intent stated on
// inspectMaxItems: one long string field prints in full.
func TestInspectDoesNotTruncateLongStrings(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := inspect(t, "xs := []string{\""+long+"\"}", "xs")
	if !strings.Contains(got, long) {
		t.Fatalf("expected the whole string to print; got:\n%s", got)
	}
	if strings.Contains(got, "...") {
		t.Errorf("strings are truncated after all -- update this test and the note above it")
	}
}
