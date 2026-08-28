# Session: Aliasing the Key Array on Decode

Session: 96c5feba-703e-419e-a182-54cdf0411bc1
Date: 2026-08-28

Continues `2026-0828-1734-fusing-the-structval-block.md`, taking its next
open item:

> **`decodeMintedKey`'s remaining cost is per-field work**, ~4.6ns a
> field through `fromKeyValue` and `arr.Index(i).Interface()`. There is no
> allocation left to remove; a cheaper decode means a cheaper per-field
> read.

The allocation pass had already been taken. What was left was one
`reflect.Value` construction and one call per field, and both went away
together.

## The array was already a []Value

An `[N]any` is N interface words laid end to end, and `Value` is `any`.
So the key's field array IS a `[]Value` the moment its address is known
-- the only thing standing between the two was that the address lived
inside an interface.

`boxKeyArr` already took an interface apart in the other direction for
the encode path. `unboxKeyArr` is its inverse and returns both words:

```go
func unboxKeyArr(a any) (typ, data unsafe.Pointer) {
	e := *(*eface)(unsafe.Pointer(&a))
	return e.typ, e.data
}
```

The read is safer than the write it mirrors: it creates no new reference,
so there is no write barrier to skip. What it claims is only the layout,
and `mintKeyArrZero` now probes that claim in both directions at
declaration.

## The measurement, and the alternative that lost

Three implementations in one binary -- the reflect loop, the alias, and a
type-switch enumeration on `[1]any`..`[8]any` that copies the array to the
stack -- minimum of five runs at a fixed iteration count, Apple M3, one
allocation on every path throughout:

| fields | 1 | 2 | 3 | 6 | 8 | 10 | 16 |
|---|---|---|---|---|---|---|---|
| reflect | 28.3 | 33.0 | 40.1 | 52.5 | 66.8 | 73.8 | 111.4 ns |
| alias | 25.3 | 26.5 | 30.1 | 33.6 | 38.9 | 42.4 | 51.5 ns |
| switch | 25.9 | 27.4 | ~30 | 33.9 | 39.9 | 76.6 | 114.7 ns |

The slope goes from ~5.5ns a field to ~1.75ns, so the gap widens with
arity: 11% at one field, 36% at six, 54% at sixteen.

The type switch ties the alias inside its enumeration -- the array copy
is free at those sizes, so the reflect calls were the whole cost -- and
falls off a cliff past it. It would also have added a THIRD fanout
constant beside `keyArrFanout` and `valBlockFanout`. The alias needs no
constant and has no second path, because it does not care what N is. That
decided it before the numbers at 10 and 16 did.

## The type word is the bounds check

An alias cannot bounds-check, so what makes reading `len(t.Fields)` words
sound is knowing the box holds this struct's `[len(Fields)]any`. An array
type's LENGTH is part of its identity, so one pointer compare against the
type word already cached in `keyArrZero` settles length and element type
together:

```go
typ, data := unboxKeyArr(a)
if keyTyp, _ := unboxKeyArr(t.keyArrZero); typ != keyTyp {
	return sv
}
```

That is STRICTER than the `Index()` bounds check it replaces, which never
looked at the element type at all. Anything failing it decodes to all-nil
fields rather than panicking -- the reachable case is the zero StructKey,
whose nil `F` has a nil type word, and all-nil fields is exactly what the
old loop gave for an array of nil interfaces.

## One decoder, one `any`

`decodeKeyArr` now takes a bare `any` instead of a `reflect.Value`, which
is what lets both entry points share it. `StructKey.structVal` passes
`k.F` directly. `decodeMintedKey` passes `sk.Field(1).Interface()` --
which is not the boxing case: `Interface()` on an interface-kind Value
hands back the eface already in the field rather than building a new one,
so it is a two-word load and the array is never copied.

## What a script feels, stated plainly

Two builds run alternately (order flipped each round, because running the
old build first every time put a uniform +1% on everything), twelve
container shapes:

| shape | time |
|---|---|
| `range-map-key-struct-10` | **-2.3%** |
| `range-map-key-struct` | **-1.3%** |
| ten others | inside a ±1% haze |

Only the two shapes that DECODE moved. The haze includes `map-hit-native`
at +1.6%, a shape that touches no struct at all, which is what says it is
haze. Both real numbers are about what the microbench predicts: 3ns of a
505ns one-field iteration, 31ns of a 1243ns ten-field one.

A decode is simply not where an interpreted loop spends its time. This
makes it cheaper without making it matter more, and the honest version of
that is worth more than a geomean that hides which two shapes moved.

## A new bench shape

`range-map-key-struct-10` is added as the decode counterpart to
`map-key-struct-hit-10`: the only shape in the file whose cost is
dominated by reading a key back rather than building one. Decoding is
per-field work and the existing one-field range shape cannot show that,
so a future change to the per-field cost now reads as a slope across two
shapes instead of one number.

## The guard pass

Four mutations, all killed:

- **The type-word guard removed** -- dies in
  `TestDecodeGuardsOnTheKeyArrayType`, on the nil-`F` case, with
  `unsafe.Slice: ptr is nil and len is not zero`.
- **`typ` and `data` swapped in `unboxKeyArr`** -- dies at DECLARATION in
  `mintKeyArrZero`'s new probe, with the message naming unboxKeyArr, so
  every test that declares a struct reports it.
- **One word short** (`len(sv.Vals)-1`) -- dies in the guard test and in
  `TestDecodedKeyDoesNotAliasTheMapKey`.
- **`fromKeyValue` skipped** -- dies in
  `TestStructMapKeysNestedFieldComesBack`; a nested struct field comes
  back as a StructKey.

New tests:

- `TestDecodeGuardsOnTheKeyArrayType`. Its wrong-length case is
  deliberately a LONGER array, so both answers are defined: with the
  guard the fields are nil, without it they are 1, 2, 3. A shorter array
  would distinguish the two only by reading past the end, which is the
  thing being prevented and not a thing to demonstrate. It also asserts
  the positive case first, or every other assertion would pass for a
  guard that rejects everything.
- `TestDecodedKeyDoesNotAliasTheMapKey`. The hazard the alias introduces:
  the decode now reads the words of the key sitting INSIDE the map, so a
  version that passed them on by reference would hand a script a window
  into the map's own memory. Asserted against a key from `MapKeys`, not
  from `intoKeyStore`.

`TestStructMapKeysAtEveryArity` already covered field ORDER at 0..9 and
12 through map printing, so the alias reading the right words in the
right order needed nothing new.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. No behavior changed except that a malformed key now decodes to
nil fields instead of panicking, which nothing pinned and nothing can
reach. Measurement worktree removed.

`unsafe` in `internal/interp` goes from three expressions to five: the
reinterpretation in `unboxKeyArr` and the `unsafe.Slice` in
`decodeKeyArr`. Both are probed at declaration by `mintKeyArrZero`.

## Method notes

- **The named item was the right size this time.** Last session's was
  scoped too narrowly and the same shape turned up in two hotter
  functions. This one grepped the same way and found nothing else:
  `arr.Index(i).Interface()` per field appeared only here.
- **Build the losing candidate too.** The type switch was the obvious
  design -- it matches both existing fanouts -- and measuring it is what
  showed the alias needs no constant at all. Without it the enumeration
  would have gone in on precedent.
- **Alternate the run order.** Running old-then-new six times put a
  uniform +1% on every shape including ones with no struct in them.
  Flipping the order each round cancelled it. Six unflipped rounds would
  have been reported as a regression that is not there.
- **A guard test needs its positive case.** A guard mutated to reject
  everything passes every all-nil assertion. The genuine-key decode is
  asserted first and fatally.
- **Probe the new claim where the old one is probed.** `mintKeyArrZero`
  already checked the box direction at declaration; adding the unbox
  direction there means one failing check wherever the layout claim stops
  holding, on a path that is cold and runs for every declared struct.

## Open

- **A decode is now ~25ns of floor plus ~1.75ns a field.** The floor is
  `newStructVal`'s allocation and the two reflect hops into the minted
  key; the per-field part is a load, a type assertion in `fromKeyValue`,
  and a store. Neither has an obvious next move.
- **`valBlockFanout` at 16 is cheap to raise** -- ~122 bytes of binary
  per case, no per-call tax -- if a script ever wants wider structs.
- **`unsafe` in `internal/interp` is five expressions**: the `NewAt`
  alias in `intoKeyStore`, `unsafe.Slice` plus the eface reinterpretation
  in the general encode path, and the same pair on the decode side. Each
  has a runtime check or a test stating its invariant.
- **`keyArrFanout` at 8 is a narrow call.** The margin is 13% at eight
  fields. If the general encode path gets cheaper again, the top cases
  are the first to drop.
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

~~**`decodeMintedKey`'s remaining cost is per-field work.**~~ Closed
here: the per-field cost is down from ~5.5ns to ~1.75ns, and the reflect
call it went through is gone.
