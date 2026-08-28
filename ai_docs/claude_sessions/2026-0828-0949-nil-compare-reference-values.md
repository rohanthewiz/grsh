# Session: `== nil` for Reference Values

Session: 7e2fd7ef-4d5b-4acf-9d9d-686a2d606845
Date: 2026-08-28

## Goal

Close the first item the struct-map-keys session left Open:
`var n map[string]int; n == nil` answered **false**.

That session pinned it as PRE-EXISTING, unrelated to either feature it
shipped, and "its own fix, touching every reference type." This is that
fix.

## The cause, and why it was one line away from being invisible

`var n map[string]int` resolves through `TypeDesc.Zero`, which for a
non-struct returns `reflect.Zero(d.RT).Interface()`. That is a **non-nil
interface wrapping a nil map header** — the interface has a type word.

`safeEqual`'s nil branch compared the interfaces themselves:

```go
if x == nil || y == nil {
	return x == nil && y == nil   // n is not untyped nil, so: false
}
```

The `nil` literal DOES arrive as an untyped Go nil (`expr.go:28` returns
a bare `nil` for the ident), so one side matched and the other never
could. The bug is the same shape as the classic Go typed-nil gotcha, but
inverted: here the interface wrapper is grsh's own storage detail, not
something the script asked for.

## The fix

One site, because every route already converged there:

```
n == nil      binaryOp -> safeEqual
n != nil      the same, negated
if n == nil   the same
switch { case n == nil: }   evalSwitch calls binaryOp
switch n { case nil: }      the same
```

`safeEqual`'s nil branch now asks `isNilRef` on BOTH sides instead of
comparing interfaces:

```go
func isNilRef(v Value) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}
```

Asking both sides, not just the non-literal one, is what keeps
`nil == nil` true without a special case.

## The pointer question, in two passes

Pointers were the whole judgement call, and the first answer was wrong.
Both passes are kept here because the second one only became visible by
writing the first one down.

### First pass: refuse every pointer

grsh has no static types, so a script's own nil `*os.File` and the typed
nil a Go callee stuffed into an `error` look like the SAME value once
they are a `Value`. That framing says the two cases must share one
answer, and the only choice is which one to get wrong:

| unwrap pointers? | `p == nil` on a nil `*T` | `err == nil` on a typed-nil error |
|---|---|---|
| yes | true (Go agrees) | true — **swallows a real error** |
| no | false (Go disagrees) | false (Go agrees) |

The second column decides it, so pointers stayed out. The
error-return convention is load-bearing here — `data :=
os.ReadFile(...)` aborts the script on a non-nil error — and letting
that check silently pass is far worse than a nil pointer comparing
wrong.

### Second pass: the premise was false

"grsh cannot tell them apart" is not true. The interpreter tells them
apart in two places, using Go's own test — see the addendum at the
bottom, which is where the fix landed. A third row belongs on that
table, and it is the one that shipped:

| unwrap pointers? | `p == nil` on a nil `*T` | `err == nil` on a typed-nil error |
|---|---|---|
| **all but error types (chosen)** | true (Go agrees) | false (Go agrees) |

## What is genuinely not unwrapped

**STRUCT.** A `*StructVal` never reaches `safeEqual` through `==` at
all; `binaryOp` intercepts it and routes to `valuesEqual`, whose
`xs == nil && y == nil` already answered a typed-nil struct correctly.
The field walk catches struct fields the same way before recursing. So
the pointer branch and the struct path never meet — and if they did,
they now agree anyway, since both call a typed nil struct nil.

## Behavior

| case | before | now | Go |
|---|---|---|---|
| `var m map[string]int; m == nil` | false | **true** | true |
| `var xs []int; xs == nil` | false | **true** | true |
| `m["miss"]` on `map[string][]int` `== nil` | false | **true** | true |
| `[]int{}` / `map[string]int{}` `== nil` | false | false | false |
| `0 == nil`, `"" == nil`, `false == nil` | false | false | (rejected) |
| `nil == nil` | true | true | true |
| nil `map[P]int` `== nil` | false | **true** | true |
| nil `*regexp.Regexp` from a failed compile | false | **true** | true |
| a live `*bytes.Buffer` `== nil` | false | false | false |
| typed-nil `*E` implementing error `== nil` | false | false | false |
| two maps `a == b` | false | false | (rejected) |

The missing-key row is the one that was not obvious going in: a map miss
hands back `reflect.Zero` of the ELEMENT type, so `m["zz"] == nil` on a
`map[string][]int` had the same bug for the same reason. It came for
free and is now asserted.

`a == b` on two maps still answers false via the `recover` — Go rejects
it at compile time, grsh has no compile time, and that silence is
unchanged. Only the untyped-nil branch moved.

## Guard pass

Four mutations across the two passes, all bite. The first two:

- **unwrap disabled** (`return false && rv.IsNil()`) — fails the three
  nil tests AND the golden. Proves the tests reach the new code.
- **unwrap universal** (drop the kind switch, `return true`) — fails
  `TestZeroScalarsDoNotEqualNil` on `0 == nil`. Proves the kind switch
  is doing work and is not decoration.

The pointer pass added two more, listed in the addendum.

Restored from a `cp` backup rather than `git stash`, per the last
session's note about a tool timeout stranding the tree mid-stash. The
backup approach has no window where the working tree is not the work.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green. Seven unit
tests in `expr_test.go`, and a new golden
`testdata/scripts/nil_compare.grsh` (100 scripts now), covering the nil
pair, built-but-empty, the map miss, scalars, both control-flow routes,
a live and a nil pointer, and the typed-nil error from both sides. Real
binary smoke-tested on the scratch scripts before the tests were
written, including the `regexp.Compile` pair.

`docs/LANGUAGE.md` — new Semantics-notes bullet, naming the error
exception and the reason for it.

## Method notes

- **Read the storage, not the operator.** The instinct was to look at
  `==`; the answer was in `TypeDesc.Zero`. Two minutes of tracing where
  a nil map actually COMES FROM turned a vague "reference types are
  broken" into a one-branch fix.
- **The exclusion needed more thought than the inclusion.** Adding
  Map/Slice/Func/Chan was mechanical. Deciding pointers stay out
  required naming which wrong answer is cheaper, and that reasoning is
  worth more in the comment than the code it guards.
- **Smoke-test the real binary before writing assertions.** Running the
  scratch script first meant the `.want` file and the unit tests were
  recording observed behavior that had already been eyeballed against
  Go's rules, not encoding a guess.

## Open

Carried forward from the struct-map-keys session, still open:

- **`x.([]P)` cannot distinguish `[]P` from `[]Q`**, and `map[P]int` /
  `map[Q]int` are the same erased type. Contents never collide.
- **`m[missing]` on a struct-valued map yields nil**, not the zero
  struct. Note this is now the LAST reference-zero case that diverges —
  the slice-element case was fixed here, but a struct's zero cannot be
  handed back by `reflect.Zero` on the erased type.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- ~~**A nil pointer does not equal nil**, new and deliberate, above.~~
  **SUPERSEDED later the same session.** A nil pointer now DOES equal
  nil; the exclusion narrowed from "every pointer" to "a pointer whose
  type implements error", which is exactly the cut `.(error)` makes.
  See the pointer section added below.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

---

## Addendum: the pointer case, fixed

The exclusion above was too wide. It answered a question grsh cannot
answer statically — "is this pointer someone's error?" — by refusing
every pointer. But the interpreter DOES answer that question, in two
places, and it uses Go's own test:

```go
if lastErr, ok := last.(error); ok || last == nil {   // expr.go, assignRHS
if last, ok := vals[len(vals)-1].(error); ok && last != nil {  // interp.go, stmt abort
```

`.(error)` succeeds exactly when the dynamic type implements `error`,
typed nil included. So the cut was already drawn; `isNilRef` only had to
use the same one:

```go
case reflect.Pointer:
	return !rv.Type().Implements(errorInterface) && rv.IsNil()
```

This is a stronger justification than the original "errors matter more."
The rule is not a judgement about which pointers are important — it is
**the script's `== nil` agreeing with its own runtime's error
detection.** Any other cut lets one value be a live failure to the
interpreter and nil to the script at the same time.

The case that proves it is reachable, not hypothetical:

```
re, err := regexp.Compile("(")   // re is a nil *regexp.Regexp
fmt.Println(re == nil)           // was false, now true -- Go says true
```

`regexp.Compile` is in the registry, returns `(*Regexp, error)`, and
hands back a nil pointer on a bad pattern. The old answer was wrong on a
one-liner a script would actually write.

**Guards, both bite:**

- pointers never nil (`return false`) — fails `TestNilPointerEqualsNil`
  and the golden.
- error exception dropped (`return rv.IsNil()`) — fails
  `TestTypedNilErrorDoesNotEqualNil` with `7 true false`, the exact
  disagreement the exception exists to prevent.

The second test asserts BOTH halves on one value: the script sees
`err != nil`, and the one-value form of the same call aborts. Either
half alone would pass under a rule that is quietly inconsistent.

**Method note.** The first version's comment argued that the tradeoff
was unavoidable. Writing that argument down is what exposed it as false
— naming the error case forced a look at how the runtime SPOTS an error,
and the answer was one grep away. A comment that has to work hard to
justify a limit is worth re-reading as a hint that the limit is wrong.
