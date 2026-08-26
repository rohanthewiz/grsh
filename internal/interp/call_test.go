package interp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// ---- closures ----

func TestClosureCallAndReturn(t *testing.T) {
	wantOut(t, `add := func(a int, b int) int { return a + b }
fmt.Println(add(2, 3))`, "5\n")
}

// Grouped parameters (a, b int) flatten to one name each; flattenParams
// is what keeps the arity check honest for that spelling.
func TestClosureGroupedParams(t *testing.T) {
	wantOut(t, `add := func(a, b, c int) int { return a + b + c }
fmt.Println(add(1, 2, 3))`, "6\n")
}

// A function with no return statement yields no values, which is only
// usable in statement position.
func TestClosureWithNoReturnHasNoValue(t *testing.T) {
	wantOut(t, `f := func() { fmt.Println("side effect") }
f()`, "side effect\n")
	// The assignment path reports it as a value-count mismatch...
	wantErr(t, `f := func() { }
x := f()
_ = x`, "assignment mismatch: 1 variables but 0 values")
	// ...and an expression context as an absent value.
	wantErr(t, `f := func() { }
fmt.Println(f())`, "expression has no value")
}

func TestClosureMultipleReturnValues(t *testing.T) {
	wantOut(t, `split := func() (int, int) { return 1, 2 }
a, b := split()
fmt.Println(a, b)`, "1 2\n")
}

// Variadic parameters collect the surplus into a slice, and an empty
// surplus is an empty slice rather than nil-and-a-panic.
func TestClosureVariadic(t *testing.T) {
	wantOut(t, `sum := func(label string, ns ...int) string {
	total := 0
	for _, n := range ns {
		total += n
	}
	return label + fmt.Sprint(total)
}
fmt.Println(sum("t:", 1, 2, 3))
fmt.Println(sum("empty:"))`, "t:6\nempty:0\n")
}

// Arity errors name the function when it has a name, so the message
// points at the call the user wrote.
func TestClosureArityErrors(t *testing.T) {
	wantErr(t, `add := func(a, b int) int { return a + b }
fmt.Println(add(1))`, "add expects 2 argument(s), got 1")
	wantErr(t, `add := func(a, b int) int { return a + b }
fmt.Println(add(1, 2, 3))`, "add expects 2 argument(s), got 3")
	// An anonymous closure has no name to report.
	wantErr(t, `fmt.Println(func(a int) int { return a }())`, "function expects 1 argument(s), got 0")
	// Variadic arity is a minimum, and it is still checked.
	wantErr(t, `f := func(a int, rest ...int) int { return a }
fmt.Println(f())`, "expects 2 argument(s), got 0")
}

// A func literal can be called where it is written.
func TestImmediatelyInvokedFuncLit(t *testing.T) {
	wantOut(t, `fmt.Println(func(n int) int { return n * n }(6))`, "36\n")
}

// Functions are values: passable, storable, returnable.
func TestFunctionsAreValues(t *testing.T) {
	wantOut(t, `apply := func(f any, n int) any { return f }
double := func(n int) int { return n * 2 }
g := apply(double, 0)
fmt.Println(g(21))`, "42\n")
}

// Run hoists every top-level `name := func(...)` before executing the
// body, so a function may call one defined further down -- and two may
// call each other.
func TestTopLevelFunctionsAreHoisted(t *testing.T) {
	wantOut(t, `main_ish := func() { fmt.Println(later()) }
main_ish()
later := func() string { return "defined below" }`, "defined below\n")
}

func TestMutualRecursion(t *testing.T) {
	wantOut(t, `even := func(n int) bool {
	if n == 0 {
		return true
	}
	return odd(n - 1)
}
odd := func(n int) bool {
	if n == 0 {
		return false
	}
	return even(n - 1)
}
fmt.Println(even(10), odd(10))`, "true false\n")
}

// Hoisting applies only to the top level: a func assigned inside a block
// is defined when that statement runs, so a forward call fails.
func TestNestedFunctionsAreNotHoisted(t *testing.T) {
	wantErr(t, `{
	caller := func() { fmt.Println(callee()) }
	caller()
	callee := func() string { return "x" }
	_ = callee
}`, "undefined: callee")
}

// Runaway recursion is caught by a depth counter rather than by blowing
// the host's stack, which would take the whole session with it.
//
// This case used to take about two seconds, and the time was not the
// descent: nine thousand levels of legal recursion run in ~14ms. The
// cost was the unwind, where callClosure wrapped the error with an
// "in_func" field at every level while serr copied the accumulated field
// list on each wrap. A runaway typed at the prompt froze the shell for
// seconds before reporting a 40-character message. The chain is now
// rendered and attached once (see callchain_test.go), and this runs in
// about 10ms.
func TestRunawayRecursionIsCaught(t *testing.T) {
	wantErr(t, `f := func(n int) int { return f(n + 1) }
fmt.Println(f(0))`, "call depth exceeded")
}

// ---- the reflect boundary: calling registered Go functions ----

func TestCallGoFunction(t *testing.T) {
	out, err := eval(t, `fmt.Println(shout("hi"), twice(21))`, map[string]any{
		"shout": strings.ToUpper,
		"twice": func(n int) int { return n * 2 },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "HI 42\n" {
		t.Errorf("got %q, want %q", out, "HI 42\n")
	}
}

func TestCallGoFunctionArityErrors(t *testing.T) {
	extra := map[string]any{
		"two":  func(a, b int) int { return a + b },
		"vari": func(a int, rest ...int) int { return a },
	}
	for _, tc := range []struct{ src, want string }{
		{`two(1)`, "two expects 2 argument(s), got 1"},
		{`two(1, 2, 3)`, "two expects 2 argument(s), got 3"},
		{`vari()`, "vari expects at least 1 argument(s), got 0"},
	} {
		_, err := eval(t, tc.src, extra)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.src, err, tc.want)
		}
	}
}

// A Go function that panics must not take the session down: callReflect
// recovers and reports it as a positioned script error naming the callee.
func TestPanicInGoFunctionBecomesAnError(t *testing.T) {
	_, err := eval(t, `boom()`, map[string]any{
		"boom": func() { panic("exploded") },
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "panic in boom") || !strings.Contains(err.Error(), "exploded") {
		t.Errorf("err = %v; want it to name the function and the panic", err)
	}
}

// A (T, error) return in a single-target assignment aborts the script on
// a non-nil error, so scripts read like Go without the error dance...
func TestSingleTargetAssignAbortsOnError(t *testing.T) {
	_, err := eval(t, `v := mayFail(true)
fmt.Println(v)`, map[string]any{
		"mayFail": func(bad bool) (string, error) {
			if bad {
				return "", errFor("it failed")
			}
			return "ok", nil
		},
		"errFor": errFor,
	})
	if err == nil || !strings.Contains(err.Error(), "it failed") {
		t.Fatalf("err = %v; want the callee's error to abort the script", err)
	}
}

// ...and a nil error is dropped, leaving just the value.
func TestSingleTargetAssignDropsNilError(t *testing.T) {
	out, err := eval(t, `v := mayFail(false)
fmt.Println(v)`, map[string]any{
		"mayFail": func(bad bool) (string, error) { return "ok", nil },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "ok\n" {
		t.Errorf("got %q, want %q", out, "ok\n")
	}
}

// Naming the error explicitly hands control back to the script.
func TestTwoTargetAssignExposesTheError(t *testing.T) {
	out, err := eval(t, `v, err := mayFail(true)
fmt.Println(v == "", err != nil)`, map[string]any{
		"mayFail": func(bad bool) (string, error) { return "", errFor("nope") },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "true true\n" {
		t.Errorf("got %q, want %q", out, "true true\n")
	}
}

// A call written as a bare statement aborts on a non-nil error, exactly
// as the single-target assignment above does.
//
// This is grsh's rule, not Go's. Go discards the error here -- but Go
// also does not abort on `v := mayFail()`, and grsh does, deliberately.
// Whether a failure is noticed must not depend on whether the caller
// happened to want the value.
func TestErrorFromAStatementCallAborts(t *testing.T) {
	_, err := eval(t, `mayFail()
fmt.Println("still here")`, map[string]any{
		"mayFail": func() error { return errFor("it failed") },
	})
	if err == nil {
		t.Fatal("expected the error to abort the script")
	}
	if !strings.Contains(err.Error(), "it failed") {
		t.Errorf("err = %v; want the callee's error", err)
	}
}

// The (T, error) shape aborts on the same rule -- it is the LAST result
// that is inspected, so both arities behave alike.
func TestErrorFromAStatementCallAbortsWithAValueToo(t *testing.T) {
	_, err := eval(t, `mayFail()`, map[string]any{
		"mayFail": func() (string, error) { return "v", errFor("it failed") },
	})
	if err == nil || !strings.Contains(err.Error(), "it failed") {
		t.Fatalf("err = %v; want the callee's error to abort", err)
	}
}

// A nil error is not an abort, and the results are still discarded.
func TestStatementCallWithNilErrorProceeds(t *testing.T) {
	out, err := eval(t, `ok()
fmt.Println("still here")`, map[string]any{
		"ok": func() (string, error) { return "v", nil },
	})
	if err != nil {
		t.Fatalf("run: %v; a nil error must not abort", err)
	}
	if out != "still here\n" {
		t.Errorf("got %q, want %q", out, "still here\n")
	}
}

// Naming the error is the opt-out, in both spellings. `_ = f()` discards
// it explicitly; `err := f()` hands the script control, as the two-target
// assignment does.
func TestStatementCallErrorCanBeOptedOutOf(t *testing.T) {
	for _, body := range []string{
		"_ = mayFail()\nfmt.Println(\"still here\")",
		"err := mayFail()\n_ = err\nfmt.Println(\"still here\")",
	} {
		out, err := eval(t, body, map[string]any{
			"mayFail": func() error { return errFor("ignored") },
		})
		if err != nil {
			t.Fatalf("run: %v\nsource:\n%s", err, body)
		}
		if out != "still here\n" {
			t.Errorf("got %q, want %q\nsource:\n%s", out, "still here\n", body)
		}
	}
}

// A call returning something that is not an error is untouched: only the
// last result is inspected, and only when it is a non-nil error.
func TestStatementCallWithNonErrorResultIsUnaffected(t *testing.T) {
	out, err := eval(t, `count()
fmt.Println("still here")`, map[string]any{
		"count": func() int { return 3 },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "still here\n" {
		t.Errorf("got %q, want %q", out, "still here\n")
	}
}

// ---- convertTo: adapting script values to Go parameter types ----

// Go's int-to-string conversion produces a code point, which is almost
// never what a script means. convertTo refuses it and names the fix.
func TestIntArgumentToStringParamIsRefused(t *testing.T) {
	_, err := eval(t, `shout(65)`, map[string]any{"shout": strings.ToUpper})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "strconv.Itoa") {
		t.Errorf("err = %v; want the message to suggest strconv.Itoa", err)
	}
}

// Numeric widths convert freely: a script int reaches an int64 or
// float64 parameter without ceremony.
func TestNumericArgumentsConvert(t *testing.T) {
	out, err := eval(t, `fmt.Println(wide(7), precise(7))`, map[string]any{
		"wide":    func(n int64) int64 { return n * 2 },
		"precise": func(f float64) float64 { return f / 2 },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "14 3.5\n" {
		t.Errorf("got %q, want %q", out, "14 3.5\n")
	}
}

// A script slice is []any-flavored wherever it was built dynamically;
// convertTo rebuilds it element-wise for a typed parameter.
func TestSliceArgumentConvertsElementwise(t *testing.T) {
	out, err := eval(t, `xs := []any{"a", "b"}
fmt.Println(joined(xs))`, map[string]any{
		"joined": func(ss []string) string { return strings.Join(ss, "-") },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "a-b\n" {
		t.Errorf("got %q, want %q", out, "a-b\n")
	}
}

// A nil script value becomes the parameter type's zero value rather than
// an invalid reflect.Value that would panic inside Call.
func TestNilArgumentBecomesTheZeroValue(t *testing.T) {
	out, err := eval(t, `fmt.Println(describe(nil))`, map[string]any{
		"describe": func(s string) string { return "[" + s + "]" },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "[]\n" {
		t.Errorf("got %q, want %q", out, "[]\n")
	}
}

// A value with no path to the parameter type reports both types.
func TestUnconvertibleArgumentIsAnError(t *testing.T) {
	_, err := eval(t, `takesSlice("not a slice")`, map[string]any{
		"takesSlice": func(xs []int) int { return len(xs) },
	})
	if err == nil || !strings.Contains(err.Error(), "cannot use") {
		t.Fatalf("err = %v; want a cannot-use message", err)
	}
}

// Calling a non-function says so instead of failing inside reflect.
func TestCallingANonFunctionIsAnError(t *testing.T) {
	wantErr(t, `x := 5
x()`, "not callable")
}

// The spread form is recognized and declined rather than silently
// passing the slice as one argument.
func TestSpreadCallIsNotSupported(t *testing.T) {
	wantErr(t, `f := func(a, b int) int { return a + b }
xs := []int{1, 2}
fmt.Println(f(xs...))`, "spread calls")
}

// ---- Go builtins ----

func TestBuiltinLenAndCap(t *testing.T) {
	wantOut(t, `xs := make([]int, 2, 8)
m := map[string]int{"a": 1}
fmt.Println(len("abc"), len(xs), cap(xs), len(m))`, "3 2 8 1\n")
}

// len(nil) is 0 rather than an error: it keeps `len(m["missing"])` safe.
func TestBuiltinLenOfNil(t *testing.T) {
	wantOut(t, `fmt.Println(len(nil))`, "0\n")
}

// cap has no meaning on strings or maps and says so.
func TestBuiltinCapRejectsStringsAndMaps(t *testing.T) {
	wantErr(t, `fmt.Println(cap("abc"))`, "cap is not defined on string")
	wantErr(t, `m := map[string]int{}
fmt.Println(cap(m))`, "cap is not defined on map")
}

func TestBuiltinLenOfANumberIsAnError(t *testing.T) {
	wantErr(t, `fmt.Println(len(5))`, "len is not defined on int")
}

func TestBuiltinAppend(t *testing.T) {
	wantOut(t, `xs := []int{1}
xs = append(xs, 2, 3)
fmt.Println(xs)`, "[1 2 3]\n")
}

// append onto a nil base starts a fresh slice, so `var xs []int` -- and
// an unset map entry -- accumulate without a make().
func TestBuiltinAppendOntoNil(t *testing.T) {
	wantOut(t, `xs := append(nil, 1, 2)
fmt.Println(len(xs), xs[0], xs[1])`, "2 1 2\n")
}

func TestBuiltinAppendRejectsNonSlice(t *testing.T) {
	wantErr(t, `fmt.Println(append("abc", 1))`, "append target must be a slice")
}

func TestBuiltinCopy(t *testing.T) {
	wantOut(t, `dst := make([]int, 2)
n := copy(dst, []int{7, 8, 9})
fmt.Println(n, dst)`, "2 [7 8]\n")
}

// reflect.Copy panics on mismatched element types, so the check happens
// before the call.
func TestBuiltinCopyRejectsMismatchedElements(t *testing.T) {
	wantErr(t, `dst := make([]int, 2)
copy(dst, []string{"a"})`, "cannot copy")
}

func TestBuiltinMinMax(t *testing.T) {
	wantOut(t, `fmt.Println(min(3, 1, 2), max(3, 1, 2))`, "1 3\n")
	// They work on anything binaryOp can order, strings included.
	wantOut(t, `fmt.Println(min("pear", "apple"), max("pear", "apple"))`, "apple pear\n")
	// One argument is the trivial case, not an error.
	wantOut(t, `fmt.Println(min(4), max(4))`, "4 4\n")
}

func TestBuiltinMake(t *testing.T) {
	wantOut(t, `xs := make([]string, 2)
m := make(map[string]int)
m["a"] = 1
fmt.Printf("%q %d\n", xs, len(m))`, "[\"\" \"\"] 1\n")
}

// reflect.MakeSlice panics on nonsense sizes; make validates first so the
// script gets a positioned error.
func TestBuiltinMakeRejectsBadSizes(t *testing.T) {
	wantErr(t, `n := -1
make([]int, n)`, "negative length")
	wantErr(t, `make([]int, 5, 2)`, "length larger than capacity")
}

func TestBuiltinMakeRejectsUnsupportedTypes(t *testing.T) {
	wantErr(t, `make(chan int)`, "")
	wantErr(t, `make([3]int)`, "fixed-size arrays are not supported")
}

// iff is grsh's lazy ternary: the condition decides, and only the chosen
// arm is evaluated -- so the dead arm may be an expression that would
// fail. That laziness is the whole reason it is an intrinsic rather than
// an ordinary function.
func TestBuiltinIffIsLazy(t *testing.T) {
	wantOut(t, `xs := []string{}
fmt.Println(iff(len(xs) > 0, xs[0], "none"))`, "none\n")
	wantOut(t, `xs := []string{"first"}
fmt.Println(iff(len(xs) > 0, xs[0], "none"))`, "first\n")
}

func TestBuiltinIffArity(t *testing.T) {
	wantErr(t, `fmt.Println(iff(true, 1))`, "iff expects (condition, thenValue, elseValue)")
}

// Builtins are only builtins while unshadowed: a script that binds the
// name gets its own value, which is what keeps user code from colliding
// with a surface that grows every milestone.
func TestBuiltinsCanBeShadowed(t *testing.T) {
	wantOut(t, `len := func(s string) string { return "shadowed:" + s }
fmt.Println(len("x"))`, "shadowed:x\n")
}

// ---- conversions ----

func TestConversions(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"float to int truncates", `fmt.Println(int(3.9))`, "3\n"},
		{"int to float", `fmt.Println(float64(3) / 2)`, "1.5\n"},
		{"int to int64", `fmt.Println(int64(3))`, "3\n"},
		{"int to rune", `fmt.Println(rune(65))`, "65\n"},
		{"int to byte", `fmt.Println(byte(65))`, "65\n"},
		{"rune to string", `fmt.Println(string(rune(65)))`, "A\n"},
		{"int to string is a code point", `fmt.Println(string(65))`, "A\n"},
		{"string of a string is identity", `fmt.Println(string("ab"))`, "ab\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// A numeric conversion applied to a string is the classic slip; the
// message points at strconv rather than producing a code point silently.
func TestNumericConversionOfAStringIsRefused(t *testing.T) {
	wantErr(t, `fmt.Println(int("42"))`, "use strconv")
}

func TestStringConversionOfAnUnsupportedTypeIsRefused(t *testing.T) {
	wantErr(t, `fmt.Println(string(1.5))`, "use fmt.Sprint or strconv")
}

func TestConversionArity(t *testing.T) {
	wantErr(t, `fmt.Println(int(1, 2))`, "conversion expects one argument")
}

// ---- package symbols ----

// Package selectors resolve through the curated registry, both as
// functions and as plain values.
func TestPackageSymbols(t *testing.T) {
	wantOut(t, `fmt.Println(strings.ToUpper("ab"), strconv.Itoa(7))`, "AB 7\n")
}

// A package in the registry but a symbol that is not says so precisely,
// and distinguishes itself from an unknown package.
func TestUnknownRegistrySymbolIsAnError(t *testing.T) {
	wantErr(t, `strings.NoSuchFunction("x")`, "not in the grsh registry")
	wantErr(t, `nosuchpkg.Thing()`, "undefined: nosuchpkg")
}

// A local binding shadows a package name, so `strings := "..."` does not
// break the script -- evalSelector checks the environment first.
func TestLocalBindingShadowsAPackageName(t *testing.T) {
	wantErr(t, `strings := "shadowed"
fmt.Println(strings.ToUpper("ab"))`, "unknown method ToUpper on string")
}

// ---- the {expr} fragment cache ----
//
// wordEval.EvalGoExpr is the shell leg, which these tests deliberately do
// not drive (see the harness note in helper_test.go) — but parseFragment
// underneath it is pure interpreter, so it is exercised directly. The node
// handed in plays the part of the enclosing __shell call: all it supplies
// is a position to anchor the fragment's line info to.

// fragmentHarness returns an interpreter standing where a running script
// stands, plus a node from its file to anchor fragments against.
func fragmentHarness(t *testing.T) (*Interp, ast.Node) {
	t.Helper()
	var out bytes.Buffer
	in := newTestInterp(&out, nil)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "t.grsh",
		"package main\n\nfunc __main() {\n\tx := 1\n\t_ = x\n}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("harness: the test source is not valid Go: %v", err)
	}
	if err := in.Run(fset, f); err != nil {
		t.Fatalf("harness run: %v", err)
	}
	// parseFragment is a mid-run operation -- it resolves the enclosing
	// node's position in in.fset and parses the fragment into it. Run
	// restores that field on the way out, because it is re-entrant (a
	// sourced file installs its own fset over the caller's), so the
	// harness re-installs it here rather than borrowing what a finished
	// run happened to leave behind.
	in.fset = fset
	return in, f.Decls[0].(*ast.FuncDecl).Body.List[0]
}

// The cache is what keeps a {expr} inside a loop from re-parsing per
// iteration, so the hit has to be an actual reuse of the AST, not merely
// an equal one.
func TestFragmentCacheReusesTheParsedAST(t *testing.T) {
	in, node := fragmentHarness(t)

	first, err := in.parseFragment("strings.ToUpper(x)", node)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	second, err := in.parseFragment("strings.ToUpper(x)", node)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if first != second {
		t.Error("the same fragment re-parsed: the cache did not hit")
	}
	other, err := in.parseFragment("strings.ToLower(x)", node)
	if err != nil {
		t.Fatalf("parse of a second fragment: %v", err)
	}
	if other == first {
		t.Error("a different fragment returned the first one's AST")
	}
}

// A generated script can hold more distinct {expr} sites than a person
// would ever write. The cache must stay bounded through that, and must
// still be serving hits on the other side of a flush.
func TestFragmentCacheIsBounded(t *testing.T) {
	in, node := fragmentHarness(t)

	for i := range exprCacheMax * 3 {
		if _, err := in.parseFragment(fmt.Sprintf("x + %d", i), node); err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		if len(in.exprCache) > exprCacheMax {
			t.Fatalf("cache holds %d entries after %d fragments, cap is %d",
				len(in.exprCache), i+1, exprCacheMax)
		}
	}
	// The flush must leave a working cache behind, not a dead one.
	a, err := in.parseFragment("x + 1", node)
	if err != nil {
		t.Fatalf("parse after the flush: %v", err)
	}
	b, _ := in.parseFragment("x + 1", node)
	if a != b {
		t.Error("the cache stopped hitting after a flush")
	}
}

// A malformed fragment must not be remembered as anything: the error is
// the answer, and it is cheap to re-derive.
func TestFragmentCacheDoesNotHoldFailures(t *testing.T) {
	in, node := fragmentHarness(t)
	if _, err := in.parseFragment("x +", node); err == nil {
		t.Fatal("an incomplete fragment parsed without error")
	}
	if len(in.exprCache) != 0 {
		t.Errorf("cache holds %d entries after a failed parse, want 0", len(in.exprCache))
	}
}

// errFor builds a plain error for the test cases above without dragging
// serr's field machinery into the assertions.
func errFor(msg string) error { return simpleErr(msg) }

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
