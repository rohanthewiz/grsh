# Session: Every Map Key in fmt's Order

Session: 058ee31e-c404-4d2c-be73-0fc59747792b
Date: 2026-08-29

Continues `2026-0829-1229-ranging-a-map-in-fmts-order.md`, which closed
the struct-keyed half and left this as its first Open item:

> **An int-keyed map still ranges in the map's own randomised order**, and
> fmt sorts it numerically -- so it is now the only map shape whose range
> and print disagree, and the only one whose range is not deterministic at
> all.

Taken here, and it turned out to be every non-struct key type, not just
int.

## What was actually missing

`sortMapKeys` had exactly two branches that ordered anything: a string key
and a minted struct key. Everything else fell out of the top of the
function with `return nil`, which the caller reads as "range them in
whatever order `MapKeys` handed back". So `map[int]V`, `map[float64]V`,
`map[bool]V`, `map[rune]V`, `map[byte]V`, `map[int64]V` and `map[any]V`
were all unordered -- and int is the second commonest map a script writes.

Three runs of a ten-entry `map[int]string`, before:

```
range: 0 1 4 7 9 2 3 5 6 8
range: 2 3 4 6 7 8 9 1 5 0
range: 3 4 6 8 0 1 5 7 9 2
```

## The fix is one comparator per KIND, and Kind is the right question

`orderScalarKeys` sorts the map's own keys in place. There is nothing to
decode -- a script's int key IS the map's int key -- so it allocates
nothing and the caller reads the keys directly.

**It switches on `reflect.Kind`, which is the exact opposite of what
`mapValRender` does, and both are right.** Rendering a named integer type
has to go by TYPE identity, because `%v` calls its `String` method and
prints a `time.Duration` as `3s`. Ordering never calls `String`: fmtsort
switches on Kind and compares the numbers, so two `time.Duration`s sort 1
before 2 however they print. Last session's "Kind is not Type" lesson has
a mirror image, and this is it.

The kinds are grouped as FAMILIES -- all the Int widths, all the Uint
widths, both Floats -- rather than as the types `typeIdents` can spell.
A map does not have to come from a script's own type expression; a stdlib
call can hand one back to range, and fmtsort orders those too.

Cost, zero allocations at every count:

```
keys        4    16    64   256  1024
 int      8.2  13.0  20.9  26.6  39.2
 string   8.2  13.9  27.1  35.0  50.2
```

The string row is the comparison worth making: same shape, same sort, one
`Int()` load against one `String()` header load.

## An interface key is a struct key's field, one level up

`map[any]V` is fmtsort's interface case: nil first, then the dynamic TYPE
by address, then the value. That is `keyCmp.field` exactly -- written last
session for struct key fields, tested, and reused here unchanged.

So a `map[any]int` whose keys are all ints orders numerically, which is
reproducibly what fmt does, and one holding an int and a string DECLINES,
because fmt's answer there is which of `int` and `string` was linked at
the lower address.

A decline falls back to the rendered-text order -- the same fallback a
declined struct-keyed map takes, for the same reason, and now literally
the same code. `textOrder` stopped embedding `keyOrder` to serve both: a
declined struct-keyed map has decoded structs to move with the keys, and a
declined `map[any]V` has nothing but the keys, so `decoded` is nil and
`textOrder.Swap` says so rather than making the struct path's own sort
carry the check.

The render is worth nothing to the accepted path, so it is not built until
the decline is known: a `map[any]int` of plain ints pays one wasted sort
and no memory.

## What is deliberately still unordered

Pointer, channel and `unsafe.Pointer` keys, whose fmt order is a machine
address and whose TEXT is also an address, so no fallback can make them
deterministic either; and complex, array and native-struct keys, which
fmtsort compares element-wise. None of them is spellable as a map key type
in a script. They range in the map's own order, exactly as every scalar
key did before.

## The probe pass, and two probes that were wrong

Eleven probes, all eleven caught by a named test: int keys unordered, int
reversed, uint compared as signed, float compared as text, bool inverted,
interface keys unordered, the interface decline fallback removed, the
fallback sorting nothing, `textOrder.Swap` dropping the keys, dropping the
decoded, and string keys reversed.

Two first drafts were bad probes and the harness said so rather than
reporting a false negative:

- **"uint compared as signed" as `int64(a.Uint())`** is not a mutation at
  all: a `uint32` widened to `int64` is always positive, so the order is
  identical. Rewritten as `int32`, where the top half of the range flips,
  it fails immediately.
- **"one-key shortcut removed"** was uncaught, and correctly. The `n == 1`
  return is load-bearing for a STRUCT key -- it skips an ordering pass
  worth 16-18ns a key -- and worth almost nothing for a scalar one, where
  it only avoids a one-element `slices.SortFunc`. The comment at the
  branch now says which half is which, and that no test pins the scalar
  half because removing it changes no answer.

That is a third and fourth entry in this project's running list of ways a
probe reports a false negative: the mutation was a no-op, and the thing it
mutated was a tuning choice rather than a behaviour.

## The NaN key forced a test decision, and turned up a crash

`TestAScalarKeyedMapRangesInFmtsOrder` rebuilds fmt's map text out of the
order a range would visit and compares the bytes -- the same shape the
struct-key parity test uses. Its float row includes NaN, because NaN sorts
below every other float under fmtsort and `cmp.Compare` agrees.

**Every value in those maps is 1, and that is forced rather than lazy.**
NaN does not equal itself, so `MapIndex` cannot find a NaN key and a
distinct value per entry could not be read back to rebuild the text.
Nothing is lost: a scalar key is not paired with a decoded slice the way a
struct key is, so there is no pairing for a distinct value to catch.

Which is also how the crash surfaced. `rangeOver` reads each value with
`rv.MapIndex(k)`, so ranging any map with a NaN key dies:

```
grsh: grsh internal error: reflect: call of reflect.Value.Interface on zero Value
```

It predates all of this -- `MapKeys` plus `MapIndex` has always been the
shape -- and fixing it means `rangeOver` carrying values alongside keys
from one `MapRange` pass, which is a per-range cost on every map. Left
open rather than fixed on the way past.

## New tests

- `TestAScalarKeyedMapRangesInFmtsOrder` -- randomised, nine key kinds
  including `uint32` past `1<<31` and a NaN float, four sizes, held
  against fmt's own map text.
- `TestAnInterfaceKeyedMapDeclinesToTextOrder` -- ten runs from ten
  different `MapKeys` permutations, asserting the text order specifically,
  which is neither fmt's answer nor a numeric one and so could only come
  from the fallback having run.
- `TestAScriptRangesAnIntKeyedMapLikeFmtPrintsIt` -- the script-level
  half, ranging and printing the same map, with the printed side coming
  from Go's own fmt on a bare map.
- `BenchmarkSortMapKeys` gained an `int` row, which used to cost nothing
  because it did nothing.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End to end through `cmd/grsh`, three runs each: int, float64,
bool, rune, byte, `any` of one type and `any` of mixed types -- every
range matching its print, and the mixed `any` map identical across runs.

## Method notes

- **An Open item can be bigger than it was written.** "An int-keyed map"
  was really "every key type that is not a string or a struct", which the
  first grep at the branch showed immediately. The item was written from
  the symptom that had been measured, not from the code.
- **Two right answers to the same question, in opposite directions.**
  Kind for ordering, Type for rendering. The pair is only obvious once
  both exist, which is why the doc on each now names the other.
- **A comparator written for one caller was the whole answer for
  another.** `keyCmp.field` was written for a struct key's dynamically
  typed field; an interface-keyed map is that case verbatim, one level up.
  No new code, and it inherits the parity test that already holds it
  against fmtsort.
- **A probe that mutates nothing reads exactly like a missing test.**
  Widening a uint through int64 preserves order, so the probe passed and
  looked like a hole. The fix is the same as the previous three: make the
  harness and the reader suspicious of a probe that survives.

## Open

- **Ranging a map with a NaN key crashes** in `rangeOver`, which reads
  values with `MapIndex` and cannot find a key that does not equal itself.
  Pre-existing, surfaced by this session's float test, NOT fixed: the fix
  is one `MapRange` pass carrying values beside keys, which is a cost on
  every map range and a decision of its own.
- **Pointer, channel, complex, array and native-struct keys stay
  unordered.** None is spellable in a script; for the address-shaped ones
  no order can be deterministic anyway.
- **A one-field struct key past ~256 entries orders 18% slower** than the
  text order it replaced. Priced last session, documented at the call
  site, unchanged.
- **A scalar-keyed map field still RENDERS through fmt.** Unchanged, and
  now a little odd-looking beside this: grsh orders scalar keys itself for
  a range but hands the same map to fmt to print. Both were measured; the
  render lost twice and the sort wins, because a range needs an order it
  cannot buy from fmt at all.
- **Two allocations an entry remain** in `appendStructKeyedMap`.
  Unchanged.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time. Unchanged.
- **Ties in the text-order fallback are still resolved arbitrarily.**
  Unchanged.
- **`growForRest`'s estimate is untested**, deliberately. It now has two
  callers, both on fallback paths.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is a
  narrow call** -- 13% margin at eight fields.
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

~~**An int-keyed map still ranges in the map's own randomised order.**~~
Closed, along with every other scalar and interface key. Every map a
script can spell now ranges deterministically, and in fmt's order wherever
fmt's order is reproducible.
