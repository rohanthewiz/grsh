# Session: Projecting a Struct Key's First Field

Session: ea210662-152a-4dd6-8650-b9ef84df26c0
Date: 2026-08-29

Takes an Open item that has ridden along unchanged for four sessions:

> **A one-field struct key past ~256 entries orders 18% slower** than the
> text order it replaced.

Closed. It is 41% FASTER than that text order now, and 55% faster than
what was in the tree this morning.

## The loss was in the comparison, and a profile said so

`fieldOrder.Less` unpacked field 0 out of its `any` on EVERY comparison:
a `*StructVal` deref, a slice-header load, an interface unpack, and
`keyCmp.field`'s type switch. A CPU profile of `struct/f1/k1024` put the
chain `Less -> keys -> field -> cmp.Compare` at 0.96s of the 1.34s the
whole sort spent -- 72%, with `sort.pdqsort` itself at 1.21s cum. The
decode the same function does was 0.1s.

The text order it lost to compared `bytes.Compare` over views into ONE
contiguous slab. Cheaper per comparison, which is the whole reason the
two rows crossed.

## Four candidate shapes, measured before any were written for keeps

Each did decode plus sort over the same keys, ns/key, min of eight:

```
                        f1/k4  k16   k64  k256  k1024
 A  what was there       37.0  41.0  53.1  73.8 100.2
 B  index permutation    30.9  42.5  64.0  95.2 124.0
 C  projection + permute 31.9  30.2  32.2  36.5  42.2
 D  projection in place  38.5  31.2  32.4  36.0  43.1
```

**B lost, which was the surprise.** Sorting an `[]int32` permutation with
`slices.SortFunc` removes the interface dispatch on Less and Swap and the
three-slice write-barriered swap -- and it is WORSE than A past 16 keys,
because `decoded[ord[a]]` turns every comparison into a random access.
The dispatch it saved was cheaper than the locality it gave up.

C and D tied. D was taken: it keeps `sort.Sort` and `keyOrder`, and adds
one array.

## What was built

- **`projectedOrder[T]`** -- `fieldOrder` with field 0 lifted out of its
  interface into a `[]T` of the field's OWN type. `Less` answers from two
  loads when the tokens differ and falls through to `keyCmp.keys` when
  they tie; `Swap` moves p with everything else `keyOrder` keeps in step.
- **`projectedOrderOf`** -- the dispatcher, and the place the two
  conditions live.
- **`projectedBy[T]`** -- one generic scan, five instantiations
  (`int`, `int64`, `rune`, `byte`, `string`).
- **`projectMinKeys = 8`**, measured.
- `BenchmarkSortMapKeys` gained `s1` (string first field), `tie10` (ten
  fields, the first CONSTANT) and a `k8` column on the struct rows.

### Why it is a speed choice and nothing else

The projection is built only when every key shares one `*StructType` AND
one dynamic type in field 0. Under those two conditions `keyCmp.keys` is
forced through its field loop into `keyCmp.field`, which takes the same
case on both sides and compares the same two values p holds. So the two
Less functions are the same function POINTWISE, one sort algorithm driven
by them makes the same decisions, and the permutation is identical --
under a decline as well.

**The decline still reaches the comparator.** Only a pair that field 0
settles skips `keys`, and such a pair cannot decline: under those two
conditions field 0 is a kind `keyCmp` answers outright. Every pair that
could declare a decline -- a later field of two dynamic types, a nested
pointer -- ties in p and goes the long way.

**A nil key refuses the projection.** It sorts before every other key,
which no value of the field's type can be made to mean.

**Floats and bools are left out on purpose.** NaN breaks the `!=` the fast
path IS -- two NaN tokens are unequal and neither is less, so Less would
answer both ways for one pair -- and a bool discriminates two ways, so
nearly every pair would tie and pay twice.

## The pessimal case was found by pricing it, not by reasoning about it

The `tie10` row exists because a projection has exactly one shape it can
lose on: a first field that separates NOTHING. Every comparison ties, pays
for the array, and does the general comparison anyway. Measured, with no
guard: **27% slower at eight keys, 18% at sixteen, 13% at sixty-four.**
That is a reachable shape -- `map[P]V` keyed by `P{Kind, Name}` with one
Kind in it.

So `projectedBy` refuses an all-equal field, seen with one comparison per
key inside a loop already touching every key. The row now tracks the
general path within the noise. Fewer ties than that are left alone: two
distinct values over a thousand keys still ties about half its
comparisons, and half of a large win is a win.

## What it costs

ns/key, Apple M3, min of eight runs. "text" is the pre-`keyCmp` order,
RE-MEASURED rather than quoted; "fields" is `keyCmp` with
`projectMinKeys` raised out of reach; "projected" is what runs now.

```
keys              4      8     16     64    256   1024
 1 int field
  text         62.0   60.6   59.0   63.7   71.2   87.8
  fields       45.0   38.8   39.3   61.7   82.6  113.5
  projected    39.4   38.5   38.4   36.9   40.8   51.4
 10 int fields
  text        212.9  195.5  195.0  200.1  213.2  227.4
  fields       72.8   65.6   69.8   76.3  102.2  138.0
  projected    62.5   53.7   52.0   55.5   57.5   74.0
 1 string field
  text         61.4   57.6   55.1   55.6   64.9   82.9
  fields       40.8   46.3   54.4   70.9   92.4  133.7
  projected    39.6   46.0   46.7   60.4   76.9  107.5
 10 int fields, the first CONSTANT
  text        210.9  192.3  192.1  194.5  205.9  236.5
  fields       64.0   63.5   77.0   97.5  132.3  188.7
  projected    69.1   60.3   81.2   97.7  128.9  171.6
```

The `nativestruct` control row, which none of the three paths can touch,
moved 5% between runs; nothing smaller than that is a reading.

One allocation per map is added: the token array.

## A string first field is the one place the text order is still ahead

By 23% at a thousand keys. The projection still takes 20% off what that
shape cost before it, but the render wins on LOCALITY -- its `[]byte`
views all point into one slab, in order, where a `[]string` holds a header
per key and the sort chases a separate body per comparison.

It is not an option regardless: the text order is not fmt's, so it is
reachable only as a decline's fallback. Written into the doc rather than
left for the next reader.

## The threshold cannot be read off the committed benchmark

At four keys BOTH of its arms take the same path -- `projectMinKeys` is
what decides which -- so the constant's own table comes from a throwaway
harness that forced the projection at every count. Eight is the first
count that is not a loss, and it is the same eight for an int field and a
string one; four costs 9-10%, two costs 15-22%. What that buys is the two-
to four-key map a shell actually ranges, left where it was already
fastest. Same reason `newKeyArena` is not built below three keys.

## New tests

- `TestAProjectedOrderIsTheGeneralOrder` -- the one the change rests on.
  Both sorters run over the same randomised map and must produce the same
  permutation slot by slot and the same decline verdict. Field 0 draws
  from a SMALL set so ties are common; field 1 is mixed-type so the
  decline is reachable; one bucket is deliberately mixed in field 0 and
  must be refused. Four arms counted -- projected, refused, declined, and
  a repeated first field -- and any empty one fails the test.
- `TestAProjectedTieOrdersOnTheFieldsBehindIt` -- field 0 takes TWO values
  rather than one, because a constant field is now refused and would send
  the map down the general path, testing it twice. Asserts
  `projectedOrderOf != nil` out loud first.
- `TestAConstantFirstFieldRefusesTheProjection` -- the refusal, and that
  it costs no order.
- `TestAMixedFirstFieldRefusesTheProjectionAndDeclines` -- the failure
  mode a projection introduces: an int and a string in one field, which
  fmt orders by a type's address, must still decline to the text order.
- `TestTheProjectionThresholdChangesNoOrder` -- crossing
  `projectMinKeys` may move a cost, never an answer. Keys start at 8 so
  the field-wise and text orders disagree.

## Probe pass: 7 probes, 6 caught, 1 uncaught and correct

```
Less: no fall-through on a tie      TestAProjectedOrderIsTheGeneralOrder,
                                    TestAProjectedTieOrdersOnTheFields...
Swap: p left behind                 TestAMapRangesInFmtsOrder + 2
projectedBy: no *StructType check   TestADeclinedMapRangesInRenderedText...
projectedBy: type mismatch ignored  TestAMixedFirstFieldRefuses... + 1
projectedBy: no nil-key check       TestAProjectedOrderIsTheGeneralOrder
projectedBy: all-equal check gone   TestAConstantFirstFieldRefuses...
projectedBy: same read off p[i]     TestAProjectedOrderIsTheGeneralOrder,
                                    TestAProjectedTieOrdersOnTheFields...
threshold removed                   UNCAUGHT -- and correct
```

The uncaught one is a deliberate no-op: removing the threshold changes
speed and not order, which is exactly what
`TestTheProjectionThresholdChangesNoOrder` asserts. Two probes were caught
by tests that PREDATE this session, which is the useful signal in the
table.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. Three runs of an int-first map through `cmd/grsh`: range
matches print, ties order on the field behind them, values stay paired
with their keys, identical every run. A string-first map checked the same
way. `docs/LANGUAGE.md` needed nothing -- no behaviour moved.

## Method notes

- **Measure the candidates before writing one for keeps.** Four shapes in
  one throwaway `_test.go` cost twenty minutes and killed the index-
  permutation idea, which read as the obvious optimisation and was slower
  than what it replaced.
- **Price the shape the change can only lose on.** The `tie10` row was
  added to MEASURE a cost that was going to be hand-waved, and it turned
  out to be 27%, which is the difference between shipping a guard and
  shipping a regression.
- **A benchmark cannot always price its own constant.** At four keys both
  arms of `BenchmarkSortMapKeys` take the same path, so a "before/after"
  run there reports pure noise -- 13% of it, in the direction that would
  have argued for no threshold at all. The threshold needed a harness that
  forced both sides.
- **Re-measure the number you are closing.** The 18% came from a session
  two ago on unknown thermal conditions. Rebuilding the text order in a
  throwaway bench put it at 87.8 against the projected 51.4 -- the item
  closed by a wider margin than the old table implied.
- **A refused fast path has to be tested for the refusal, not only for
  the answer.** Three of the five new tests assert that something did NOT
  project. A projection taken where fmt answers by an address would give a
  confident order for a map that has none, and no parity test would catch
  it reliably -- two addresses usually happen to be ordered the way the
  values are.

## Open

- **A string first field still trails the abandoned text order** by 23% at
  a thousand keys, on locality. Not fixable without giving up fmt's order.
- **The projection reads FIELD 0 ONLY.** A key that ties there for a
  field or two before differing gets a p-compare it cannot use. Projecting
  further fields costs another array and another scan to shorten a tail
  that is already rare; not pursued.
- **Few-but-not-one distinct values in field 0** get partial benefit and
  pay the full array. There is no cheap way to count distinct values.
- **One allocation per map** added, the token array; two more on the
  declinable paths remain from before.
- **Pointer, channel and `unsafe.Pointer` keys stay unordered.**
  Irreducible: fmt answers by an address and the text of a pointer is that
  address.
- **A composite key declines to the text order**, deterministic but not
  fmt's. Same for the interface case.
- **`P{X: 1}` stores an int in a `float64` field.** Unchanged.
- **A scalar-keyed map field still RENDERS through fmt.** Unchanged.
- **Two allocations an entry remain** in `appendStructKeyedMap`.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time.
- **Ties in the text-order fallback are still resolved arbitrarily.**
- **`growForRest`'s estimate is untested**, deliberately.
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

~~**A one-field struct key past ~256 entries orders 18% slower** than the
text order it replaced.~~ Closed. It is 41% faster than that text order
now. The fix was not a faster comparator but a cheaper thing to compare.
