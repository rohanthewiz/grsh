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

// `?m` on a map with a NaN key used to panic the REPL, for the reason a
// range over one used to die: the inspector walked MapKeys and fetched
// each value with MapIndex, and a NaN key is a live entry no lookup can
// find. The values are read beside the keys now.
//
// Each value here is distinct, so a value landing under the wrong key is
// a failure and not just a survived panic -- the sort moves the keys, and
// the values have to move with them.
//
// INSPECTED EIGHT TIMES, from eight fresh maps, because the permutation
// the sort starts from is the map's own randomised one: five keys already
// in order is a 1-in-120 accident, and one run of this test would report
// a dropped Swap as a pass that often.
func TestInspectAMapWithANaNKey(t *testing.T) {
	const want = "m: map[float64]int (len 5) {\n  -3.5: 3\n  0   : 4\n  1.5 : 1\n  9.25: 5\n  NaN : 2\n}"
	for run := 0; run < 8; run++ {
		got := inspect(t, `m := map[float64]int{1.5: 1, math.NaN(): 2, -3.5: 3, 0.0: 4, 9.25: 5}`, "m")
		if got != want {
			t.Fatalf("run %d got:\n%s\nwant:\n%s", run, got, want)
		}
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

// A long string in a ROW is cut like any other value. This was the gap
// the item cap left open: oneLine quoted strings and returned before the
// width bound, so twenty rows could still be twenty megabytes.
//
// The cut lands in the content and the result is re-quoted, so the row
// still reads as a closed string with more after it.
func TestInspectTruncatesLongStringsInRows(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := inspect(t, "xs := []string{\""+long+"\"}", "xs")
	if strings.Contains(got, long) {
		t.Fatalf("the whole string printed; the row budget did not apply:\n%s", got)
	}
	if !strings.Contains(got, `"aaa`) || !strings.Contains(got, `"...`) {
		t.Errorf("want a closed quote followed by an ellipsis; got:\n%s", got)
	}
	assertRowsWithinBudget(t, got)
}

// Every rendering path is bounded, not just the string one: a struct's
// String form, a closure's, and a nested container's all go through the
// same budget.
func TestInspectBoundsEveryRowKind(t *testing.T) {
	long := strings.Repeat("z", 200)
	cases := []struct{ name, body, target string }{
		{"struct", "type P struct {\n\tS string\n}\nxs := []any{P{\"" + long + "\"}}", "xs"},
		{"closure field", "type P struct {\n\tS string\n}\np := P{\"" + long + "\"}", "p"},
		{"nested slice", "xs := []any{make([]int, 200)}", "xs"},
		{"map value", "m := map[string]string{\"k\": \"" + long + "\"}", "m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertRowsWithinBudget(t, inspect(t, c.body, c.target))
		})
	}
}

// A cut never lands mid-rune. A byte-slice truncation of a multi-byte
// string produces U+FFFD, which reads as corrupted data rather than as
// elision -- the inspector misdescribing the value it exists to describe.
//
// Both cutters are exercised, because they cut differently and only one
// of them is reachable with a string: a bare string row goes through
// quoteBounded (which cuts a []rune), while a struct's String form --
// where %v prints the field RAW, unquoted -- goes through truncated
// (which walks rune starts). Testing only the first leaves the second
// free to cut bytes.
func TestInspectTruncationIsRuneAligned(t *testing.T) {
	// 200 three-byte runes: any byte-indexed cut at 57 lands inside one.
	wide := strings.Repeat("世", 200)
	for _, c := range []struct{ name, body, target string }{
		{"quoted string row", `xs := []any{"` + wide + `"}`, "xs"},
		{"struct String form", "type P struct {\n\tS string\n}\nxs := []any{P{\"" + wide + "\"}}", "xs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := inspect(t, c.body, c.target)
			if strings.ContainsRune(got, '\uFFFD') {
				t.Errorf("the cut split a rune:\n%s", got)
			}
			assertRowsWithinBudget(t, got)
		})
	}
}

// The top-level `?s` view keeps a much wider budget -- seeing the content
// is the point of asking -- but it is still a budget, and the (len N)
// beside it tells the reader what was withheld.
func TestInspectTopLevelStringIsWidelyButFinitelyBounded(t *testing.T) {
	short := strings.Repeat("a", 500)
	got := inspect(t, "s := \""+short+"\"", "s")
	if !strings.Contains(got, short) {
		t.Errorf("500 chars should print in full at top level; got:\n%s", got)
	}

	huge := strings.Repeat("a", inspectMaxString*2)
	got = inspect(t, "s := \""+huge+"\"", "s")
	if strings.Contains(got, huge) {
		t.Fatalf("an unbounded string printed in full (%d chars)", len(got))
	}
	if !strings.Contains(got, "(len 4096)") {
		t.Errorf("the true length must still be reported; got the prefix:\n%.80s", got)
	}
	if n := len([]rune(got)); n > inspectMaxString+64 { // + the name/type preamble
		t.Errorf("top-level rendering is %d runes, past its budget", n)
	}
}

// Column padding is measured in runes because fmt's `%-*s` pads in runes.
//
// The symptom of counting bytes is NOT misalignment -- fmt pads every row
// to the same width whatever number it is handed, so the rows still line
// up. It is a column wider than any key in it: two extra spaces per
// three-byte rune, a gap with nothing in it. So the assertion is that the
// padding is MINIMAL, not merely equal.
func TestInspectMapPaddingIsMinimal(t *testing.T) {
	// Rendered keys are `"a"` and `"世"` -- 3 runes each, but 3 and 5
	// bytes. A byte count therefore pads both to 5, two too far.
	got := inspect(t, `m := map[string]int{"a": 1, "世": 2}`, "m")
	widest := 0
	var cols []int
	for _, line := range strings.Split(got, "\n") {
		i := strings.Index(line, ": ")
		if !strings.HasPrefix(line, "  ") || i < 0 {
			continue
		}
		key := line[2:i]
		cols = append(cols, len([]rune(key)))
		if n := len([]rune(strings.TrimRight(key, " "))); n > widest {
			widest = n
		}
	}
	if len(cols) != 2 {
		t.Fatalf("expected two entry rows, got %d:\n%s", len(cols), got)
	}
	for _, c := range cols {
		if c != widest {
			t.Errorf("key column is %d runes wide for a widest key of %d:\n%s", c, widest, got)
		}
	}
}

// assertRowsWithinBudget checks every indented row against the width
// budget. Header and closer are exempt: they carry the variable's name
// and type, which the user asked for by name and cannot be surprised by.
func assertRowsWithinBudget(t *testing.T, got string) {
	t.Helper()
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		// The row is the padded label plus the rendered value; only the
		// value carries the budget, so allow the label its width.
		if n := len([]rune(line)); n > inspectMaxWidth+32 {
			t.Errorf("row is %d runes, past the budget: %q", n, line)
		}
	}
}

// A container of script structs must not print grsh's own internals.
// []P is stored as []*interp.StructVal, so %T would leak the erasure
// into the one surface whose whole job is describing a value.
func TestInspectNamesScriptStructContainers(t *testing.T) {
	got := inspect(t, `type Job struct {
	Name string
}
xs := []Job{{"a"}, {"b"}}`, "xs")
	if !strings.HasPrefix(got, "xs: []Job (len 2) [") {
		t.Errorf("slice header = %q, want a []Job header", firstLine(got))
	}
	got = inspect(t, `type Job struct {
	Name string
}
m := map[string]Job{"k": {"a"}}`, "m")
	if !strings.HasPrefix(got, "m: map[string]Job (len 1) {") {
		t.Errorf("map header = %q, want a map[string]Job header", firstLine(got))
	}
	// An EMPTY container used to be the one case with no element to read
	// the name off. Its element TYPE names the struct now, so the header
	// is exact without any instance to consult.
	got = inspect(t, `type Job struct {
	Name string
}
xs := []Job{}`, "xs")
	if !strings.HasPrefix(got, "xs: []Job (len 0) [") {
		t.Errorf("empty slice header = %q, want a []Job header", firstLine(got))
	}
	got = inspect(t, `type Job struct {
	Name string
}
m := map[string]Job{}`, "m")
	if !strings.HasPrefix(got, "m: map[string]Job (len 0) {") {
		t.Errorf("empty map header = %q, want a map[string]Job header", firstLine(got))
	}
	// Nothing else moved: a native container still renders through %T.
	got = inspect(t, `xs := []int{1}`, "xs")
	if !strings.HasPrefix(got, "xs: []int (len 1) [") {
		t.Errorf("native slice header = %q", firstLine(got))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- InspectParts: the same data, without the presentation ----
//
// The tutor's `var` verifier grades type and value independently, so it
// reads these rather than parsing Inspect's rendered line. The two must
// stay genuinely separate: sharing the string would make a lesson's
// regexp depend on the inspector's quoting, its "(len N)", and its
// 60-rune elision.

func TestInspectPartsSplitsTypeAndValue(t *testing.T) {
	tests := []struct {
		name, src, binding, typ, val string
	}{
		{"int", `n := 42`, "n", "int", "42"},
		{"float", `f := 1.5`, "f", "float64", "1.5"},
		{"bool", `b := true`, "b", "bool", "true"},
		// A string's value is its RAW contents: no quotes, no length
		// prefix. A lesson writes value=^ada$, not value=^"ada"$.
		{"string", `s := "ada"`, "s", "string", "ada"},
		{"empty string", `s := ""`, "s", "string", ""},
		// A nil binding has no type to report, matching Inspect's own
		// special case for it.
		{"nil", `z := nil`, "z", "", "nil"},
		{"slice", `xs := []string{"a", "b"}`, "xs", "[]string", "[a b]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, out, err := evalKeep(t, tc.src, nil)
			if err != nil {
				t.Fatalf("run: %v\noutput:\n%s", err, out)
			}
			typ, val, ok := in.InspectParts(tc.binding)
			if !ok {
				t.Fatalf("InspectParts(%q) reported no such binding", tc.binding)
			}
			if typ != tc.typ || val != tc.val {
				t.Errorf("got type %q value %q, want type %q value %q", typ, val, tc.typ, tc.val)
			}
		})
	}
}

func TestInspectPartsUnknownName(t *testing.T) {
	in, _, err := evalKeep(t, `n := 1`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := in.InspectParts("nope"); ok {
		t.Error("InspectParts reported a binding that does not exist")
	}
}

// TestInspectPartsIsUntruncated is the whole reason this accessor exists
// rather than the verifier scraping Inspect: the human-facing renderer
// elides past inspectMaxWidth, and a grader that matched the elided
// string would pass any answer whose first 60 runes were right.
func TestInspectPartsIsUntruncated(t *testing.T) {
	long := strings.Repeat("x", inspectMaxWidth*2) + "END"
	in, _, err := evalKeep(t, `s := "`+long+`"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, val, ok := in.InspectParts("s")
	if !ok {
		t.Fatal("no binding")
	}
	if val != long {
		t.Errorf("value was transformed: got %d runes ending %q, want the raw %d", len(val), val[max(0, len(val)-8):], len(long))
	}
}
