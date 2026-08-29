# Session: Ordering a Map Costs More Than Decoding It

Session: cb1e9a45-28f6-43bd-909b-97ef8354cdda
Date: 2026-08-29

Continues `2026-0829-0901-slabbing-a-maps-decoded-keys.md`, taking two of
its open items at once:

> **`sortMapKeys` renders and insertion-sorts every key**, which is O(n^2)
> string compares plus an allocation-heavy `String` per key. Past a few
> dozen keys that, not the decode, is what a range over a struct-keyed map
> costs. Untouched here and the obvious next thing to price.

> **The arena's retention is uncapped.** Chunking was measured at 2-8% and
> declined. If a script ever holds one key of a large map past its loop,
> the chunk goes in.

## The first measurement ended the previous three sessions' story

`BenchmarkSortMapKeys` prices the whole ordering pass -- decode, render,
sort -- and its first run said this, ns per key:

```
              4     16     64    256   1024
1-field    187.5  177.8  196.2  322.7 1005.0    19..4099 allocs
10-field   806.0  749.9  780.6  981.3 1834.0    67..16387 allocs
```

The last three sessions took a decode from ~30ns to ~20ns to ~11ns. The
decode was 11ns of a 187ns floor. **The render was the other 176.**
`String` built a `strings.Builder` and ran an `fmt.Fprintf` PER FIELD --
four allocations a key at one field, sixteen at ten -- to produce text
that exists only to be compared and thrown away.

Sitting under that was the quadratic: flat-ish ns/key to 64 keys, then
323 and 1005. Two costs of different shapes stacked, which is why neither
was visible in `BenchmarkKeyCrossing` or `BenchmarkMapKeyArena` -- both
price one key or one decode, and this is neither.

## What shipped

**`(*StructVal).appendTo(b []byte) []byte`**, with `String` rewritten as
`string(sv.appendTo(nil))`. `appendValue` fast-paths the types a script
can hold and falls through to `fmt` for anything else, so an unknown Value
kind is slow, never wrong.

**One slab for a map's rendered keys.** `sortMapKeys` appends every key's
text end to end into one buffer, records bounds, and cuts `[][]byte` views
once the buffer has stopped growing -- views taken during the loop would
point into a reallocated array. `growForRest` sizes the slab from the
first key, which is a good estimator when all n keys are one struct type.

**`sort.Sort` over a `keyOrder`** holding the three slices that must move
together: the text, the map's own keys, the decoded structs.

**A one-key shortcut.** Everything in the function exists to establish an
order, so `n == 1` decodes and returns without rendering at all.

**The string branch pays for nothing it does not need.** A string key IS
its own sort text, so that branch sorts `keys` in place and allocates
zero.

```
keys              4      16      64     256    1024
 struct, 1 field
  before      187.5   177.8   196.2   322.7  1005.0
  after        87.2    79.2    62.1    66.3    82.4
  allocs      19->8   67->9  259->9 1027->10 4099->16
 struct, 10 fields
  before      806.0   749.9   780.6   981.3  1834.0
  after       186.2   161.9   166.7   190.4   211.1
 string
  before       14.4    16.9    54.4   204.6   789.1
  after         9.5    15.7    26.0    36.9    51.6
  allocs       1->0    1->0    1->0     1->0     1->0
```

53-92% off the struct rows, 34-93% off the string rows, and the
allocation count stops tracking n.

## The string branch was measured three ways in one binary

Writing it took three attempts, and the third is the one that ships. All
three in one binary, minimum of eight runs, ns per key:

| shape | 4 | 16 | 64 | 256 | 1024 | allocs |
|---|---|---|---|---|---|---|
| insertion sort on a `[]string` | 10.3 | 22.3 | 58.2 | 179.5 | 779.9 | 1 |
| `sort.Sort` on a `[]string` | 16.3 | 16.1 | 27.1 | 34.4 | 46.7 | 2 |
| **sort the keys directly** | **6.4** | **12.9** | **25.9** | 34.8 | 51.5 | **0** |

The `[]string` wins at a thousand keys because it calls `String` once per
key where sorting in place calls it n log n times. It also allocates the
slice and the sorter, which escapes into `sort.Sort`'s interface -- and
those two allocations ARE what a four-key map costs. A shell ranges small
string maps constantly and thousand-key ones almost never, so the
allocation-free path that leads to 256 keys and trails by 10% at 1024 beat
two paths and a threshold between them.

Routing strings through the struct branch's byte slab was the second
attempt and the worst of both: 5 allocations for a four-key map.

## The retention cap, and why it is measured in slots

The previous session declined chunking at 2-8%. Taken here, with the unit
changed.

**A key count is the wrong cap.** A chunk of 64 keys is 3KB for a
one-field struct and 12KB for a ten-field one, so any key count that
bounds the wide struct over-charges the narrow one. `keyChunkFor` divides
a slot budget instead.

**And each key is charged for its header, not just its fields.** A
`StructVal` is 32 bytes where a `Value` is 16, so a one-field key is two
parts header to one part fields -- capping on fields alone would let a
narrow struct retain three times what a wide one does. Dividing by
`nf + svSlots` holds both slabs together at 1024 slots -- **16KB at every
arity** -- and it removes both edge cases a field-only divisor needed: the
divide-by-zero on `type P struct{}`, which is a legal key type, and the
quotient rounding to zero for a struct wider than the cap.

`svSlots` is `unsafe.Sizeof(StructVal{}) / unsafe.Sizeof(Value(nil))`.
`Sizeof` is a compile-time constant, not a pointer reinterpretation: the
package's count of five aliasing `unsafe` expressions is unchanged, and
writing `2` would have been the same arithmetic with the reason left out.

256 ten-field keys, minimum of twelve runs, against no cap at all:

| slots/chunk | keys/chunk | chunks | ns/key | vs uncapped |
|---|---|---|---|---|
| 128 | 10 | 26 | 30.51 | +23.7% |
| 256 | 21 | 13 | 27.15 | +10.1% |
| 512 | 42 | 7 | 25.64 | +4.0% |
| **1024** | **85** | **4** | **25.26** | **+2.4%** |
| 2048 | 170 | 2 | 25.16 | +2.0% |
| uncapped | 256 | 1 | 24.66 | 0.0% |

The refill is fixed per chunk, so halving the chunk doubles how often it
is paid -- and the bottom of the table does not reach zero. Two chunks
still cost 2%, which is the branch every carve runs to notice an empty
slab. That part is paid whatever the cap is, and it is why raising the cap
further buys almost nothing. 1024 is the knee.

`newKeyArena` still inlines and its header still does not escape, which
the two-allocation test rests on. That is why the first chunk is cut in
`newKeyArena`'s own body rather than by calling `refill` -- `refill`'s two
`make`s would push it past the inline budget and put the header on the
heap, turning every arena from two allocations into three.

## A test that had quietly stopped testing its claim

`TestRangingAMapDecodesItsKeysIntoOneArena` asserts the GAP between
`sortMapKeys` and a control doing the same work with per-key decodes, so
that everything the two share cancels and the gap is the arena alone. Its
control built a `[]string` with a `String()` per key.

The moment the render became a slab, the control stopped mirroring it. The
gap grew from "the arena" to "the arena plus the render" -- and because
the assertion is a floor, the test **passed the whole time while measuring
something else**. It now renders exactly as `sortMapKeys` does, sorts with
the same `keyOrder`, and permutes `keys` in place rather than copying it,
since a copy would put an allocation in the control that the thing under
test does not have.

## New tests

- `TestAppendValueMatchesFmt`. Every case of the fast-path switch plus a
  fallthrough, held against `fmt` itself rather than a hand-written
  string, both bare and inside a field. A case that drifts from `%v` does
  not fail loudly: it changes what a script prints AND silently reorders
  every range over a struct-keyed map.
- `TestAppendToExtendsItsBuffer`. A render that ignored the buffer handed
  to it would pass every test that only calls `String`.
- `TestOrderingAMapAllocatesPerMapNotPerKey`. Asserts the SHAPE -- that
  doubling the keys does not double the allocations -- not a count, for
  the same reason the arena threshold is not pinned.
- `TestOrderedKeysStayPairedWithTheirValues`. A `Swap` that moved two of
  the three slices still produces sorted output, and pairs every key with
  another key's value. Driven from a script, because that is where it
  shows.
- `TestAMapRangesInRenderedTextOrder`, `TestAStringKeyedMapRangesInOrder`.
  Forty keys, past every threshold in the function.
- `TestKeyChunkForCoversEveryArity`. Both ends of the division, plus the
  bound itself: every arity's chunk holds within one key of 1024 slots.
- `TestABigMapsKeysComeFromSeveralChunks`, and
  `TestKeysFromDifferentChunksDoNotShareFields` -- the hazard a refill
  adds that no small map can reach.

## The probe pass, and a probe harness that lied

Thirteen probes, each reverting one piece; every one fails a named test.
Notably: rendering per key fails the per-map allocation test; either half
of a partial `Swap` fails the pairing test; removing chunking fails the
chunk test; `%f` for `%g` fails the fmt test; dividing by `nf + 1` instead
of `nf + svSlots` fails the uniform-bound test.

The first run of the harness reported `NOTHING FAILED` for the probe that
removes the string sort. It had left `strings` unused, so the package did
not build, and the harness's grep for `FAIL:` found nothing in a build
error. **A probe that does not compile looks exactly like a probe nothing
catches.** The harness now runs `go vet` first and says `PROBE DID NOT
BUILD`. Re-run with that check, all thirteen probes bite.

## A wrong expectation, not a wrong result

`TestAMapRangesInRenderedTextOrder` failed first time: the map ranged
`0 10 11 ... 19 1 20 ...` where the test wanted `0 1 10 ...`. The test was
wrong. Ordering is on the WHOLE rendered text, closing brace included, so
`P{N: 10}` sorts before `P{N: 1}` because `'0' < '}'`. That is exactly
what the old insertion sort did on the same strings, so the rewrite
preserved it. The expectation now renders each key the way the interpreter
does and sorts that, with a comment saying why sorting the bare numbers is
the wrong answer.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End-to-end through `cmd/grsh`: a five-key struct-keyed map
ranges in order and survives a mutated range variable; a one-key map and a
string-keyed map take their own branches; a 600-key map crosses three
chunk boundaries, sums correctly, and a key held past the loop mutates
without touching the map. `unsafe` in `internal/interp` stays at five
aliasing expressions.

## Method notes

- **The same lesson as last session, one level up.** Last session's note
  was "a cost with no move may have the wrong UNIT". This one found the
  unit was still wrong -- the decode was 11ns of a 187ns pass, and three
  sessions of decode work had been optimising 6% of the thing a range
  actually pays for. Benchmarking the caller's unit is not a one-time fix;
  it is a question to re-ask after every win.
- **Two costs of different shapes hide each other.** A per-key render and
  a quadratic sort look like one flat curve up to 64 keys. Neither was
  visible until the key count reached 1024, and neither could have been
  seen at all by a benchmark that prices one key.
- **When a test's control mirrors an implementation, the mirror is part of
  the implementation.** Changing the render should have failed the gap
  test; instead the gap silently changed meaning. A floor assertion is
  robust to noise and blind to exactly this.
- **A probe harness needs its own probe.** `NOTHING FAILED` and `DID NOT
  BUILD` are indistinguishable if you only grep for failures.
- **Cap the quantity you actually mean.** Chunking by key count bounds
  keys; the thing at risk is bytes. Dividing a slot budget -- and charging
  each key for its header as well as its fields -- made the bound uniform
  across arities and deleted two edge cases on the way.
- **Ask what the alternative costs before keeping it.** The first string
  branch reused the struct branch's slab for symmetry and made the common
  case 5x the allocations. The measurement that caught it took two
  minutes; the symmetry would have cost every string-keyed range forever.

## Open

- **A range over a struct-keyed map now costs ~62-87ns a key at one field
  and ~166-211 at ten**, with allocations per MAP. What is left is the
  render itself (`appendValue` per field, `strconv` and appends) and the
  n log n compares. Both are linear-ish and neither has an obvious next
  move.
- **`fmt.Appendf` is still the fallback for any field type not in
  `appendValue`'s switch.** A script holding, say, a `[]string` field in a
  map key pays fmt per key. Adding cases is cheap if one ever does.
- **Ties in the sort are still resolved arbitrarily.** Two distinct keys
  that render to the same text -- an `int` 1 and a `string` "1" in the
  same field -- come out in the map's own randomised order. That was true
  of the stable insertion sort too, since its input order was already
  random; making it deterministic means tie-breaking on the encoded key
  bytes, which nothing has asked for.
- **`growForRest`'s estimate is untested**, deliberately: it changes only
  how many times a slab grows, and a test on that would measure the
  allocator.
- **The arena's retention is capped at 16KB.** Closed here.
- **`valBlockFanout` at 16 is cheap to raise** -- ~122 bytes of binary per
  case, no per-call tax -- if a script ever wants wider structs.
- **`unsafe` in `internal/interp` is five aliasing expressions**, plus
  `svSlots`, which is `Sizeof` arithmetic and aliases nothing.
- **`keyArrFanout` at 8 is a narrow call.** The margin is 13% at eight
  fields.
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

~~**`sortMapKeys` renders and insertion-sorts every key.**~~ Closed: the
render is one slab a map, the sort is n log n, and a one-key map renders
nothing at all.

~~**The arena's retention is uncapped.**~~ Closed: 1024 slots a chunk,
16KB at every arity, for 2.4% at 256 ten-field keys.
