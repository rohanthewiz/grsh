package interp

import (
	"strings"
	"testing"
)

// ---- for, in each of its shapes ----

func TestForClauseLoop(t *testing.T) {
	wantOut(t, `for i := 0; i < 3; i++ {
	fmt.Print(i)
}
fmt.Println()`, "012\n")
}

// A condition-only for is Go's while.
func TestForConditionOnly(t *testing.T) {
	wantOut(t, `n := 0
for n < 3 {
	n++
}
fmt.Println(n)`, "3\n")
}

// A bare for runs until something breaks out of it.
func TestForBareWithBreak(t *testing.T) {
	wantOut(t, `n := 0
for {
	n++
	if n == 4 {
		break
	}
}
fmt.Println(n)`, "4\n")
}

// break stops the loop; continue skips the rest of the body but still
// runs the post statement, so the loop advances rather than hanging.
func TestBreakAndContinue(t *testing.T) {
	wantOut(t, `for i := 0; i < 6; i++ {
	if i == 4 {
		break
	}
	if i%2 == 0 {
		continue
	}
	fmt.Print(i, " ")
}
fmt.Println()`, "1 3 \n")
}

// break binds to the innermost loop. grsh has no labels, so this is the
// only binding there is -- see the labeled-statement case below.
func TestBreakLeavesOnlyTheInnerLoop(t *testing.T) {
	wantOut(t, `for i := 0; i < 2; i++ {
	for j := 0; j < 5; j++ {
		if j == 1 {
			break
		}
		fmt.Print(i, j, " ")
	}
}
fmt.Println()`, "0 0 1 0 \n")
}

// A for with no condition and no break would spin forever; the evaluator
// caps iterations rather than wedging the session. Driving it to the real
// 100M limit is too slow for a test, so this only pins that the loop body
// executes and the guard does not fire early.
func TestForRunsToCompletionUnderTheIterationCap(t *testing.T) {
	wantOut(t, `n := 0
for i := 0; i < 10000; i++ {
	n += i
}
fmt.Println(n)`, "49995000\n")
}

// ---- range, over each supported operand kind ----

func TestRangeOverSlice(t *testing.T) {
	wantOut(t, `for i, v := range []string{"a", "b"} {
	fmt.Print(i, v, " ")
}
fmt.Println()`, "0a 1b \n")
}

func TestRangeKeyOnly(t *testing.T) {
	wantOut(t, `for i := range []int{7, 8, 9} {
	fmt.Print(i)
}
fmt.Println()`, "012\n")
}

// Ranging a string walks runes, with the key as a BYTE offset -- so a
// multi-byte rune leaves a gap in the indices, exactly as in Go.
func TestRangeOverStringWalksRunes(t *testing.T) {
	wantOut(t, `for i, r := range "héy" {
	fmt.Print(i, ":", string(r), " ")
}
fmt.Println()`, "0:h 1:é 3:y \n")
}

// range over an int counts from zero and has no value variable.
func TestRangeOverInt(t *testing.T) {
	wantOut(t, `for i := range 4 {
	fmt.Print(i)
}
fmt.Println()`, "0123\n")
}

// A nil operand is zero iterations, not a crash -- this is what makes
// `for _, x := range m["missing"]` safe.
func TestRangeOverNilIsZeroIterations(t *testing.T) {
	wantOut(t, `ran := false
for range nil {
	ran = true
}
fmt.Println(ran)`, "false\n")
}

func TestRangeOverUnsupportedKindIsAnError(t *testing.T) {
	wantErr(t, `for range 1.5 {
}`, "cannot range over")
}

// break and continue work the same inside range.
func TestRangeBreakAndContinue(t *testing.T) {
	wantOut(t, `for _, v := range []int{1, 2, 3, 4} {
	if v == 2 {
		continue
	}
	if v == 4 {
		break
	}
	fmt.Print(v, " ")
}
fmt.Println()`, "1 3 \n")
}

// The range variable must be a plain name; there is no destructuring.
func TestRangeVarMustBeAnIdentifier(t *testing.T) {
	wantErr(t, `xs := []int{1}
ys := []int{0}
for ys[0] = range xs {
}`, "range variable must be an identifier")
}

// ---- switch ----

func TestSwitchOnATag(t *testing.T) {
	wantOut(t, `for _, n := range []int{1, 2, 9} {
	switch n {
	case 1:
		fmt.Print("one ")
	case 2, 3:
		fmt.Print("few ")
	default:
		fmt.Print("many ")
	}
}
fmt.Println()`, "one few many \n")
}

// A tagless switch tests each case expression as a boolean -- Go's
// if/else-if chain in switch clothing.
func TestSwitchWithoutATag(t *testing.T) {
	wantOut(t, `n := 7
switch {
case n < 5:
	fmt.Println("small")
case n < 10:
	fmt.Println("medium")
default:
	fmt.Println("large")
}`, "medium\n")
}

// The default clause runs only when nothing matched, wherever it is
// written -- evalSwitch scans every case before falling back to it.
func TestSwitchDefaultRunsLastEvenWhenWrittenFirst(t *testing.T) {
	wantOut(t, `switch 2 {
default:
	fmt.Println("default")
case 2:
	fmt.Println("two")
}`, "two\n")
}

// A tagless case expression must be bool; an int is not truthy.
func TestSwitchTaglessCaseMustBeBool(t *testing.T) {
	wantErr(t, `switch {
case 1:
	fmt.Println("no")
}`, "switch case must be bool")
}

// break inside a case leaves the switch and nothing more -- it does not
// escape into an enclosing loop.
func TestSwitchBreakLeavesOnlyTheSwitch(t *testing.T) {
	wantOut(t, `for i := 0; i < 3; i++ {
	switch i {
	case 1:
		break
	}
	fmt.Print(i)
}
fmt.Println()`, "012\n")
}

// continue inside a case belongs to the enclosing loop and is passed
// outward: runCaseBody returns it rather than swallowing it like break.
func TestSwitchContinuePropagatesToTheLoop(t *testing.T) {
	wantOut(t, `for i := 0; i < 4; i++ {
	switch {
	case i%2 == 0:
		continue
	}
	fmt.Print(i)
}
fmt.Println()`, "13\n")
}

// Cases do not fall through by default (Go's rule), and the explicit
// fallthrough keyword is not implemented.
func TestSwitchDoesNotFallThrough(t *testing.T) {
	wantOut(t, `switch 1 {
case 1:
	fmt.Println("one")
case 2:
	fmt.Println("two")
}`, "one\n")
}

func TestFallthroughIsNotSupported(t *testing.T) {
	wantErr(t, `switch 1 {
case 1:
	fallthrough
case 2:
	fmt.Println("two")
}`, "not supported")
}

// ---- statements the evaluator declines ----
//
// These pin the surface: each is a construct a user might reasonably
// reach for, and each must fail with a message that says so rather than
// misbehaving quietly. A labeled break is the one that matters most --
// if labels ever parsed while BranchStmt kept ignoring the label, a
// `break outer` would silently leave only the inner loop.

func TestUnsupportedStatementsReportThemselves(t *testing.T) {
	tests := []struct {
		name, src string
	}{
		{"labeled statement", "outer:\nfor i := 0; i < 1; i++ {\n}"},
		{"goto", "goto done\ndone:\nfmt.Println(1)"},
		{"go statement", "go fmt.Println(1)"},
		{"select", "select {\n}"},
		{"type switch", "var v any = 1\nswitch v.(type) {\ncase int:\n\tfmt.Println(\"int\")\n}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := eval(t, tc.src, nil)
			if err == nil {
				t.Fatalf("expected an error, got output %q", out)
			}
			if !strings.Contains(err.Error(), "not supported") {
				t.Errorf("error %q should say the construct is not supported", err.Error())
			}
		})
	}
}

// ---- return at the top level ----

// The __main body is a script, so a top-level return ends the run
// without an error -- and skips everything after it.
func TestTopLevelReturnEndsTheScript(t *testing.T) {
	wantOut(t, `fmt.Println("before")
return
fmt.Println("after")`, "before\n")
}

// ---- defer ----

// Deferred calls run in LIFO order when the frame unwinds.
func TestDeferRunsInLIFOOrder(t *testing.T) {
	wantOut(t, `f := func() {
	defer fmt.Println("first")
	defer fmt.Println("second")
	fmt.Println("body")
}
f()`, "body\nsecond\nfirst\n")
}

// Go semantics: the callee and its ARGUMENTS are evaluated where the
// defer is written, not where it runs. A later mutation is not observed.
func TestDeferEvaluatesArgumentsImmediately(t *testing.T) {
	wantOut(t, `f := func() {
	x := 1
	defer fmt.Println("deferred saw", x)
	x = 99
	fmt.Println("body saw", x)
}
f()`, "body saw 99\ndeferred saw 1\n")
}

// A defer still runs when the function returns early.
func TestDeferRunsOnEarlyReturn(t *testing.T) {
	wantOut(t, `f := func(n int) int {
	defer fmt.Println("cleanup", n)
	if n > 0 {
		return n * 2
	}
	return 0
}
fmt.Println(f(3))`, "cleanup 3\n6\n")
}

// The top level has a frame of its own (Run pushes one), so a script can
// defer without wrapping itself in a function.
func TestDeferAtTopLevel(t *testing.T) {
	wantOut(t, `defer fmt.Println("last")
fmt.Println("first")`, "first\nlast\n")
}

// A defer that fails surfaces its error once the body has succeeded.
func TestDeferErrorSurfacesWhenTheBodySucceeded(t *testing.T) {
	out, err := eval(t, `f := func() {
	defer boom()
	fmt.Println("body ran")
}
f()`, map[string]any{
		"boom": func() { panic("kaboom") },
	})
	if out != "body ran\n" {
		t.Errorf("body output = %q, want %q", out, "body ran\n")
	}
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v; want the deferred call's failure to surface", err)
	}
}

// ...and the body's own error wins when both fail, so the original cause
// is not masked by cleanup noise.
func TestBodyErrorWinsOverDeferError(t *testing.T) {
	_, err := eval(t, `f := func() {
	defer boom()
	fmt.Println(1 / 0)
}
f()`, map[string]any{
		"boom": func() { panic("kaboom") },
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("err = %v; want the body's division error, not the defer's", err)
	}
}

// Deferred calls still run when the body fails -- cleanup must not be
// skipped just because something went wrong.
func TestDeferRunsAfterABodyError(t *testing.T) {
	out, _ := eval(t, `f := func() {
	defer fmt.Println("cleanup")
	fmt.Println(1 / 0)
}
f()`, nil)
	if out != "cleanup\n" {
		t.Errorf("output = %q; the defer must still run after a body error", out)
	}
}
