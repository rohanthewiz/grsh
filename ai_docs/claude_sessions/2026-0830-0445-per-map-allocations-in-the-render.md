# Session: Per-Map Allocations in the Render

Session: 7b97c2f0-eb5b-4cbe-b751-440f2ae1d927
Date: 2026-08-30

Takes an Open item carried since the session that built the fast path:

> **Two allocations an entry remain** in `appendStructKeyedMap`.

Closed. It is ONE per map now -- seven allocations render three entries
and seven render two hundred and fifty-six -- and the render got 15-27%
faster on the way.

## The item was stale by one, and a profile named the one that was left

Measured before anything was written: 9 allocations at 3 entries, 24 at
16, 74 at 64, 269 at 256. That is 1.016 an entry, not two, plus a fixed
part -- so the doc's number had already been halved by work that did not
notice it.

An `alloc_objects` profile put 91% of them in ONE place:

```
546145 91.04%  reflect.unsafe_New
     0   ...   reflect.packEface
     0   ...   reflect.Value.Interface (inline)
     0   ...   github.com/rohanthewiz/grsh/internal/interp.decodeMintedKey
```

## The cause was a premise the function states about itself

`decodeMintedKey`'s doc comment already said it:

> THE CARRIER IS LIFTED OUT WHOLE ... That is a trade with a condition
> attached, and the condition is ADDRESSABILITY. ... Every key on this
> path comes from MapKeys, which is exactly the non-addressable case, so
> the lift is free here ... A future caller handing this an addressable
> key would still get the right answer, just a second allocation for it.

That future caller existed. `sortMapKeys` passes keys from `MapKeys` and
`iter.Key()` -- non-addressable, and `Interface()` hands back a view of
the words already there. `appendStructKeyedMap` calls `SetIterKey` into a
scratch value, and a scratch **is** addressable, so boxing a three-word
`ScriptKey` out of it had to copy it to the heap first.

The move that took reflect's per-entry allocation off the key -- using a
scratch instead of `MapKeys` -- handed one straight back through the door
it had just closed.

## Two changes

- **`keyScratch`** (store.go) -- `intoKeyStore` read backwards. It returns
  a settable `reflect.Value` of the minted key type that ALIASES a plain
  `ScriptKey` the caller owns, so `SetIterKey` writes through the Value
  and the loop reads `sk.K` as an ordinary Go struct: no reflect, no box,
  no allocation.
- **`growForRest` on the value slab** -- the estimate `sortMapKeys`'
  fallback already makes over its key slab, applied to the values here for
  the same reason: one map has one VALUE type, so the first entry's width
  predicts the rest. It flattens the log-n doubling steps that were the
  whole remaining slope.

### Why the alias is sound

Exactly `intoKeyStore`'s reason, and it rests on the SAME guard rather
than a second assumption: a minted key type is one embedded `ScriptKey`
at offset zero and nothing else, so the two are the same three words under
different names, and `mintKeyType` panics at declaration if a future mint
stops being layout-identical. The write barrier is emitted the usual way
-- `SetIterKey` does a typed copy into memory reflect knows the type of,
and what changes is only which type NAME it is read back under.

The `svSlots` comment's tally of aliasing unsafe expressions went from
five to six.

## The obvious factoring was 2-8% slower and was rejected

The clean split -- lift the shared tail into
`decodeKey(StructKey, *keyArena)` and let `decodeMintedKey` become a
one-line wrapper -- reads better and costs a second call frame on
`decodeMintedKey`, which is the RANGE path and the hotter of the two.
Neither function is inlinable (`decodeKey` costs 130 against a budget of
80), so the frame is real.

`BenchmarkMapKeyArena`, two binaries built from one tree and run
alternately, min of 5 rounds x 6 counts at 3000x:

```
                        split   flat
 arena/f1/k64           820.3  809.8   (baseline 799.7)
 arena/f1/k256         3056    2875    (baseline 2890)
 arena/f10/k256        6930    6646    (baseline 6676)
 perkey/f1/k64         1466    1398    (baseline 1369)
 perkey/f10/k64        2622    2528    (baseline 2566)
```

The split regressed EVERY row by 2-8%; the flat version sits inside
+-2% of baseline with mixed signs. So the three lines are repeated in the
render loop instead, with the measurement written next to them. What
could actually drift between the two is not a nil check but the decoding,
and that is `fillKeyArr` -- which both call, and which `decodeKeyArr`'s
comment already names as the single decoder for exactly this reason.
`StructKey.structVal` repeats the same nil check today for the same
reason.

## What it costs

ns per entry, `BenchmarkStructKeyedMap/fast/structkey`, Apple M3, minimum
of twenty-four runs at 500x:

```
entries          3     16     64    256
 boxed        147.2  134.6  140.3  174.5   allocs 9, 24, 74, 269
 per map      114.2   97.7  111.1  148.9   allocs 7,  7,  7,   7
```

15-27% off the render. The gap is widest in the middle: the small map's
fixed costs dilute it, and the large one spends more of its time in the
sort.

Allocations are flat to 256 exactly and become 13 at 1024 -- the arena
cutting three more chunks, which is `keyChunkVals` bounding what one
retained key can hold alive. A different promise, and not this one.

## `growForRest` needed more samples than it first got

A single `-count=8` run said it cost 9% at three entries and bought 2% at
256 -- which would have argued for dropping it. Min of 24 across three
rounds said 1.3% at three (noise) and 3-5% BETTER at 16 and 64. The first
reading was thermal drift, and the numbers it produced were close enough
to the pre-change baseline to look like a real regression.

## New test

`TestRenderingAStructKeyedMapAllocatesPerMapNotPerEntry`. The existing
`TestRenderingAStructKeyedMapAllocatesLessThanFmt` passed the whole time
this bug was live and would have kept passing: it asserts `fast < slow/2`,
and fmt costs six allocations an entry, so one an entry clears that bar
comfortably at every size. **A per-entry cost is invisible to a comparison
against something worse. Only the slope catches it.**

So the new test counts 16 entries and 256 and requires the difference to
be at most 2 -- a slope, not a pinned number, for the reason
`checkMapAllocs` already gives. Two is tight enough to pin the value slab
as well: dropping `growForRest` shows 5.

## Probe pass: 5 probes, 4 caught, 1 uncaught and correct

```
keyScratch -> reflect.New          TestRenderingAStructKeyedMapAllocates-
                                   PerMapNotPerEntry (+ ...LessThanFmt/n3)
growForRest removed                TestRenderingAStructKeyedMapAllocates-
                                   PerMapNotPerEntry
nil-key check dropped              TestAStructKeyedMapMatchesFmt (panic)
keyScratch aliases fresh memory    TestAStructKeyedMapMatchesFmt + 3
growForRest estimate halved        UNCAUGHT -- and correct
```

The uncaught one is the standing "`growForRest`'s estimate is untested,
deliberately" item: a short guess falls back on append's own doubling and
changes no answer, only a cost.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. A map with a nil key, a negative field, and two keys tying in
the first field run through `cmd/grsh` three times: byte-identical output,
range order matching print, values paired with their keys, nil key first.
`docs/LANGUAGE.md` needed nothing -- no behaviour moved.

## Method notes

- **Re-measure the item before working on it.** The Open item said two
  allocations an entry; it was one, and the fixed part had grown. Working
  from the doc's number would have sent the search to the wrong place.
- **A function's own doc comment can be the bug report.** `decodeMintedKey`
  described the addressability condition, named the caller class that
  breaks it, and priced the breakage -- three sessions before a caller
  that breaks it was written. The comment was right and nobody re-read it
  when the second caller arrived.
- **A test that asserts "better than X" cannot see a slope.** The existing
  allocation test compared against fmt and passed at 269 allocations. The
  replacement compares the path against ITSELF at two sizes.
- **Measure the clean factoring, not just the dirty one.** The shared
  `decodeKey` was written first because it was obviously right, and it
  cost 2-8% on the hotter of the two callers. Both were built from one
  tree and run alternately; a sequential before/after would have buried it
  in drift.
- **Min-of-8 is not enough for a 3% question.** The first `growForRest`
  reading argued for removing it, on numbers that turned out to be
  thermal. Three rounds of eight reversed the sign.

## Open

- **A string first field still trails the abandoned text order** by 23% at
  a thousand keys, on locality. Not fixable without giving up fmt's order.
- **The projection reads FIELD 0 ONLY.** A key that ties there for a field
  or two before differing gets a p-compare it cannot use.
- **Few-but-not-one distinct values in field 0** get partial benefit and
  pay the full array. There is no cheap way to count distinct values.
- **The render's fixed seven allocations** are the arena's two slabs, the
  entry slice, the value slab, the two scratches and the projection's
  token array. Nothing in it is obviously removable, and none of it grows.
- **A map of 1024 keys allocates thirteen**, six of them the arena's three
  extra chunks. That is `keyChunkVals` doing its job.
- **Pointer, channel and `unsafe.Pointer` keys stay unordered.**
  Irreducible: fmt answers by an address and the text of a pointer is that
  address.
- **A composite key declines to the text order**, deterministic but not
  fmt's. Same for the interface case.
- **`P{X: 1}` stores an int in a `float64` field.** Unchanged.
- **A scalar-keyed map field still RENDERS through fmt.** Unchanged.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time.
- **Ties in the text-order fallback are still resolved arbitrarily.**
- **`growForRest`'s estimate is untested**, deliberately -- and it now has
  a second caller resting on the same "a short guess is only a cost"
  argument.
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

~~**Two allocations an entry remain** in `appendStructKeyedMap`.~~ Closed.
It was one, not two, and it is now none: the cost is per map. The fix was
not a cheaper allocation but noticing that a scratch value is addressable.
