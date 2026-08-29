# Session: Closing appendValue's fmt Fallback

Session: cda1e10a-040c-4f36-94c2-dd9cda68a2ce
Date: 2026-08-29

Continues `2026-0829-0926-ordering-a-map-costs-more-than-decoding-it.md`,
taking one open item:

> **`fmt.Appendf` is still the fallback for any field type not in
> `appendValue`'s switch.** A script holding, say, a `[]string` field in a
> map key pays fmt per key. Adding cases is cheap if one ever does.

## The item's own premise was wrong, and the work was still worth doing

A `[]string` field cannot be part of a map key. `keyValue` rejects any
field whose value is not Go-comparable before the key is built, so a
struct with a slice field is refused as a key type outright — the
ordering pass never sees one. The stated motivation ("pays fmt per key")
was unreachable.

What IS reachable is everything else `appendValue` feeds: `String` is how
a struct prints anywhere, so `fmt.Println(s)` on a struct with slice
fields went through fmt per element, on every print. Different caller,
same cost, so the item stood.

## What the fallback actually cost

Measured before writing anything, ns for one value:

| value | via fmt | allocs |
|---|---|---|
| `[]string` × 16 | 551 | 16 |
| `[]int` × 16 | 554 | 16 |
| `[]P` × 16 | 1043 | 32 |
| `map[string]int` × 3 | 358 | 7 |
| `[]byte` × 8 | 97 | 0 |
| an `int` (fast path) | 7.6 | 0 |

fmt reflects over the slice and puts every element through an interface,
which is where the per-element allocation comes from. `[]P` pays twice:
the box, plus a fresh string from the minted type's promoted `String`.

## The list of cases is CLOSED, and that is the whole design

The previous session's note on the switch — "the cases are the types a
script can put in a field" — turned out to be checkable rather than
aspirational. `typeOf` resolves a field's element type from `typeIdents`,
a nine-entry map: `int`, `int64`, `float64`, `string`, `bool`, `byte`,
`rune`, `any`, `error`. Plus a script struct. So `[]T` for every `T` a
script can spell is **exactly ten shapes**, and "add cases if one ever
does" is answerable in full rather than case by case.

Nine are static cases in the switch. The tenth, `[]P`, has no static case
because its element type is minted at runtime.

```
                fast    fmt      allocs
[]string n16    37.3   545.3    0 <- 16
[]int    n16   124.7   646.7    0 <- 16
[]float64 n16  341.9   892.4    0 <- 16
[]any    n16   156.1   785.9    0 <- 0
[]P      n16   301.2  1055.0    0 <- 32
[]string n64   141.1  2140.0    0 <- 64
[]P      n64  1151.0  3964.0    0 <- 128
```

2.6x to 15x, and no rendered slice allocates at any length.

## The `[]P` path is affordable for a reason store.go already wrote down

There can be no `case []P:` — the type is made by `reflect.StructOf` at
declaration time. So after the switch:

```go
if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && storeOwnerOf(rv.Type().Elem()) != nil {
    ...
    sv, _ := rv.Index(i).Field(0).Field(0).Interface().(*StructVal)
    b = sv.appendTo(b)
}
```

Two `Field` hops — minted type, carrier, struct — the same walk
`fromStore` takes. The `Interface()` allocates nothing because what it
boxes is a POINTER, which is the shape property store.go's whole design
already rests on ("the type is one pointer wide and pointer-shaped").
That is why this specific reflect path is worth having and a GENERIC
reflect path over any slice is not: `rv.Index(i).Interface()` on a
`[][]string` boxes a slice header and allocates exactly as fmt does, so
it would buy nothing.

The `Kind()` guard is first so that everything still heading for fmt — a
map, a nested slice, a stdlib type — pays one comparison rather than a
map lookup.

## Three things measured and declined

**A generic helper.** Nine near-identical loops asked to be one
`appendSeq[T](b, xs, one func([]byte, T) []byte)`. Measured at 16
elements: the indirect call survives instantiation and costs **43% on
`[]string` (35→51ns) and 13% on `[]int`**, which is most of what the case
buys. Written out instead.

**Maps.** `map[string]int` is 358ns and 7 allocations through fmt and
looks like the same kind of win. It is not the same kind of change: `%v`
on a map prints its keys SORTED, by `internal/fmtsort`'s ordering, which
is field-wise for a struct key and kind-wise elsewhere. Matching it means
reimplementing that ordering, and a fast path that renders a map's
entries in a different order from fmt is not a fast path, it is a second
definition. Left on fmt.

**A regression on the scalar path.** Nine more arms on a type switch
could plausibly cost the common case. A/B against the stashed file, best
of six at 2M iterations:

| | before | after |
|---|---|---|
| `int` | 9.35 | 8.97 |
| `string` | 2.58 | 2.52 |
| `float64` | 30.37 | 29.91 |
| `nil` | 1.90 | 1.82 |

Nothing. A Go type switch over concrete types resolves through an
interface-type lookup, not a linear scan, so arms are close to free.
`BenchmarkSortMapKeys` is unchanged too.

## The test that had to read the source

Every new case is a SPEED change, and speed is only sometimes
observable. Seven of the nine are detectable by allocation count, because
fmt boxes per element and the fast path does not. **Two are not**: fmt has
its own fast path for `[]byte`, and an `error` already carries its
interface, so removing either case changes neither the bytes nor the
allocations. Behaviour cannot distinguish those cases from their absence.

So `TestEveryScriptSliceTypeHasAFastPath` parses `structs.go`, finds
`func appendValue`, and collects the element type names of its
`case []T:` arms. Three assertions, in three directions:

- every `typeIdents` name has a sample in the test — adding a native type
  name fails until someone writes one,
- every sample has a `case` in the source — removing a case fails, and
  this is the only assertion `[]byte` and `[]error` have,
- every sample renders byte-for-byte as fmt does, and allocates zero.

A case nothing can falsify is indistinguishable from a case nobody wrote.
Reading the source is what makes the last two falsifiable.

## The element values in that test are chosen, not arbitrary

`[]int{1, 2}` would pass with the case removed. Boxing an int in
0..255 hits the runtime's cached-small-integer table and allocates
nothing, so fmt's per-element box would be invisible. The samples use
`1 << 40`, runes above 100000, and distinct strings, so that the thing
the assertion is looking for is actually there to be seen.

Same reasoning for `[]any`: the sample holds a `*StructVal`, because a
`[]any` of scalars costs fmt nothing to box — the elements are already
interfaces — and it is the nested struct's `String()` that makes fmt
allocate four times where recursion allocates none.

## New tests

- `TestAppendValueMatchesFmt` grew from 21 values to 45. Every slice case
  appears in three states — several elements, one element, empty, and
  nil — because the frame and the separator are written out per case, and
  a case that drops the brackets on an empty slice or emits a leading
  space on a one-element one is wrong in exactly one state. Plus
  `[]string{"", " ", "with space"}`, where the separator is genuinely
  ambiguous and fmt's is too, and three values that must STILL fall
  through: a map, a `[][]string`, and a `[]uint`.
- `TestEveryScriptSliceTypeHasAFastPath`, above.
- `TestASliceOfStructsRendersWithoutFmt`. The `[]P` path, with the slice
  built by a SCRIPT rather than by calling `mintStoreType` in the test —
  a hand-built `[]*StructVal` would pass while the reachable value fell
  through to fmt. It asserts the value really is a minted-element slice
  before testing it, matches fmt, allocates zero, and renders a nil
  element as `<nil>`.
- `TestAStructWithSliceFieldsPrintsLikeFmt`. The same claim from the
  outside, through the interpreter, with all ten shapes in one struct.
- `BenchmarkAppendSliceValue`. Fast path against fmt at 4, 16 and 64
  elements, so the numbers quoted in the comments stay reproducible.

## The probe pass

Twelve probes, each reverting one piece; every one fails a named test.
Notably: removing the `[]byte` or `[]error` case is caught ONLY by the
source-parsing assertion, which is why it exists; a comma separator, a
missing closing bracket, `%f` for `%g`, and a case that writes `b[:0]`
instead of appending all fail on output parity; dropping the minted
path's owner check makes it swallow every slice and fails the fmt table.

**The harness lied once again, differently.** Probe 5 renders `[]byte` as
text, which puts control bytes in the test output; `grep` decided the
stream was binary and printed `Binary file matches` instead of the
failures, so the probe read as `Binary` rather than as three failing
tests. Last session the harness could not tell `NOTHING FAILED` from a
build error; this session it could not tell a failure from a non-UTF-8
byte. `grep -a` fixes it. **A probe harness's own output is a parser, and
parsers meet inputs the author did not picture.**

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End-to-end through `cmd/grsh`: a struct with all ten slice
shapes plus a map field prints identically under `Println` and `%v`; the
zero struct prints every empty slice as `[]`; a `[]P` with an appended
nil renders `[P{A: 1} <nil>]`; a slice field inside a map VALUE survives
a range; a struct-keyed map still ranges in order. `unsafe` in
`internal/interp` is unchanged — this change adds none.

## Method notes

- **Check the item's premise before taking the item.** "Pays fmt per key"
  was false — a slice field cannot be in a map key at all. The work was
  still right, for a different caller. Had the premise gone unchecked,
  the session would have optimised the ordering pass, measured no
  improvement in `BenchmarkSortMapKeys`, and concluded wrongly.
- **A closed set beats a judgement call.** "Add cases if one ever does"
  invites an endless trickle of one-off decisions. `typeIdents` turns it
  into a finite list of ten with a test that keeps it finite — and the
  test now fails when the LANGUAGE grows, which is the moment the
  decision actually needs making.
- **Ask what a fast path can be tested by BEFORE writing it.** Two of the
  nine cases are behaviourally invisible. Noticing that during the design
  is what produced a source-parsing test; noticing it afterwards would
  have produced two untested cases and no reason to look.
- **A test's inputs can hide the thing it tests.** `[]int{1, 2}` boxes
  without allocating. An assertion on allocation count is only as good as
  the values it runs on.
- **Symmetry is not a reason on its own — this is the second session in a
  row it was priced and refused.** Last session it was routing strings
  through the struct branch's slab; this session it was nine loops
  through one generic helper. Both looked tidier and both cost more than
  the fast path saved.

## Open

- **`map[K]V` fields still render through fmt**, at 358ns and 7
  allocations for three entries. Closing it means reproducing
  `internal/fmtsort`'s key ordering exactly, including for a minted
  struct key. Priced and declined above, not forgotten.
- **A nested `[][]T` still renders through fmt.** A generic reflect walk
  would cover it and buy nothing — boxing a slice header allocates
  exactly as fmt does — so the fix, if one is ever wanted, is another
  static case rather than a general path.
- **`[]error` elements still reach fmt one at a time.** The case saves
  the reflect walk over the slice, not the per-element format, since
  rendering an error means calling `Error()`.
- **A range over a struct-keyed map costs ~62-87ns a key at one field
  and ~166-211 at ten.** Unchanged: a key cannot hold a slice, so nothing
  here touches that path.
- **Ties in the sort are still resolved arbitrarily.** Unchanged.
- **`growForRest`'s estimate is untested**, deliberately.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is
  a narrow call** — 13% margin at eight fields.
- **`unsafe` in `internal/interp` is five aliasing expressions** in
  `structs.go`, plus `svSlots`.
- **A re-declared `P` does not find its own earlier keys.** Unchanged.
- **`[]map[P]int{{{1}: 2}}` cannot elide the key literal.** Unchanged.
- **`%T` on a container prints storage.** Unchanged.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** --
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

~~**`fmt.Appendf` is still the fallback for any field type not in
`appendValue`'s switch.**~~ Closed for slices, which is the whole of what
a script can spell as a field's container except a map: nine static cases
plus the minted `[]P` path, all allocation-free.
