# Session: Ranging a Map in fmt's Order

Session: 058ee31e-c404-4d2c-be73-0fc59747792b
Date: 2026-08-29

Continues `2026-0829-1210-a-struct-keyed-map-orders-itself.md`, taking the
first item it left open:

> **`for k := range m` and `fmt.Println(m)` order a struct-keyed map
> differently** -- rendered text against field-wise. Both deterministic,
> neither wrong, and they disagree. Making them agree means pointing
> `sortMapKeys` at `keyCmp`, which now exists; it is a language decision
> and a behaviour change to ranging, so it is not taken here.

Taken here. `sortMapKeys` orders by `keyCmp` now, so a range and a print
visit the same entries in the same order.

## The change is smaller than the thing it removes

`sortMapKeys` had two jobs stacked on each other: RENDER every key into
one slab, then sort the views into that slab. The render existed only to
produce something comparable. `keyCmp` compares the DECODED fields --
which the function already builds, because they are what the range
variable has to be -- so the whole render, its slab, its bounds array and
its slice of views all leave the hot path.

What is left is a decode pass and a sort, and the sort is over two
parallel slices instead of three.

## Ordering by fields is not uniformly faster, and the doc says so

BenchmarkSortMapKeys, ns per key, minimum of six runs, Apple M3:

```
keys              4      16      64     256    1024
 struct, 1 field
  text        104.8    84.2    70.0    71.9    80.2
  fields       45.8    48.3    54.9    74.6    95.0
  allocs      8->6    9->6    9->6   10->6   15->12
 struct, 10 fields
  text        181.7   166.0   180.3   194.3   208.5
  fields       50.6    49.5    66.7    85.6   122.2
  allocs     12->6   12->6   12->6  18->12  36->30
```

**A render is paid n times; a comparison is paid n log n times.** So the
two rows CROSS. At one int field and a thousand keys this is 18% SLOWER
than the text order it replaces, because one int compare is about as cheap
as `bytes.Compare` on the text and there is no longer a render to save.
At ten fields the crossover is past every count measured, because a
comparison still stops at the first field while a render has ten to write.

The four-key map a shell actually ranges is 2.3x faster at one field and
3.6x at ten. The trade is taken and the loss is written into the doc
rather than left for the next reader to rediscover -- along with the fact
that the benchmark's keys differ in their FIRST field, which is the
comparator's best case and the render's worst.

## The decline needs a different answer here than it does in a print

`appendStructKeyedMap` declines by handing the map back to fmt, which is
correct by definition. A range has nowhere to hand it: fmt's order in
those cases IS a machine address and changes between runs.

So a declined map falls back to the rendered-text order -- the order this
function used before. **The fallback's job is not parity, it is
determinism**, which is the property `sortMapKeys` exists for and the only
one still available once fmt's answer is unreproducible.

## The argument that lets the sort find the decline

The decline is discovered mid-sort, not by a pass over the keys first, and
that is a choice with a reason on both sides.

A pre-pass would have to declare a field position declined the moment two
keys disagree about its type -- including when no comparison ever reaches
that field. `P{A: 1, B: 1}` and `P{A: 2, B: "x"}` are settled by `A`, by
fmt as well as here, so a pre-pass would give up a map the two agree on.

What makes the lazy version safe is that **a pair the sort skips cannot be
a pair the answer depends on.** A correct comparison sort must compare
every pair that ends up ADJACENT in its output; a skipped pair `x,y`
therefore has some `z` with `x < z < y`, and those two comparisons did not
decline, so fmt orders them the same way and puts `x` before `y` for the
same reason this does. The pairs whose order rests on an address are
exactly the adjacent ones, and those are exactly the ones compared.

## Two tests, from two directions

**`TestARangeVisitsAStructKeyedMapInFmtsOrder`** is the randomised parity
test, over the same generated maps the renderer's parity test uses. It is
not a copy of that test: that one holds `appendStructKeyedMap` against
fmt, this one holds `sortMapKeys` -- a different function, a different
sort, reaching `keyCmp` from the other side -- against the same fixed
point, by rebuilding fmt's map text out of the order the RANGE would
visit. It counts both arms and fails if either is empty, so a change that
made everything decline cannot leave it asserting nothing.

**`TestAMapRangesInFmtsOrder`** drives a script that ranges a 40-key map
AND prints it, then requires the two sequences to match. The printed side
is `fmt.Println(m)` on a bare map, which never reaches grsh's renderer --
Go's fmt orders and prints it itself -- so that half of the comparison is
the standard library's own answer rather than a copy of the implementation
under test. It replaces `TestAMapRangesInRenderedTextOrder`, which pinned
the old order and was the only test in the package that failed.

## A two-key test is not a test of a sort

`TestADeclinedMapRangesInRenderedTextOrder` was written with two keys --
the smallest map that can hold two `*StructType`s -- and probe 3 (drop the
`ord` swap from `textOrder.Swap`) went UNCAUGHT. A sort of two elements
makes one comparison and stops, so it cannot tell an ordering that keeps
its slices in step from one that does not.

Rewritten with forty keys alternating between the two declarations, it
catches the probe, and it also now checks that each key still carries its
own value after the fallback sort.

## The probe pass

Eleven probes, ten caught by a named test:

| probe | caught by |
|---|---|
| `fieldOrder.Less` reversed | the two order tests |
| decline fallback removed | the decline test |
| `textOrder.Swap` drops `ord` | the decline test (after it grew) |
| `keyOrder.Swap` drops `decoded` | four tests, incl. the pairing test |
| nil key sorts last | three parity tests |
| one-key shortcut returns nil | four tests |
| no arena | the two allocation tests |
| fallback sorts by fields again | the decline test |
| no field sort at all | four tests |
| struct-type decline removed | the decline test + the declines test |

The one uncaught probe is `growForRest`, which is an ESTIMATE and is
recorded as deliberately untested in the previous session.

The harness carried forward last session's fix -- it fails loudly on a
match count it did not expect -- and that mattered: two probes needed
their patterns rewritten because the code around them had moved.

## The control that had been wrong twice

`TestRangingAMapDecodesItsKeysIntoOneArena` asserts a GAP against a
control that mirrors `sortMapKeys` with per-key decodes. The control has
now been stale twice: once when the render became a slab, and once here
when the render left entirely. Both times the gap silently grew from "the
arena" to "the arena plus something else" -- a floor assertion that keeps
passing while measuring something it does not name. The comment now says
so in the plural, and the control gained a check that its own keys do not
decline.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End to end through `cmd/grsh`: a 12-entry map with a nil key, a
nested-struct-keyed map, a string-keyed map, a 300-key map, a one-key map
and an empty one -- every print and every range in the same order, and the
nil key first in both.

## Method notes

- **A behaviour change is cheapest when its comparator already exists and
  is already tested.** `keyCmp` shipped last session with a randomised
  parity test behind it; pointing a second caller at it cost a day's
  reading and no new correctness surface.
- **Report the crossover, not just the win.** The one-field/1024-key row
  is slower and is in the doc table with the reason. A table that only
  showed the counts where the change wins would be the same kind of lie as
  a control that stopped mirroring its implementation.
- **A test whose n is below the algorithm's smallest interesting size is
  not testing the algorithm.** Two keys make one comparison; the probe
  that walked through it was the only thing that said so.
- **Laziness can be the correct semantics, given an argument.** Declining
  during the sort rather than before it is more permissive AND provably
  no less correct -- but only because of the adjacency argument, which is
  now written down where the choice is made.

## Open

- **An int-keyed map still ranges in the map's own randomised order**, and
  fmt sorts it numerically -- so it is now the only map shape whose range
  and print disagree, and the only one whose range is not deterministic at
  all. Found by this session, pre-existing, and NOT fixed: `keyCmp` is
  about decoded structs and cannot answer for a scalar key, so closing it
  means a comparator per scalar kind. A separate change; a note sits at
  the branch in `sortMapKeys` that reaches it.
- **A one-field struct key past ~256 entries orders 18% slower than it
  did.** Priced, documented at the call site, and accepted for the sizes a
  shell ranges.
- **A scalar-keyed map field still renders through fmt.** Unchanged;
  measured twice, both shapes lose.
- **Two allocations an entry remain** in `appendStructKeyedMap`, one per
  key and one for anything the value renderer does not fast-path.
  Unchanged.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time. Unchanged.
- **Ties in the text-order fallback are still resolved arbitrarily** --
  two distinct keys with identical text keep whatever order the declined
  sort left them in. Unchanged, and it was arbitrary before too.
- **`growForRest`'s estimate is untested**, deliberately. It now runs only
  on the fallback path.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is a
  narrow call** -- 13% margin at eight fields.
- **A re-declared `P` does not find its own earlier keys.** Unchanged --
  and it is what the decline test builds its map out of.
- **`[]map[P]int{{{1}: 2}}` cannot elide the key literal.** Unchanged.
- **`%T` on a container prints storage.** Unchanged.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** --
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

~~**`for k := range m` and `fmt.Println(m)` order a struct-keyed map
differently.**~~ Closed. They now agree for every struct-keyed map whose
order fmt itself can reproduce, and a map fmt orders by address falls back
to the text order, which is deterministic where fmt is not.
