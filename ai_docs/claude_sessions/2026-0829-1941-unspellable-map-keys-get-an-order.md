# Session: Unspellable Map Keys Get an Order

Session: 05b254b6-c296-421f-bd15-a6efd186119f
Date: 2026-08-29

Takes an Open item from `2026-0829-1837-nan-key-range-crash.md`:

> **Pointer, channel, complex, array and native-struct keys stay
> unordered.** Unchanged. Note that complex and array keys now take the
> paired pass -- they can hold a NaN -- while still not being ordered.

Two of the five closed, and the parenthetical that had kept all five off
the list -- *unspellable in scripts* -- turned out to be the wrong test.

## The item was wrong twice

**`fmtsort` orders three of the five reproducibly.** A complex by real
then imag, an array and a struct element by element. There was an order
to match; only pointer, channel and `unsafe.Pointer` are answered by a
machine address.

**Unspellable is not unreachable.** `typeIdents` offers no array type and
no foreign struct, so `map[[4]int]V` cannot be written -- but a
`map[any]V` KEY boxes whatever a stdlib call handed back, and that door
is open. This is a disagreement anyone could write, reproduced before
touching anything:

```
m := map[any]int{}
m[time.Hour] = 1; m[time.Second] = 2; m[time.Millisecond] = 3; m[time.Minute] = 4
fmt.Println(m)      map[1ms:3 1s:2 1m0s:4 1h0m0s:1]
for k := range m    1h0m0s, 1m0s, 1ms, 1s
```

## The cause: one comparator asked for a TYPE where fmt asks for a KIND

`keyCmp.field` switches on the concrete Go type, so `case int64` does not
catch a `time.Duration`. It fell out of the switch, declined, and the
text-order fallback put `1h0m0s` first because `'h' < 'm'`. `fmtsort`
switches on `reflect.Kind` and compares the numbers.

`orderScalarKeys` had this right already and says so in its own comment
-- "Kind is exactly the question here". The interface/struct-field path
underneath it did not.

## What was built

- **`keyCmp.rv`** -- `internal/fmtsort.compare` mirrored over
  `reflect.Value`: complex, array, struct and interface, recursively.
  `field`'s tail now calls it instead of declining, so the fast switch
  stays a fast path and everything else is reached the slow way.
- **`orderScalarKeys` -> `orderNativeKeys`**, splitting key kinds three
  ways: scalar (sorted in place), declinable (array / native struct /
  interface -- sorted, then a rendered-text fallback), unordered.
- **`orderDeclinableKeys`** -- the shared "sort, and if the comparator
  gave up, sort by the text instead" shape, extracted from
  `orderInterfaceKeys` and now serving `orderCompositeKeys` too.
- `cmpComplexKeys` beside the other scalar comparators; `scalarOrder` ->
  `pairedOrder`, since it is no longer scalars-only.

### Two rules the code now carries

**`rv` must never call `Interface()`.** Struct recursion descends into
UNEXPORTED fields -- `time.Time` is `wall`, `ext` and a `*Location`, none
of them exported -- and `Interface` panics on a Value obtained that way,
where `Int`, `Uint`, `Float`, `Complex`, `Bool`, `String`, `Len`,
`Index`, `NumField`, `Field` and `Elem` all read it fine. `fmtsort`
carries the same rule at the top of its file.

**The `*StructVal` case must stay ABOVE the reflect fallback.** A
`*StructVal` is a pointer, so `rv` would compare two decoded structs by
their addresses and decline. The case order became load-bearing in a way
it was not before; see the probe pass below.

## The residue is three kinds and it is irreducible

Pointer, channel, `unsafe.Pointer`. fmt orders those by an address, so
its own order changes between runs -- and a decline's usual escape, the
rendered text, is no escape here, because the text of a pointer IS that
address. They range in the map's own order.

**Sorting them by address was considered and rejected.** It would have
made "a range and a print agree" total, and given repeat-range stability
a pointer-keyed map does not have. It loses on the reachable case: a
`map[error]V` keyed by `errors.New` values currently declines to the
MESSAGE order, deterministic across runs, and an address order would
replace something useful with something arbitrary. The project's stated
rule -- "the fallback's job is not parity but DETERMINISM" -- already
settles it.

## The decline is found by the sort, one level lower than before

`sortMapKeys` argues that a pre-pass over key TYPES would give up on maps
the comparator can actually order. The same argument now applies inside a
composite: `rv` stops at the first element that differs, so a
`time.Time`-keyed map orders on `wall`/`ext` and declines only on two
times that differ ONLY by location. A static "does this type contain a
pointer anywhere" check was drafted first and thrown away for exactly
this reason.

## What it costs: nothing measurable, and a priced new path

`field` was ALREADY past the inline budget -- cost 591 against a budget
of 80 -- so growing it to 773 changes nothing for the accepted path.
Confirmed with `-gcflags='-m -m'` on both sides rather than assumed.

`BenchmarkSortMapKeys` before/after in a `git worktree` of HEAD: int and
string rows unmoved. The machine is noisy enough that the `copyonly`
control row -- which the change cannot touch -- swings +-14% at 300x and
still +-3% at 3000x, so anything smaller than that is not a reading.

New rows price what the newly-ordered kinds cost, ns per key, minimum of
ten runs, Apple M3:

```
keys                4     16     64    256   1024
 int              6.4   11.7   20.4   31.0   37.3
 string           6.9   12.0   25.4   36.0   49.0
 complex          8.8   15.4   25.4   34.3   53.1
 [4]int          24.9   46.9   77.0  120.4  150.3
 struct{int;str} 27.7   54.1   89.7  125.1  155.2
 any(Duration)   29.2   55.1   90.0  128.6  160.2
```

Complex sits with the scalars because it IS one -- two floats, no
recursion, no decline. The three below are 3-4x an int: a Kind switch and
an `Index`/`Field` call per element per comparison. Two allocations PER
MAP (the escaping `keyCmp` and the method value), which is what
`orderInterfaceKeys` has always cost; the scalar rows still allocate
nothing.

## Probe pass: 21 probes, 3 uncaught, and all three were real gaps

Not one of the three was a false negative. Each named a test that was
weaker than it looked.

1. **`rv`: interface nil sorts high** -- the pool had no nil INSIDE a
   composite. A bare interface key goes through `field`, which answers
   for nil itself, so `rv`'s interface case is only reachable one level
   down. Added `[2]any{nil, 1}`.
2. **`rv`: uint compared as int** -- the pool had `uint(0)`, `uint(1)`
   and `uint64(1<<63)`, and a pool entry only ever meets values of its
   OWN type, so the high bit and the small values never met. Added
   `^uint(0)` and a `uint64(1)` companion.
3. **`field`: the `*StructVal` case falls through to `rv`** -- nothing
   anywhere pinned the ORDER of a key with a nested struct field.
   Pre-existing, but this change made the case order load-bearing.

The third became `TestANestedStructKeyFieldOrdersFieldWise`, whose keys
are `In{N: 10}`, `In{N: 2}`, `In{N: 1}` -- chosen because that is where
field order and text order DISAGREE (`'0' < '}'` puts 10 first as text,
comparing N puts 2 first), so a decline is caught rather than passing
under an order that happens to match.

Re-probed at `-count=3`: all three caught on the first try.

## An existing test's premise went stale, and that is the right signal

`TestAStructKeyedMapDeclinesWhereFmtUsesAnAddress` had a subtest "a field
type the comparator does not know" holding a `uint`. `rv` orders a uint
now, so the subtest failed -- correctly. The `uint` moved to
`TestAFieldOfANamedTypeOrdersByItsKind`, and the decline is now asserted
where it is still true: a pointer field and a channel field.

## New tests

- `TestWhichKeyKindsGetAnOrder` -- the table, written so the NOs are
  visible. Each case's keys are given in DESCENDING fmt order; an ordered
  kind must reverse them, an unordered kind must hand back the same slice
  UNTOUCHED, checked by pointer identity rather than by "some order",
  which a sort that silently ran would also satisfy.
- `TestKeyCmpMirrorsFmtOverEveryComparableKind` -- every PAIR in a pool
  of ~50 values goes into a two-entry map, and fmt's printing of that map
  says which one fmtsort put first. Pairs rather than whole maps is what
  makes it a test of the comparator instead of a second test of the sort.
  Both arms -- same-type and cross-type -- are counted and required.
- `TestAFieldOfANamedTypeOrdersByItsKind` -- the reachable defect, at the
  `map[any]` key and at the struct-key field.
- `TestATimeKeyedMapOrdersOnTheFieldsBeforeItsPointer` -- both halves of
  the stops-at-the-first-difference claim, using `time.FixedZone` so it
  does not need a zoneinfo database.
- `TestACompositeKeyedMapDeclinesToTextOrder`,
  `TestANestedStructKeyFieldOrdersFieldWise`,
  `TestAScriptRangesAMapKeyedByNamedTypes` (end to end).
- `BenchmarkSortMapKeys` gained complex / array / nativestruct /
  anynamedint rows.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. Three runs of each shape through `cmd/grsh`: range matches
print, identical across runs, and `map[error]V` still orders by message.

`docs/LANGUAGE.md` claimed only that STRING-keyed maps range in sorted
order -- stale since the previous session, which ordered every scalar
key. It now states the rule and names the three exceptions.

## Method notes

- **"No script can spell it" is not "no script can reach it."** The
  reason four sessions left these keys alone was a claim about the type
  grammar. The door was `map[any]V`, which boxes anything a registry call
  returns, and nobody had walked through it.
- **A comment can be the bug report.** `orderScalarKeys` already said
  "fmtsort switches on reflect.Kind... Kind is exactly the question
  here". The function one level down asked for the type. Reading the two
  side by side is what found it.
- **A probe that reads as uncaught is worth trusting twice.** All three
  uncaught probes here were test gaps, not no-op mutations -- and one of
  them (the nested struct field) was a gap that PREDATED this session and
  only became dangerous because of it. That is a sixth entry in the
  running list of probe-pass outcomes, and the second where the fault was
  the test.
- **Price against a worktree, not a stash.** Stashing the source left the
  new tests referencing symbols that no longer existed; `git worktree add
  HEAD` gives a buildable baseline in one command.
- **Read the control row before believing the measurement.** `copyonly`
  cannot be affected by anything here and moved 14%. Every delta smaller
  than the control's own swing was noise.

## Open

- **Pointer, channel and `unsafe.Pointer` keys stay unordered.** Now the
  WHOLE residue, and irreducible: fmt answers by an address and the text
  of a pointer is that address. Deliberate, documented at `scalarKeyCmp`.
- **A composite key declines to the text order**, which is deterministic
  but is not fmt's. Same as the interface case, same reason.
- **Two allocations per map** on the declinable paths -- the escaping
  `keyCmp` and its method value. Per map, not per key; not pursued.
- **`P{X: 1}` stores an int in a `float64` field.** Unchanged.
- **A one-field struct key past ~256 entries orders 18% slower** than the
  text order it replaced. Unchanged.
- **A scalar-keyed map field still RENDERS through fmt.** Unchanged.
- **Two allocations an entry remain** in `appendStructKeyedMap`.
  Unchanged.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time. Unchanged.
- **Ties in the text-order fallback are still resolved arbitrarily.**
  Unchanged.
- **`growForRest`'s estimate is untested**, deliberately.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is
  a narrow call** -- 13% margin at eight fields.
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

~~**Complex, array and native-struct keys stay unordered.**~~ Closed.
They order in fmt's order now, and the reason they were skipped -- that
no script can spell one -- was never the reason that mattered.
