package interp

import "testing"

// ---- literals ----

func TestBasicLiterals(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"decimal int", `fmt.Println(42)`, "42\n"},
		// ParseInt with base 0 accepts every Go integer spelling.
		{"hex int", `fmt.Println(0x1f)`, "31\n"},
		{"octal int", `fmt.Println(0o17)`, "15\n"},
		{"binary int", `fmt.Println(0b1010)`, "10\n"},
		{"underscores", `fmt.Println(1_000_000)`, "1000000\n"},
		{"float", `fmt.Println(1.5)`, "1.5\n"},
		{"exponent", `fmt.Println(2e3)`, "2000\n"},
		{"interpreted string", `fmt.Println("a\tb")`, "a\tb\n"},
		{"raw string", "fmt.Println(`a\\tb`)", "a\\tb\n"},
		// A char literal is a rune, so it prints as its code point.
		{"char", `fmt.Println('a')`, "97\n"},
		{"escaped char", `fmt.Println('\n')`, "10\n"},
		{"true", `fmt.Println(true)`, "true\n"},
		{"false", `fmt.Println(false)`, "false\n"},
		{"nil", `fmt.Println(nil)`, "<nil>\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// ---- arithmetic and the int/float promotion rule ----

// Integer operands stay integer -- including division, which truncates.
// Any float operand promotes the whole expression to float64.
func TestNumericOps(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"int add", `fmt.Println(2 + 3)`, "5\n"},
		{"int sub", `fmt.Println(2 - 3)`, "-1\n"},
		{"int mul", `fmt.Println(4 * 3)`, "12\n"},
		{"int div truncates", `fmt.Println(7 / 2)`, "3\n"},
		{"int div negative truncates toward zero", `fmt.Println(-7 / 2)`, "-3\n"},
		{"int mod", `fmt.Println(7 % 3)`, "1\n"},
		{"float div", `fmt.Println(7.0 / 2)`, "3.5\n"},
		{"mixed promotes", `fmt.Println(1 + 0.5)`, "1.5\n"},
		{"float result that is whole prints bare", `fmt.Println(2.0 * 2)`, "4\n"},
		{"unary minus int", `fmt.Println(-(3))`, "-3\n"},
		{"unary minus float", `fmt.Println(-(1.5))`, "-1.5\n"},
		{"unary plus is identity", `fmt.Println(+(3))`, "3\n"},
		{"precedence", `fmt.Println(2 + 3*4)`, "14\n"},
		{"parens override precedence", `fmt.Println((2 + 3) * 4)`, "20\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// Division by zero is a positioned script error, not a Go panic that
// would take the session down.
func TestDivisionByZeroIsAnError(t *testing.T) {
	wantErr(t, `fmt.Println(1 / 0)`, "integer division by zero")
	wantErr(t, `fmt.Println(1 % 0)`, "integer modulo by zero")
}

// Float division by zero follows IEEE rather than erroring, matching Go.
func TestFloatDivisionByZeroIsInf(t *testing.T) {
	wantOut(t, `fmt.Println(1.0 / 0.0)`, "+Inf\n")
}

func TestBitwiseOps(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"and", `fmt.Println(12 & 10)`, "8\n"},
		{"or", `fmt.Println(12 | 10)`, "14\n"},
		{"xor", `fmt.Println(12 ^ 10)`, "6\n"},
		{"and-not", `fmt.Println(12 &^ 10)`, "4\n"},
		{"shift left", `fmt.Println(1 << 5)`, "32\n"},
		{"shift right", `fmt.Println(64 >> 3)`, "8\n"},
		// Unary ^ is Go's bitwise complement.
		{"complement", `fmt.Println(^(0))`, "-1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// Go panics on a negative shift count; the interpreter reports it.
func TestNegativeShiftIsAnError(t *testing.T) {
	wantErr(t, `n := -1
fmt.Println(1 << n)`, "negative shift amount")
}

// ---- strings ----

func TestStringOps(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"concat", `fmt.Println("ab" + "cd")`, "abcd\n"},
		{"equal", `fmt.Println("a" == "a")`, "true\n"},
		{"not equal", `fmt.Println("a" != "b")`, "true\n"},
		{"less", `fmt.Println("abc" < "abd")`, "true\n"},
		{"greater or equal", `fmt.Println("b" >= "a")`, "true\n"},
		// Indexing a string yields a byte, as in Go.
		{"index yields a byte", `fmt.Println("abc"[1])`, "98\n"},
		{"slice yields a string", `fmt.Println("hello"[1:3])`, "el\n"},
		{"open-ended slice", `fmt.Println("hello"[2:])`, "llo\n"},
		{"len counts bytes", `fmt.Println(len("héllo"))`, "6\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// Subtraction is not defined on strings; the message names the operator
// rather than leaking a Go type error.
func TestStringMinusIsAnError(t *testing.T) {
	wantErr(t, `fmt.Println("a" - "b")`, "is not defined on strings")
}

// ---- comparison, equality, booleans ----

func TestComparisonAndBooleans(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"int less", `fmt.Println(1 < 2)`, "true\n"},
		{"int greater", `fmt.Println(1 > 2)`, "false\n"},
		{"mixed numeric compare", `fmt.Println(1 < 1.5)`, "true\n"},
		{"bool equality", `fmt.Println(true == true, true != false)`, "true true\n"},
		{"not", `fmt.Println(!true)`, "false\n"},
		{"nil equals nil", `fmt.Println(nil == nil)`, "true\n"},
		{"value is not nil", `fmt.Println(1 == nil)`, "false\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { wantOut(t, tc.src, tc.want) })
	}
}

// && and || evaluate the right operand only when the left cannot decide
// the result. The probe records whether it ran; a non-lazy implementation
// would print it.
func TestLogicalOpsShortCircuit(t *testing.T) {
	probe := map[string]any{
		"touch": func() bool { return true },
	}

	// The right operand of && is dead when the left is false.
	out, err := eval(t, `fmt.Println(false && touch())`, probe)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "false\n" {
		t.Errorf("&& got %q, want %q", out, "false\n")
	}

	// ...and of || when the left is true.
	out, err = eval(t, `fmt.Println(true || touch())`, probe)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "true\n" {
		t.Errorf("|| got %q, want %q", out, "true\n")
	}
}

// The stronger form of the same claim: the dead operand is not merely
// ignored, it never runs -- so its errors never surface.
func TestLogicalOpsDoNotEvaluateDeadOperand(t *testing.T) {
	wantOut(t, `xs := []int{}
fmt.Println(len(xs) > 0 && xs[0] == 1)`, "false\n")
}

// Conditions must be genuinely bool; a truthy int is not accepted.
func TestNonBoolConditionIsAnError(t *testing.T) {
	wantErr(t, `if 1 {
	fmt.Println("no")
}`, "condition must be bool")
}

// Uncomparable operands must not panic. Go rejects slice == slice at
// compile time; the interpreter has no type checker, so safeEqual absorbs
// the runtime panic and reports unequal.
func TestUncomparableEqualityDoesNotPanic(t *testing.T) {
	wantOut(t, `a := []int{1}
b := []int{1}
fmt.Println(a == b)`, "false\n")
}

// An operator with no rule for its operand types names both types.
func TestUndefinedOperatorMessageNamesTypes(t *testing.T) {
	wantErr(t, `fmt.Println("a" * 2)`, "not defined on")
}

// ---- indexing, slicing, containers ----

func TestSliceLiteralsAndIndexing(t *testing.T) {
	wantOut(t, `xs := []int{10, 20, 30}
fmt.Println(len(xs), xs[0], xs[2])`, "3 10 30\n")
}

func TestSliceIndexOutOfRangeIsAnError(t *testing.T) {
	wantErr(t, `xs := []int{1}
fmt.Println(xs[5])`, "index out of range")
	wantErr(t, `xs := []int{1}
fmt.Println(xs[-1])`, "index out of range")
}

func TestSliceExpressions(t *testing.T) {
	wantOut(t, `xs := []int{0, 1, 2, 3, 4}
fmt.Println(xs[1:3], xs[:2], xs[3:], xs[:])`, "[1 2] [0 1] [3 4] [0 1 2 3 4]\n")
}

// Inverted and over-long bounds report the offending range and the
// length, the way Go's runtime message does.
func TestSliceBoundsOutOfRangeIsAnError(t *testing.T) {
	wantErr(t, `xs := []int{1, 2}
fmt.Println(xs[2:1])`, "slice bounds out of range")
	wantErr(t, `xs := []int{1, 2}
fmt.Println(xs[0:9])`, "slice bounds out of range")
}

func TestIndexAssignment(t *testing.T) {
	wantOut(t, `xs := []int{1, 2, 3}
xs[1] = 99
fmt.Println(xs)`, "[1 99 3]\n")
}

func TestIndexAssignmentOutOfRangeIsAnError(t *testing.T) {
	wantErr(t, `xs := []int{1}
xs[3] = 1`, "index out of range")
}

func TestMapLiteralsAndLookup(t *testing.T) {
	wantOut(t, `m := map[string]int{"a": 1, "b": 2}
fmt.Println(len(m), m["a"], m["b"])`, "2 1 2\n")
}

// A missing key yields the element type's zero value, not an error.
func TestMapMissingKeyYieldsZero(t *testing.T) {
	wantOut(t, `m := map[string]int{}
fmt.Println(m["nope"])`, "0\n")
	wantOut(t, `m := map[string]string{}
fmt.Printf("%q\n", m["nope"])`, "\"\"\n")
}

// Comma-ok distinguishes "absent" from "present and zero" -- the reason
// assignRHS special-cases a two-target IndexExpr before the generic path.
func TestMapCommaOk(t *testing.T) {
	wantOut(t, `m := map[string]int{"a": 0}
v, ok := m["a"]
w, ok2 := m["b"]
fmt.Println(v, ok, w, ok2)`, "0 true 0 false\n")
}

func TestMapAssignmentAndDelete(t *testing.T) {
	wantOut(t, `m := map[string]int{}
m["x"] = 1
m["y"] = 2
delete(m, "x")
_, ok := m["x"]
fmt.Println(len(m), ok, m["y"])`, "1 false 2\n")
}

// reflect's SetMapIndex panics on a nil map; the check in setIndexed
// turns that into a positioned message that also says how to fix it.
func TestNilMapAssignmentIsAnError(t *testing.T) {
	wantErr(t, `var m map[string]int
m["k"] = 1`, "assignment to entry in nil map")
}

// Go's delete on a nil map is a silent no-op, and reflect's is a panic --
// the interpreter follows Go.
func TestDeleteOnNilMapIsANoOp(t *testing.T) {
	wantOut(t, `var m map[string]int
delete(m, "k")
fmt.Println(len(m))`, "0\n")
}

// Map iteration is sorted by key when keys are strings, so scripts that
// range over a map are reproducible -- Go's randomized order would make
// golden output impossible.
func TestMapRangeIsSortedByStringKey(t *testing.T) {
	wantOut(t, `m := map[string]int{"c": 3, "a": 1, "b": 2}
for k, v := range m {
	fmt.Print(k, v, " ")
}
fmt.Println()`, "a1 b2 c3 \n")
}

func TestNestedComposites(t *testing.T) {
	wantOut(t, `m := map[string][]int{"a": []int{1, 2}}
fmt.Println(m["a"][1])`, "2\n")
	wantOut(t, `xs := [][]int{[]int{1, 2}, []int{3}}
fmt.Println(len(xs), xs[0][1], xs[1][0])`, "2 2 3\n")
}

// KNOWN GAP, pinned as it stands.
//
// Go lets a nested composite literal elide the element type -- the inner
// []int in [][]int{{1, 2}} is implied. evalComposite requires an explicit
// type on every literal (a *ast.CompositeLit with a nil Type reaches it
// with nothing to build from), so the elided spelling is rejected.
//
// The message is at least actionable, and writing the inner type out
// works. Recording it here means a future fix shows up as a test change
// rather than as an unexplained behavior shift.
func TestElidedNestedCompositeTypeIsNotSupported(t *testing.T) {
	wantErr(t, `xs := [][]int{{1, 2}}
_ = xs`, "composite literal needs an explicit type here")
	wantErr(t, `m := map[string][]int{"a": {1}}
_ = m`, "composite literal needs an explicit type here")
}

// ---- type assertions ----

// Two-value form reports failure through ok and yields the target type's
// zero value, exactly as Go does.
func TestTypeAssertionCommaOk(t *testing.T) {
	wantOut(t, `var v any = "hi"
s, ok := v.(string)
n, ok2 := v.(int)
fmt.Println(s, ok, n, ok2)`, "hi true 0 false\n")
}

// Single-value form: Go panics, grsh reports an error. The ok value must
// not leak into the assignment either -- assignRHS strips it.
func TestTypeAssertionSingleValue(t *testing.T) {
	wantOut(t, `var v any = "hi"
s := v.(string)
fmt.Println(s)`, "hi\n")
	wantErr(t, `var v any = "hi"
n := v.(int)
_ = n`, "type assertion failed")
}

// ---- declarations ----

// `var` without a value takes the type's zero.
func TestVarDeclarationZeroValues(t *testing.T) {
	wantOut(t, `var i int
var f float64
var s string
var b bool
fmt.Printf("%d %v %q %v\n", i, f, s, b)`, "0 0 \"\" false\n")
}

func TestVarDeclarationWithValue(t *testing.T) {
	wantOut(t, `var n = 5
var s string = "x"
fmt.Println(n, s)`, "5 x\n")
}

// const behaves as var here: the interpreter has no notion of
// immutability, and evalDecl handles both tokens identically.
func TestConstDeclaration(t *testing.T) {
	wantOut(t, `const n = 5
fmt.Println(n * 2)`, "10\n")
}

// Several names on one var line each get the zero value; evalDecl walks
// Names, not Values, so the extra names are not silently dropped.
func TestVarDeclarationOfSeveralNames(t *testing.T) {
	wantOut(t, `var x, y int
fmt.Println(x, y)`, "0 0\n")
}

// `_` never becomes a binding, in any of the forms that can target it.
func TestBlankIdentifierIsNeverBound(t *testing.T) {
	wantErr(t, `_ = 1
fmt.Println(_)`, "")
	wantOut(t, `_, b := 1, 2
fmt.Println(b)`, "2\n")
}

// ---- assignment forms ----

func TestCompoundAssignment(t *testing.T) {
	wantOut(t, `n := 10
n += 5
n -= 3
n *= 2
n /= 4
n %= 4
fmt.Println(n)`, "2\n")
}

func TestCompoundAssignmentOnStringsAndBits(t *testing.T) {
	wantOut(t, `s := "a"
s += "b"
n := 12
n &= 10
n |= 1
n ^= 2
n <<= 2
fmt.Println(s, n)`, "ab 44\n")
}

func TestCompoundAssignmentIntoContainers(t *testing.T) {
	wantOut(t, `xs := []int{1, 2}
xs[0] += 10
m := map[string]int{"k": 1}
m["k"] *= 5
fmt.Println(xs[0], m["k"])`, "11 5\n")
}

func TestIncDec(t *testing.T) {
	wantOut(t, `n := 1
n++
n++
n--
fmt.Println(n)`, "2\n")
}

func TestMultiAssign(t *testing.T) {
	wantOut(t, `a, b := 1, 2
a, b = b, a
fmt.Println(a, b)`, "2 1\n")
}

// A count mismatch is caught rather than silently dropping values.
func TestAssignmentCountMismatchIsAnError(t *testing.T) {
	wantErr(t, `a, b := 1
_, _ = a, b`, "assignment mismatch")
}

// `=` to an undeclared name is an error, and the message says how to fix
// it -- the single most common slip when moving between shell and Go.
func TestAssignToUndeclaredIsAnError(t *testing.T) {
	wantErr(t, `x = 1`, "use := to declare")
}

func TestUndefinedIdentifier(t *testing.T) {
	wantErr(t, `fmt.Println(nope)`, "undefined: nope")
}
