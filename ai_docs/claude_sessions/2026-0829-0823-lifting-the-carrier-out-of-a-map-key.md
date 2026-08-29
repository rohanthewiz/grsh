# Session: Lifting the Carrier Out of a Map Key

Session: 13948399-c32a-4f0d-bc1c-36a436b2bac8
Date: 2026-08-29

Continues `2026-0828-1754-aliasing-the-key-array-on-decode.md`, taking its
next open item:

> **A decode is now ~25ns of floor plus ~1.75ns a field.** The floor is
> `newStructVal`'s allocation and the two reflect hops into the minted
> key; the per-field part is a load, a type assertion in `fromKeyValue`,
> and a store. Neither has an obvious next move.

The item said there was no obvious move. Splitting the floor into its
pieces found one, in the half of it nobody had priced.

## Price the floor before believing it has no move

Four measurements in one binary -- the allocation alone, the allocation
plus the field fill, the reflect walk down to the key's two fields with
the decode taken out, and the whole thing:

| fields | alloc only | +unbox/guard/fill | reflect hops | full |
|---|---|---|---|---|
| 1 | 14.0 | 15.6 | **11.1** | 25.7 |
| 3 | 16.2 | 20.1 | **11.1** | 30.5 |
| 6 | 25.4 | 24.0 | **11.1** | 34.6 |
| 10 | 31.9 | 33.6 | **11.2** | 45.0 |

The hops cost ~11ns and are FLAT: 43% of a one-field decode and 25% of a
ten-field one, and they allocate nothing, which is why the allocation
pass never touched them. Splitting a "floor" into named parts is what
turned one unpromising number into two, one of which had an obvious move.

## The walk was guarding against a caller that does not exist

`decodeMintedKey` read the key field by field on purpose, and the reason
was written down:

> A three-word struct does not fit in an interface word, so boxing one
> out of an ADDRESSABLE field copies it to the heap first.

True, and beside the point. Boxing out of a NON-addressable value copies
nothing -- there is nothing to protect the interface from, so reflect
hands back a pointer to the words already there -- and the only caller,
`sortMapKeys`, gets its keys from `MapKeys`, which is exactly that case.
The walk was paying ~6ns of every decode to be safe against a caller that
has never existed.

So the carrier comes out whole:

```go
sk := rv.Field(0).Interface().(ScriptKey).K
```

## The measurement, and the candidate that lost

Three implementations in one binary, minimum of ten runs at a fixed
iteration count, Apple M3, one allocation on every path throughout:

| fields | 1 | 3 | 6 | 10 |
|---|---|---|---|---|
| field walk (was) | 26.2 | 31.5 | 36.0 | 45.6 ns |
| lifted (taken) | 19.9 | 24.7 | 29.7 | 39.4 ns |
| eface alias | 18.4 | 23.8 | 27.8 | 38.0 ns |

The saving is 6.2-6.8ns at every arity, which is what says it is floor:
24% off a one-field decode, 14% off a ten-field one, the gap NARROWING
with arity because the per-field work it does not touch grows around it.
That is the opposite shape from last session's change, and the two are
complementary -- one took the slope, this takes the intercept.

The third form reads the eface's data word straight into a `*StructKey`.
It is sound for the same reason `intoKeyStore`'s alias is: `mintKeyType`
has already checked that a minted key type is one `ScriptKey` at offset
zero, and a `ScriptKey` is one `StructKey` at offset zero. It won by
~1.4ns and was not taken. That is a SIXTH `unsafe` expression bought with
a third of a percent of one interpreted range iteration -- a price paid
in the thing this package counts, for a saving no script can feel.

A fourth, two `Field` calls then one assertion on `StructKey` itself,
came in at 21.6/26.4/30.9/40.9 and is dominated by the lift.

## The assertion is a check the walk did not have

`rv.Field(0).Field(0)` then `Field(0)` and `Field(1)` read `StructKey`'s
fields BY INDEX, so their ORDER was load-bearing, and the only thing
holding it was a test asserting the names of fields 0 and 1.
`.(ScriptKey)` names the type instead. A mint that stopped being one
`ScriptKey` now panics at the decode rather than reading the wrong slots,
and `mintKeyType` still catches it earlier, at declaration.

That test's stated reason is gone, so its check went with it and the
invariant it protected is now stated where a reader meets the layout
check that does still bind -- in the same block, pointing at the
assertion.

## What a script feels, stated plainly

Two builds run alternately, order flipped each round, six rounds:

| shape | time |
|---|---|
| `range-map-key-struct` | **-1.29%** |
| `range-map-key-struct-10` | **-0.96%** |
| `map-key-struct-hit-10` | +0.64% |
| `map-hit-native` | +1.55% |

Only the two shapes that DECODE moved down, and the one-field number is
what the microbench predicts almost exactly: 6.3ns of a 499ns iteration
is 1.26%.

But the haze here is WIDER THAN THE SIGNAL. `map-hit-native` touches no
struct at all and drifted +1.55%, more than either real number moved. So
this table is consistent with the change and cannot on its own establish
it; the microbench is the evidence, and this is the sanity check that
nothing else moved. Reporting it the other way round would be claiming a
resolution the harness does not have.

## The guard pass

- **The nil-`T` guard removed** -- dies in
  `TestMintedValuesDoNotEscapeToScripts/range_map_keys,_nil_key` with a
  nil dereference, because `newStructVal` reads `len(t.Fields)`.
- **The premise of the new alloc test probed** rather than assumed: the
  same decode fed an ADDRESSABLE key (from `intoKeyStore`) costs 2
  allocations against the MapKeys path's 1. Without that check a test
  asserting `== 1` might have been asserting something unfalsifiable.

New test:

- `TestDecodingAMapKeyDoesNotCopyIt`. The saving now lives in a property
  of the CALLER -- that its keys are non-addressable -- and losing it
  would change no answer, only add an allocation per ranged key, which no
  test that reads the decoded struct back could see. It asserts the
  decoded value too, because a decode that did nothing would also
  allocate nothing.

`BenchmarkKeyCrossing`'s doc already warned that a decode benchmarked on
a key from `intoKeyStore` reports an allocation the real path never pays.
That warning is now stronger than a measurement note -- the decode TRADES
on the difference -- and says so.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. No behavior changed. `unsafe` in `internal/interp` stays at
five expressions. Measurement worktree removed.

## Method notes

- **A floor with no obvious move is a floor that has not been split.**
  The item was written as one number, ~25ns. Two of its parts had been
  worked over in previous sessions and one -- the reflect hops -- had
  never been priced on its own. Four sub-benchmarks found the 11ns.
- **Re-read the comment that justifies the slow path.** The walk's
  reasoning was correct about reflect and wrong about this program: it
  guarded the addressable case, and the caller has never produced one.
  The fact that overturned it was already written three functions away,
  in `BenchmarkKeyCrossing`'s doc.
- **Count the currency the package counts.** The eface alias won on time
  and lost on `unsafe` expressions, which is a number this package tracks
  session to session precisely so it can be spent deliberately. 1.4ns
  that no script can feel is not a reason to spend it.
- **Probe a bound before asserting it.** `AllocsPerRun == 1` is worth
  nothing if 1 is what every version gives. Feeding the same function an
  addressable key and getting 2 is what makes the assertion a guard.
- **`zsh` does not word-split.** `for b in $order` with `order="old new"`
  runs ONCE with `b` set to the whole string, so an alternating two-build
  loop silently benchmarked the new build twice and the old build never.
  It was caught by an empty results file, not by a wrong number, which is
  luck.

## Open

- **A decode is now ~20ns of floor plus ~1.9ns a field.** What is left of
  the floor is `newStructVal`'s allocation (~14ns at one field) and one
  `Field` plus one `Interface` (~5ns). The allocation is the fresh
  `*StructVal` that makes a range variable safe to mutate, so removing it
  means proving the script cannot keep the previous one -- a much larger
  change than anything here.
- **`valBlockFanout` at 16 is cheap to raise** -- ~122 bytes of binary
  per case, no per-call tax -- if a script ever wants wider structs.
- **`unsafe` in `internal/interp` is five expressions**: the `NewAt`
  alias in `intoKeyStore`, `unsafe.Slice` plus the eface reinterpretation
  in the general encode path, and the same pair on the decode side. Each
  has a runtime check or a test stating its invariant. A sixth was
  measured and declined this session.
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

~~**A decode is ~25ns of floor plus ~1.75ns a field, and the floor has no
obvious next move.**~~ Closed here: the floor is down to ~20ns, and the
half of it that was never priced -- the reflect walk into the key -- is
gone.
