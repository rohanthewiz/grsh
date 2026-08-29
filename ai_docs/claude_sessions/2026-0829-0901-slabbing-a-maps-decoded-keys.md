# Session: Slabbing a Map's Decoded Keys

Session: b46f8274-4937-4c24-a352-129f57c3536d
Date: 2026-08-29

Continues `2026-0829-0823-lifting-the-carrier-out-of-a-map-key.md`, taking
its next open item:

> **A decode is now ~20ns of floor plus ~1.9ns a field.** What is left of
> the floor is `newStructVal`'s allocation (~14ns at one field) and one
> `Field` plus one `Interface` (~5ns). The allocation is the fresh
> `*StructVal` that makes a range variable safe to mutate, so removing it
> means proving the script cannot keep the previous one -- a much larger
> change than anything here.

The item was right that the allocation cannot be REMOVED. It was wrong
that removing it was the only move, because it framed the cost per key.

## The unit was wrong, not the number

`sortMapKeys` is `decodeMintedKey`'s only caller, and it decodes EVERY
key of a map before the caller's loop body runs once -- it has to, since
ordering the keys means rendering each of them first. So all n
`*StructVal`s are alive simultaneously however they were allocated.

That is what makes a slab legitimate rather than clever: it changes where
the memory comes from, not how long it lives. The freshness the item was
protecting -- a range variable the script can mutate without reaching the
key inside the map -- is a property of each STRUCT being distinct, not of
each struct having its own allocation.

```
per key   [StructVal|[N]Value] [StructVal|[N]Value] ...   n allocations
arena     [StructVal StructVal ...] [Value Value ...]     2 allocations
```

## The change

`keyArena`: two slabs carved for a whole map, threaded through
`decodeMintedKey` as a nilable parameter. `decodeKeyArr` split into
itself plus `fillKeyArr`, so the two ways of OBTAINING a StructVal --
one allocation each, or a carve -- share one decoder and cannot drift.

`(*keyArena).structVal` has a nil receiver case and falls back to
`newStructVal` on every way of running out: no arena, an exhausted slab,
or a type wanting more fields than the arena was sized for. The last
cannot happen while a map holds one key type, but two compares make the
carve TOTAL rather than a bounds panic resting on a premise held three
functions away. A nil key returns before touching the arena, which is why
an arena sized for the whole map can never be short.

## The measurement

`BenchmarkMapKeyArena`, minimum of twelve runs at a fixed iteration
count, Apple M3, ns PER KEY:

| keys | 1 | 2 | 3 | 4 | 16 | 64 |
|---|---|---|---|---|---|---|
| per key, 1 field | 30.1 | 26.3 | 27.1 | 23.1 | 20.4 | 20.4 |
| **arena** | 46.3 | 28.7 | **22.3** | **17.5** | **14.4** | **11.2** |
| per key, 10 fields | 31.9 | 33.0 | 34.4 | 41.4 | 38.6 | 38.2 |
| **arena** | 49.4 | 36.4 | **33.6** | **35.0** | **28.4** | **24.5** |

Allocations go from n to 2 and STAY at 2, which is the whole mechanism:
past a couple of keys a decode is paying the allocator, not the fields.
So the saving GROWS with the key count and SHRINKS with the field count
-- 45% of a one-field decode at sixteen keys, 26% of a ten-field one --
because what it removes is per-KEY floor sitting under per-FIELD work it
does not touch. That is a third shape again: the last two sessions took
the slope and the intercept of one decode; this takes the count.

Small maps lose. Two slabs are two allocations where the fused block is
one each, so a one-key map pays 16-18ns per key and a two-key map still
pays 2-3ns. `sortMapKeys` builds an arena only past two keys.

## A benchmark that could not see the change

`BenchmarkKeyCrossing` prices ONE decode, so a change that amortises
across a map reads there as no change at all. The in-tree benchmark had
to be the map, not the key, and adding it was what corrected the
threshold: a scratch harness had put the turn at two keys, and
`BenchmarkMapKeyArena` -- which is the one that ships and can be re-run
-- put it at three. Two harnesses disagreeing by one is the reason the
constant is set from the one in the tree.

## What a script feels: nothing this harness can resolve

Ten rounds, order flipped each round, each round's ratios normalised by
the median ratio of four shapes that decode no struct key:

| shape | normalised |
|---|---|
| `map-key-struct-hit-6` | -1.01% |
| `map-key-struct-hit` | -0.77% |
| `map-key-struct-write` | -0.77% |
| `map-hit-struct` | -0.69% |
| `range-map-key-struct` | -0.65% |
| `range-map-key-struct-10` | -0.47% |
| four control shapes | -0.09% .. +0.07% |

The normalisation works -- the controls collapse to zero -- and the
answer is still that this table shows NOTHING. The two shapes that
decode moved less than four that do not, and those four are encode and
lookup paths this change cannot reach. Coherent per-shape offsets like
that are code layout, which averaging does not remove. The microbench is
the evidence; this is only a check that nothing broke.

## The guard pass

Removing the arena from `sortMapKeys` -- its only user, the whole
change at the call site -- left the entire package GREEN. The arena is a
source of memory, not of answers, so nothing that reads a decoded key
can see whether one was built.

New tests:

- `TestRangingAMapDecodesItsKeysIntoOneArena`. Asserts the GAP between
  `sortMapKeys` and the same rendering done with per-key decodes, so
  everything the two share cancels. A floor rather than an equality
  because `newKeyArena`'s header is stack-allocated only while it
  inlines, and `-race` turns that off.
- `TestArenaKeysDoNotShareFields`. Writes a sentinel into every field of
  every key in turn and checks every other key still reads what it
  decoded to, restoring each field immediately so all of them stay ARENA
  slots for the whole sweep. Re-decoding the mutated key instead would
  swap it for an independently allocated one and quietly stop testing the
  slot its neighbours border.
- `TestOneArenaServesAWholeMapsKeys`. 2 allocations with an arena against
  8 without, both asserted, because "2" is worth nothing unless the
  alternative is n.
- A multi-key case in `TestStructMapKeysComeBack`: the existing script
  test ranged a ONE-key map, which takes the nil-arena path and could not
  see this at all.

Probes: not advancing the field slab, and advancing it by one instead of
`nf`, both fail the sharing test; forcing the carve to fall back fails
the allocation test; removing the arena from `sortMapKeys` and dropping
its length check both fail the call-site test.

## The threshold is not pinned, and that is a finding

Three ways of catching a dropped length check were built and all three
measure the compiler instead:

- against a control, `-race` adds one allocation to `sortMapKeys` that
  the control does not see -- exactly the size of the effect sought;
- against a constant, every count on this path shifts when `newKeyArena`
  stops inlining;
- against the STEP from one key to two, which would cancel both, except
  the step is not uniform: `String`'s own allocations vary with the text
  a key renders to (7, 13, 19, 24, 29, 34 for one to six keys, and a
  different sequence under `-race`).

Dropping the check costs 16-18ns per key on one-key maps and changes no
answer, so it is TUNING. It is recorded the way this package records
`valBlockFanout` and `keyArrFanout` -- as a measurement beside the
benchmark that set it.

## The trade taken deliberately

A key that outlives its loop now holds the whole slab, where a fused
block would have held only itself. Two things bound it: the slab is sized
by the map just ranged, and it is SMALLER than the `[]string` of rendered
keys `sortMapKeys` already builds and holds for the same n. Chunking to
cap it was built and measured -- 2-8% at every arity to guard a shape no
script has hit -- and declined, with the cap left as a note for when one
does.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End-to-end through `cmd/grsh`: a three-key struct-keyed map
ranged with the range variable mutated leaves the map untouched, sorted
and findable. No behavior changed. `unsafe` in `internal/interp` stays at
five expressions -- the arena adds none. Measurement worktree removed.

## Method notes

- **A cost with no move may have the wrong UNIT.** The item priced one
  decode and concluded the allocation was load-bearing. It is -- per key.
  The caller works a map at a time, and the same allocation amortised
  across n is a different question with a different answer.
- **Benchmark the unit the caller works in.** `BenchmarkKeyCrossing`
  could not have shown this at any effort, because it decodes one key.
  Adding the map-sized benchmark also MOVED the threshold, so the missing
  benchmark had been hiding a wrong constant, not only a saving.
- **Probe the call site, not just the function.** Every test passed with
  the change reverted where it was used. The gap-against-a-control test
  exists because that probe was run.
- **When a test would measure the compiler, say so instead of writing
  it.** Three attempts at the threshold guard all tracked inlining. The
  honest output is a comment naming all three failures, so the next
  person does not spend the same hour.
- **Normalising noise can validate itself and still prove nothing.** The
  control shapes collapsed to ~0%, which says the normalisation works,
  and shapes the change cannot touch still moved further than the ones it
  targets. A clean method on an unresolvable effect is still
  unresolvable.
- **`zsh` does not word-split -- again.** `for b in $order` silently ran
  zero benchmarks, caught by an empty file. A `cd` inside a compound
  command also persisted into the next command and built the same binary
  twice, caught by two files with identical sizes. Both were the same
  class of error the last session recorded.

## Open

- **A decode's per-key floor is now ~2 allocations a MAP.** What is left
  per key is one `Field`, one `Interface` and the type assertion (~5ns),
  plus ~1.9ns a field. The `newStructVal` allocation is not gone, it is
  amortised; a map of one or two keys still pays it in full, by choice.
- **`sortMapKeys` renders and insertion-sorts every key**, which is O(n^2)
  string compares plus an allocation-heavy `String` per key. Past a few
  dozen keys that, not the decode, is what a range over a struct-keyed
  map costs. Untouched here and the obvious next thing to price.
- **The arena's retention is uncapped.** Chunking was measured at 2-8%
  and declined. If a script ever holds one key of a large map past its
  loop, the chunk goes in.
- **`valBlockFanout` at 16 is cheap to raise** -- ~122 bytes of binary
  per case, no per-call tax -- if a script ever wants wider structs.
- **`unsafe` in `internal/interp` is five expressions**: the `NewAt`
  alias in `intoKeyStore`, `unsafe.Slice` plus the eface reinterpretation
  in the general encode path, and the same pair on the decode side. Each
  has a runtime check or a test stating its invariant. A sixth was
  measured and declined last session.
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

~~**A decode is ~20ns of floor plus ~1.9ns a field, and the allocation in
the floor cannot be removed without proving a script cannot keep the
previous StructVal.**~~ Closed here, by not removing it: the caller
decodes a map at a time, so n allocations became 2.
