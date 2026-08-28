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

## What is deliberately NOT unwrapped

**POINTER.** This is the whole judgement call in the change.

grsh has no static types: a script's own nil `*os.File` and the typed
nil a Go callee stuffed into an `error` are the SAME value once they are
a `Value`. There is no information left to tell them apart. So the two
cases have to share one answer, and the choice is which one to get
wrong:

| unwrap pointers? | `p == nil` on a nil `*T` | `err == nil` on a typed-nil error |
|---|---|---|
| yes | true (Go agrees) | true — **swallows a real error** |
| **no (chosen)** | false (Go disagrees) | false (Go agrees) |

The second column decides it. Go itself answers false there, and the
error-return convention is load-bearing in this language — `data :=
os.ReadFile(...)` aborts the script on a non-nil error. Making that
check silently pass is a much worse failure than a rare nil pointer
comparing wrong, and it also leaves the pre-existing behavior untouched
rather than changing it in the risky direction.

**STRUCT.** A `*StructVal` never reaches `safeEqual` through `==` at
all; `binaryOp` intercepts it and routes to `valuesEqual`, whose
`xs == nil && y == nil` already answered a typed-nil struct correctly.
Unwrapping pointers here would have collided with that, since
`*StructVal` is a pointer kind — another reason the exclusion is right.

Both exclusions are written into the function's comment, with the
reasoning, because the next reader's instinct will be to "finish the
job" by adding `reflect.Ptr` to the switch.

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
| two maps `a == b` | false | false | (rejected) |

The missing-key row is the one that was not obvious going in: a map miss
hands back `reflect.Zero` of the ELEMENT type, so `m["zz"] == nil` on a
`map[string][]int` had the same bug for the same reason. It came for
free and is now asserted.

`a == b` on two maps still answers false via the `recover` — Go rejects
it at compile time, grsh has no compile time, and that silence is
unchanged. Only the untyped-nil branch moved.

## Guard pass

Two mutations, both bite:

- **unwrap disabled** (`return false && rv.IsNil()`) — fails the three
  nil tests AND the golden. Proves the tests reach the new code.
- **unwrap universal** (drop the kind switch, `return true`) — fails
  `TestZeroScalarsDoNotEqualNil` on `0 == nil`. Proves the kind switch
  is doing work and is not decoration.

Restored from a `cp` backup rather than `git stash`, per the last
session's note about a tool timeout stranding the tree mid-stash. The
backup approach has no window where the working tree is not the work.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green. Five unit tests
in `expr_test.go`, a new golden `testdata/scripts/nil_compare.grsh`
(100 scripts now), covering the nil pair, built-but-empty, the map miss,
scalars, and both control-flow routes. Real binary smoke-tested on both
scratch scripts before the tests were written.

`docs/LANGUAGE.md` — new Semantics-notes bullet, naming the pointer
divergence explicitly.

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
- **A nil pointer does not equal nil**, new and deliberate, above.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.
