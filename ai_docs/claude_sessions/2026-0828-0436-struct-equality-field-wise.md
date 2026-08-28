# Session: Struct Equality, Field-Wise

Session: f09f8e39-647c-4189-b4b6-d8eb5a6dbb12
Date: 2026-08-28

## Goal

The last session closed script struct types in TYPE position and named
**struct equality** as the natural next step. This session does it.

- `75f831f` — Struct equality compares fields, not identities

## The bug the erasure left behind

`p == q` fell through `binaryOp` to the fallback `x == y`, and a
`*StructVal` is a POINTER. So equality was identity:

```
P{1, "a"} == P{1, "a"}     false     two literals, two instances
b := a; a == b             false     := COPIES, so b was never a
```

The second line is the damning one: copy-on-store is what gives grsh Go's
value semantics, and it was the very thing making a struct unequal to
itself.

## Where it hooks

`structEqual` walks the fields. `switch` and `!=` needed nothing —
`evalSwitch` already calls `binaryOp(EQL)`, and `NEQ` is the same branch.

The interception sits **after every native kind** in `binaryOp` (strings,
bool, int, float) and before the fallback. A struct is none of those, so
an int comparison returns long before the two type assertions — which is
why every loop benchmark is unmoved.

## The design question: when is a struct comparable?

Go answers from the STATIC field types, at compile time. grsh has no
compile time, so the choice was where to put the verdict:

| approach | why not |
|---|---|
| check each field VALUE at compare time | `p == q` succeeds while a slice field is nil and starts failing the moment it is set |
| refuse all structs with any exotic field, silently returning false | a wrong answer where Go gives an error |
| **compute once at declaration, from the field types** | chosen |

`StructType.noCmp *cmpDefect` is nil for a comparable struct, so the gate
is one nil check. When set it names the culprit:

```
Out cannot be compared with ==: field I.Tags has type []string
```

The path is dotted because a struct field erases to a POINTER, and
reflect calls every pointer comparable — `RT.Comparable()` would wave
through a struct whose nested type is the problem. Only the nested type's
own verdict is correct, and it is already final: a field type must be
declared before the struct using it, so the type graph is a DAG.

## Three cases the static verdict cannot reach

**A func field.** grsh leaves func types unresolved (its closures are
`*Closure`, not reflect funcs), so `TypeDesc.RT` is nil and there is
nothing to ask. The verdict is read off the **syntax** instead —
`ast.Unparen(e).(*ast.FuncType)` — because leaving it to the value walk
recreates exactly the instability the static verdict exists to prevent:
two nil `Fn` fields would compare equal until either was set.

**An `any` field.** Go calls `interface{}` comparable and panics at
runtime when it holds a slice. Same here, except it reports:
`cannot compare a field holding []int`.

**A type grsh does not model at all.** Value-checked, same path.

## Two decisions that look like omissions

**Different struct types answer `false`, not an error.** Go rejects
`P{1} == Q{1}` at compile time. The rest of the interpreter already
answers `false` for a cross-type `==` (`1 == "a"` is not a complaint), and
consistency with the surrounding design beat imitating a compile step
that does not exist.

**There is deliberately no `a == b` identity fast path.** It would be
correct for the ANSWER and wrong for the ERROR: an incomparable struct
compared to itself would quietly succeed while the equal-looking twin
failed. A diagnostic that depends on whether the operands happen to be
the same instance is worse than no diagnostic.

## The map-key refusal stays, and last session's note was wrong

The Round-7 doc said field-wise comparison "would make both work" —
equality and struct map keys. It does not. The interpreter owns the `==`
operator; it does not own the MAP. `reflect.Map` hashes and compares the
erased `*StructVal` with Go's own runtime, which this package cannot
reach into. Keying by struct needs a hashable encoding of the value,
which is a different feature. The comment at the refusal says so now
instead of promising equality would lift it.

## Recursion terminates — but not for the reason first written

The first draft of `structEqual`'s doc claimed the type DAG bounds the
recursion. That is wrong for a self-referential field: `type N struct {
Next N }` leaves `Next` unresolved, and it still holds a real struct at
runtime, outside the DAG.

The real argument is `copyStruct`'s: a struct value cannot CONTAIN
itself, because `a.Next = a` stores a copy taken before the write.
Copy-on-store is what makes a cycle unconstructible. (A cycle through a
SLICE field is constructible, but a slice never recurses in
`valuesEqual` — it hits the non-comparable report.)

## Cost: nothing moved

| bench | before | after | allocs |
|---|---|---|---|
| Loop/plain | 235.8 | 235.2 ns/iter | identical |
| Loop/nested-if | 324.4 | 323.5 | identical |
| Loop/range | 215.4 | 215.8 | identical |
| Loop/closure-call | 452.3 | 450.6 | identical |
| StructCopy/nested | 395.2 | 392.0 | identical |
| StructZero/nested | 413.3 | 410.3 | identical |

Every shape within noise at n=5, allocations byte-for-byte. `declareType`
gained one `compareDefect` call per field, once per declaration.

**Binary size is byte-for-byte unchanged (11,154,818).** That was a
decision, not luck. Rendering the func SIGNATURE in the error message
needs an AST printer, and both were measured:

| renderer | binary cost | message |
|---|---|---|
| `go/types.ExprString` | +1.6MB (+14%) | `field Fn has type func(int) error` |
| `go/printer` | +276KB (+2.5%) | same |
| **the bare kind word** | **0** | `field Fn has type func` |

The FIELD NAME is the actionable half of the message and it is already
there. A shell does not spend 14% of its binary on decoration.

## Fixed in passing

The type error at the bottom of `binaryOp` used `%T`, which printed
`*interp.StructVal` — grsh's own internals — at the one place whose job is
describing the script's values. `valTypeName` reads the name off the
INSTANCE (reflect sees only the erased storage type and could never say
which struct), so it is `operator < is not defined on P and P` now.

## Method notes

- **A MISSED guard is a claim about which line a test covers** — the same
  lesson as the last two rounds, hit again. `structEqual`'s nil guard
  reported MISSED because the obvious test, `xs[0] == nil`, has an
  UNTYPED nil on the right and is answered one level up in `valuesEqual`.
  Only a comparison whose BOTH sides came out of a `[]P` reaches
  `structEqual` with a nil. Added that test; the mutation panics now.
- **Measure before paying for a nicety.** `go/types.ExprString` is the
  obvious call for rendering a type expression and it costs 14% of the
  binary. Two `go build`s answered a question that would otherwise have
  been settled by taste.
- **A previous session's "next step" is a hypothesis, not a spec.** The
  Round-7 doc's claim that equality would unlock map keys did not survive
  contact with where the comparison actually happens.
- **Guards broken:** the `binaryOp` struct route, the cross-type false,
  the `noCmp` gate, `structEqual`'s nil operands, the field walk itself,
  `compareDefect`'s nested verdict, its func-syntax branch, its
  `RT.Comparable` case, first-defect-wins in `declareType`,
  `valuesEqual`'s struct-vs-non-struct branch, its nil pair, its dynamic
  `Comparable` check, `valTypeName`, and the map-key refusal. Fourteen
  guards, all bite; tree hashed before and after, restored clean.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, including the
golden suite (a new `struct_equality.grsh`, 48 scripts) and the pty e2e.
Benchmarks A/B'd at n=5 by stashing the tree. Real binary smoke-tested on
a script mixing `$(...)`, a shell line, `export`, a struct-typed field,
`[]P`, a `map[string]P`, a typed nil and a `switch` on a struct.

`docs/LANGUAGE.md` updated: the "no struct equality yet" limit is gone,
replaced by what equality does and what it still refuses.

## Open

- **`x.([]P)` cannot distinguish `[]P` from `[]Q`.** Unchanged; `x.(P)`
  is exact. Pinned with a test.
- **`m[missing]` on a struct-valued map yields nil**, not the zero
  struct. Unchanged, pinned.
- **Struct map keys** need a hashable encoding of the value — the honest
  next step for that feature, and unrelated to `==`.
- **An `Equal` method is not consulted.** The refusal message suggests
  writing one, but `==` does not dispatch to it. Go does not either, so
  this is a deliberate non-feature unless script authors ask.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.
