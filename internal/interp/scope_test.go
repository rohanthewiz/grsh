package interp

import (
	"strings"
	"testing"
)

// ---- lexical scoping: the safety net ----
//
// Every construct that introduces a scope does it by wrapping the current
// Env, and several wrap TWICE: evalFor builds one Env for the loop clause
// and another per iteration, then the *ast.BlockStmt handler wraps the
// body a third time (the same shape holds for if/else and range). That
// redundancy is a known allocation cost -- 13 allocs per plain loop
// iteration -- and the obvious fix is to drop the extra wrap.
//
// The fix is only safe if the OBSERVABLE scoping is unchanged, and the
// observable scoping had no direct coverage at all: internal/interp had no
// tests, only the indirect reach of the golden scripts. These cases pin
// the behavior a wrap-removal would perturb -- what a `:=` shadows, how
// long a binding lives, and which cell a closure captures -- so the
// optimization can be judged by a diff in this file rather than by
// reading the evaluator and hoping.

// A block is a scope. Removing the *ast.BlockStmt wrap would leak this
// binding into the enclosing scope and this is the case that catches it.
func TestScopeBlockIsolatesDeclarations(t *testing.T) {
	wantErr(t, "{ x := 1 }\nfmt.Println(x)", "undefined: x")
}

// The counterpart: a block must still be ABLE to declare. A lazily
// allocated Env map that forgets to allocate on first Define fails here
// rather than in some distant script.
func TestScopeBlockCanDeclareAndRead(t *testing.T) {
	wantOut(t, "y := 1\n{ x := 2\nfmt.Println(x + y) }", "3\n")
}

// `:=` inside a block shadows; the outer binding is untouched after.
func TestScopeBlockShadowRestores(t *testing.T) {
	wantOut(t, `x := 1
{
	x := 2
	fmt.Println(x)
}
fmt.Println(x)`, "2\n1\n")
}

// `=` inside a block walks outward and mutates the outer binding -- the
// distinction from the shadow case above, and the reason Set exists
// separately from Define.
func TestScopeBlockAssignmentReachesOuter(t *testing.T) {
	wantOut(t, `x := 1
{
	x = 2
}
fmt.Println(x)`, "2\n")
}

func TestScopeLoopBodyDeclarationDoesNotEscape(t *testing.T) {
	wantErr(t, "for i := 0; i < 2; i++ { y := i }\nfmt.Println(y)", "undefined: y")
}

// Each iteration gets a fresh body scope, so a `:=` of the same name
// succeeds every time rather than colliding with the previous iteration's
// binding -- and never sees the previous iteration's value.
func TestScopeLoopBodyIsFreshEachIteration(t *testing.T) {
	wantOut(t, `for i := 0; i < 3; i++ {
	seen := i * 10
	fmt.Print(seen, " ")
}
fmt.Println()`, "0 10 20 \n")
}

// The loop clause variable, by contrast, lives across iterations: it is
// declared once in the clause scope and the post statement mutates it.
// A body-scoped `i` would restart from 0 forever.
func TestScopeLoopClauseVarPersistsAcrossIterations(t *testing.T) {
	wantOut(t, `n := 0
for i := 0; i < 4; i++ {
	n += i
}
fmt.Println(n)`, "6\n")
}

// The clause variable is scoped to the loop, not to the enclosing block.
func TestScopeLoopClauseVarDoesNotEscape(t *testing.T) {
	wantErr(t, "for i := 0; i < 2; i++ { }\nfmt.Println(i)", "undefined: i")
}

// Cond and Post evaluate in the CLAUSE scope, so a body declaration is
// invisible to them. Were the body sharing the clause scope, `stop` would
// resolve on the second iteration and this would print 1 instead of
// failing.
func TestScopeLoopCondCannotSeeBodyDeclarations(t *testing.T) {
	wantErr(t, `for i := 0; !stop; i++ {
	stop := true
	fmt.Println(i)
}`, "undefined: stop")
}

// Shadowing the clause variable in the body is legal and does not disturb
// the loop's own counter -- the post statement still advances the outer
// cell, so this terminates.
func TestScopeLoopBodyMayShadowClauseVar(t *testing.T) {
	wantOut(t, `for i := 0; i < 3; i++ {
	i := i * 100
	fmt.Print(i, " ")
}
fmt.Println()`, "0 100 200 \n")
}

// KNOWN DIVERGENCE FROM GO, pinned deliberately.
//
// Go 1.22 made the for-clause variable per-iteration, so real Go prints
// "0 1 2" here. grsh declares it once in the clause scope, so all three
// closures capture the SAME cell and observe its final value -- Go's
// pre-1.22 semantics.
//
// This is not an endorsement; it is a tripwire. Whichever way it is
// settled it should be settled on purpose, and an Env refactor that
// flipped it as a side effect would otherwise ship silently.
func TestScopeLoopClosuresShareTheClauseVar(t *testing.T) {
	wantOut(t, `fns := []any{}
for i := 0; i < 3; i++ {
	fns = append(fns, func() { fmt.Print(i, " ") })
}
for _, f := range fns {
	f()
}
fmt.Println()`, "3 3 3 \n")
}

// Range is the other half of that divergence and lands on the modern
// side: evalRange builds a fresh scope per iteration and defines the key
// and value into it, so each closure captures its own cell. The asymmetry
// with the for-clause case above is real, and pinning both is what makes
// it visible.
func TestScopeRangeClosuresCaptureTheirOwnValue(t *testing.T) {
	wantOut(t, `fns := []any{}
for _, v := range []int{10, 20, 30} {
	fns = append(fns, func() { fmt.Print(v, " ") })
}
for _, f := range fns {
	f()
}
fmt.Println()`, "10 20 30 \n")
}

func TestScopeRangeVarsDoNotEscape(t *testing.T) {
	wantErr(t, "for i, v := range []int{1} { _ = i\n_ = v }\nfmt.Println(i)", "undefined: i")
}

// The body still nests inside the range scope, so a `:=` on the value
// name shadows for that iteration only -- the next iteration re-binds
// from the container.
func TestScopeRangeBodyMayShadowValueVar(t *testing.T) {
	wantOut(t, `for i, v := range []string{"a", "b"} {
	v := v + "!"
	fmt.Println(i, v)
}`, "0 a!\n1 b!\n")
}

// `for k, v = range` (assignment, not declaration) targets bindings that
// already exist, and they survive the loop carrying the last iteration's
// values.
func TestScopeRangeAssignFormReusesOuterBindings(t *testing.T) {
	wantOut(t, `i := -1
v := ""
for i, v = range []string{"a", "b", "c"} {
}
fmt.Println(i, v)`, "2 c\n")
}

// ...and fails loudly when the target was never declared, rather than
// quietly declaring it.
func TestScopeRangeAssignFormNeedsExistingBinding(t *testing.T) {
	wantErr(t, "for i = range []int{1, 2} { }", "undefined: i")
}

// An if statement's init binding is visible in the condition, the body,
// and the else -- and nowhere after.
func TestScopeIfInitVarSpansBothBranches(t *testing.T) {
	wantOut(t, `if v := 5; v > 10 {
	fmt.Println("then", v)
} else {
	fmt.Println("else", v)
}`, "else 5\n")
}

func TestScopeIfInitVarDoesNotEscape(t *testing.T) {
	wantErr(t, "if v := 5; v > 1 { }\nfmt.Println(v)", "undefined: v")
}

// Body and else are separate scopes, so a name declared in one is not in
// scope in the other. (Only the taken branch runs, so this is asserted by
// declaring in the branch that is skipped.)
func TestScopeIfBranchDeclarationsDoNotEscape(t *testing.T) {
	wantErr(t, `if true {
	inner := 1
	_ = inner
}
fmt.Println(inner)`, "undefined: inner")
}

// else-if chains nest: each link's init is visible to the links below it,
// which is what makes the idiomatic `if a := f(); ... else if a > 2` work.
func TestScopeElseIfSeesEarlierInit(t *testing.T) {
	wantOut(t, `if a := 3; a > 5 {
	fmt.Println("big")
} else if a > 2 {
	fmt.Println("mid", a)
} else {
	fmt.Println("small")
}`, "mid 3\n")
}

// A switch has two nested scopes: the init's, shared by every case
// expression, and a per-case-body one.
func TestScopeSwitchInitVarSpansCases(t *testing.T) {
	wantOut(t, `switch x := 3; x {
case 3:
	fmt.Println("three", x)
default:
	fmt.Println("other", x)
}`, "three 3\n")
}

func TestScopeSwitchCaseBodyDoesNotEscape(t *testing.T) {
	wantErr(t, `switch 3 {
case 3:
	y := 9
	_ = y
}
fmt.Println(y)`, "undefined: y")
}

// A closure body is a scope over its DEFINING env, not its calling env:
// `hidden` is in scope where f runs but not where f was written.
func TestScopeClosureIsLexicalNotDynamic(t *testing.T) {
	wantErr(t, `f := func() {
	fmt.Println(hidden)
}
{
	hidden := 1
	_ = hidden
	f()
}`, "undefined: hidden")
}

// The captured env is shared, not copied: the closure sees writes made
// after it was created, and the definer sees the closure's writes.
func TestScopeClosureSharesCellsWithDefiner(t *testing.T) {
	wantOut(t, `n := 1
bump := func() { n = n + 10 }
n = 5
bump()
fmt.Println(n)`, "15\n")
}

// Two closures over the same env share one cell -- the counter idiom.
func TestScopeClosuresShareOneCell(t *testing.T) {
	wantOut(t, `make_counter := func() any {
	n := 0
	return func() int {
		n = n + 1
		return n
	}
}
c := make_counter()
fmt.Println(c(), c(), c())`, "1 2 3\n")
}

// Separate invocations get separate cells: each call to the maker runs a
// fresh closure-call scope.
func TestScopeCounterInstancesAreIndependent(t *testing.T) {
	wantOut(t, `make_counter := func() any {
	n := 0
	return func() int {
		n = n + 1
		return n
	}
}
a := make_counter()
b := make_counter()
a()
a()
fmt.Println(a(), b())`, "3 1\n")
}

// Parameters live in the call scope and shadow same-named globals without
// touching them.
func TestScopeParamsShadowGlobals(t *testing.T) {
	wantOut(t, `x := "global"
f := func(x string) string { return "param:" + x }
fmt.Println(f("arg"), x)`, "param:arg global\n")
}

// A `:=` in a function body is local to the call, so recursion does not
// stomp on the caller's frame.
func TestScopeRecursionHasIndependentLocals(t *testing.T) {
	wantOut(t, `fact := func(n int) int {
	if n <= 1 {
		return 1
	}
	sub := fact(n - 1)
	return n * sub
}
fmt.Println(fact(5))`, "120\n")
}

// Deep nesting: every construct that wraps must still let the innermost
// scope reach an outer binding by `=`. This is the composite case a wrap
// refactor is most likely to break, since it exercises loop, if, block
// and switch scopes stacked on one chain.
func TestScopeDeepNestingReachesOuterBinding(t *testing.T) {
	wantOut(t, `total := 0
for i := 0; i < 3; i++ {
	if i > 0 {
		{
			switch {
			case true:
				total = total + i
			}
		}
	}
}
fmt.Println(total)`, "3\n")
}

// The reverse direction of the same nesting: a name declared at each
// level shadows the one above it, and unwinding restores each in turn.
func TestScopeShadowUnwindsThroughEveryLevel(t *testing.T) {
	var b strings.Builder
	b.WriteString(`x := 0
fmt.Print(x, " ")
for i := 0; i < 1; i++ {
	x := 1
	fmt.Print(x, " ")
	if true {
		x := 2
		fmt.Print(x, " ")
		{
			x := 3
			fmt.Print(x, " ")
		}
		fmt.Print(x, " ")
	}
	fmt.Print(x, " ")
}
fmt.Println(x)`)
	wantOut(t, b.String(), "0 1 2 3 2 1 0\n")
}

// ---- the conditionally-created scopes ----
//
// evalFor, evalRange, evalSwitch and the if handler now build their extra
// scope only when something will be defined into it. That turns a
// straight line into a branch, and these cover the side of each branch
// the cases above do not reach: a loop with no clause to hold, a range
// that declares nothing, a switch with no init.

// A condition-only for has no clause scope at all now. Its body must
// still isolate declarations -- that job belongs to the body block, which
// is exactly why the clause scope was removable.
func TestScopeConditionOnlyForStillIsolatesItsBody(t *testing.T) {
	wantErr(t, `n := 0
for n < 2 {
	n++
	inner := n
	_ = inner
}
fmt.Println(inner)`, "undefined: inner")
}

// The same for a bare for, which likewise has no clause to hold.
func TestScopeBareForStillIsolatesItsBody(t *testing.T) {
	wantOut(t, `n := 0
for {
	acc := n * 10
	n++
	fmt.Print(acc, " ")
	if n == 3 {
		break
	}
}
fmt.Println()`, "0 10 20 \n")
}

// `for range n` binds nothing, so there is no per-iteration range scope
// -- the body block is the only scope, and it is still fresh each time.
func TestScopeBareRangeStillIsolatesItsBody(t *testing.T) {
	wantErr(t, `for range 2 {
	inner := 1
	_ = inner
}
fmt.Println(inner)`, "undefined: inner")
}

// The `=` range form assigns outward, so every iteration writes the SAME
// cell and closures over it all observe the final value -- the for-clause
// behavior, not the `:=` range behavior. The two range forms genuinely
// differ here, and the difference is not new: Set always walked outward.
// It is pinned now because the per-iteration scope that used to sit in
// front of that walk is gone, and its absence must not be what changes
// the answer.
func TestScopeRangeAssignFormClosuresShareOneCell(t *testing.T) {
	wantOut(t, `v := 0
fns := []any{}
for _, v = range []int{10, 20, 30} {
	fns = append(fns, func() { fmt.Print(v, " ") })
}
for _, f := range fns {
	f()
}
fmt.Println()`, "30 30 30 \n")
}

// A tagless switch has no init scope now. A case body still gets its own
// scope from runCaseBody, which a CaseClause needs because its body is a
// bare statement list that no block handler ever wraps.
func TestScopeTaglessSwitchCaseBodyStillIsolates(t *testing.T) {
	wantOut(t, `x := "outer"
switch {
case true:
	x := "inner"
	fmt.Println(x)
}
fmt.Println(x)`, "inner\nouter\n")
}
