# Session: A Struct-Keyed Map Orders Itself

Session: cda1e10a-040c-4f36-94c2-dd9cda68a2ce
Date: 2026-08-29

Continues `2026-0829-1130-closing-appendvalues-fmt-fallback.md`, taking
the item that session left after pricing and declining it:

> **`map[K]V` fields still render through fmt**, at 358ns and 7
> allocations for three entries. Closing it means reproducing
> `internal/fmtsort`'s key ordering exactly, including for a minted
> struct key. Priced and declined above, not forgotten.

The decline was right about scalar-keyed maps and wrong about struct-keyed
ones, and only measurement could tell which.

## Two implementations were written and thrown away first

Both aimed at `map[string]int`, which is what "358ns for three entries"
was measured on.

**A straight reflect walk** — `MapKeys`, sort, render — is 1.5x faster
than fmt and allocates **exactly as much**: 129 allocations at 64 entries,
both. `reflect.Value.MapKeys` mints a Value per key and `MapIndex` mints
one per lookup, and that pair is precisely what fmt pays for. Taking a
different route to the same two allocations is not a fast path.

**Changing the shape** — one `MapRange` pass with `SetIterKey` into a
preallocated slice — makes the count flat in n and costs about ten
allocations to set up. At three entries that is 505ns against fmt's 413.
This is last session's lesson repeating: the setup allocations ARE what a
small map costs, and a shell prints small maps.

There is no third route. So scalar-keyed maps stay on fmt, and the
benchmark keeps a `stringkey` row measuring the decline, which is worth
what it says: **identical to fmt, to within noise, at every size.**

## The cost was not in maps. It was in struct keys.

Per entry, ns, through fmt:

| entries | 3 | 16 | 64 |
|---|---|---|---|
| `map[string]int` | 150.4 | 135.4 | 171.7 |
| `map[P]int` | 301.7 | 609.1 | 931.2 |

A scalar-keyed map is flat. A struct-keyed one **climbs**, which means the
sort, not the render. fmt orders keys with `internal/fmtsort`, whose
comparator walks reflect four levels deep per comparison — minted type,
`ScriptKey`, `StructKey`, then an interface holding an `[N]any` — n log n
times. On top of that it renders each key through the minted type's
promoted `String`, which decodes the key into a fresh `StructVal` and
builds a string that is thrown away.

Every part of that is work grsh already does better. It has an arena that
decodes a whole map's keys into one slab, and once decoded the fields can
be compared directly instead of through reflect.

```
             fast      fmt    speedup   allocs
n=3         389.7     807.5     2.1x     9 <- 19
n=16       1975.0   10468.0     5.3x    24 <- 97
n=64       8794.0   60378.0     6.9x    74 <- 385
n=256     44967.0  319084.0     7.1x   269 <- 1537
```

ns per entry goes from 269 / 654 / 943 / 1246 — climbing — to 130 / 123 /
137 / 176, which is flat. The shape changed, not just the height.

## The rule the whole thing turns on: DECLINE, do not guess

`fmtsort` orders pointers, channels, and interfaces-of-different-types by
**machine address**. No reproduction can predict those. So the comparator
records a decline instead of inventing an answer, and a declined map is
handed back to fmt, which is correct by definition. Four ways to reach it,
each its own test:

- a key type that is not a minted struct,
- two keys carrying different `*StructType`s — reachable, because a
  re-declared `P` mints one storage type but keeps a `StructType` per
  declaration,
- one field holding two different dynamic types across two keys —
  `P{A: 1}` and `P{A: "1"}` are both legal keys of one map,
- a field type the comparator has no case for.

The tests assert **the decline**, not only the output. An implementation
that stopped declining would still produce fmt's answer most of the time,
because two addresses usually happen to be ordered the way the values are.

One address comparison IS predictable and is kept: a nil key encodes the
zero `StructKey`, whose `T` is the nil pointer, and no `*StructType` lives
at address 0. So `m[nil]` sorts first, under fmt and here.

## The test that makes copying an internal package defensible

`keyCmp` mirrors `internal/fmtsort.compare`, an unexported standard
library detail with no compatibility promise. That coupling is held by
`TestAStructKeyedMapMatchesFmt`, which renders **randomised** maps both
ways and requires the bytes to be equal — random field values, random
field TYPES, nil keys, nine value types, sizes either side of the arena
threshold, 1800 maps in all. A Go release that reorders map printing fails
loudly here instead of leaving grsh quietly disagreeing with fmt.

That test is the entire reason this was worth doing rather than declining
a second time.

## A second value renderer, and why it matches on TYPE

Half the remaining per-entry allocation was `fromStore(...).Interface()`
boxing a scalar. `appendMapValue` reads it straight off the
`reflect.Value` instead, which halves the count (138 to 74 at 64 entries).

It dispatches on `reflect.Type` **identity**, never on `Kind`, and that is
the whole correctness argument rather than a style preference. `%v` on a
NAMED integer type calls its `String` method — fmt prints `time.Duration`
as `3s` and `time.Month` as `March` — so a switch on `reflect.Int` would
render both as bare numbers. Type identity admits only the predeclared
types, which have no methods. `appendValue`'s own type switch is exact for
the same reason, which is why it never had this bug.

`float32` is deliberately absent: `%v` on one is `%g` at the precision
that round-trips a 32-BIT float, and `rv.Float()` widens to 64, so the
obvious case would print `0.1` as `0.10000000149011612`.

## The test that had been testing fmt

`TestAScriptPrintsAStructKeyedMapLikeFmt` printed a map from a script and
compared the output. It passed with the fast path REVERSED — probe 1 flips
the nil key to sort last and nothing failed.

`fmt.Println(m)` on a bare map is handed to Go's fmt, which renders it
itself and never asks grsh. **appendValue sees a map only as a struct
FIELD**, reached through `appendTo`. The test was measuring the standard
library.

That is not a narrowing of the change — the open item says "map FIELDS" —
but it is a correction to what a script can observe, and the test now
prints a struct that HOLDS the maps. It bites immediately.

## The probe pass, and the harness's third way of lying

Sixteen probes. Thirteen fail a named test: the nil key sorting last, each
of the three declines removed, `mapValRender` on Kind, `float32` routed
through the 64-bit renderer, bool inverted, no arena, the map case
unwired, value bounds recorded before the value is rendered, fields
compared last-to-first, a minted value left wrapped, and both separators.

**Three do not, and all three are honest:**

- an unstable sort changes nothing, because the only tie two DISTINCT keys
  can produce is two NaNs, whose order fmt does not fix either. The stable
  sort is a mirror of fmtsort, not a behaviour. The comment now says so.
- dropping `ents = ents[:i]` changes nothing, because reaching it needs a
  map mutated concurrently with its own printing. It is belt, and is
  labelled belt.
- rendering a minted value without unwrapping it produces the same text,
  because fmt finds the carrier's `String`. Only the allocation test
  catches it, which is why that test grew a `map[P]P` shape.

And the harness lied a third time. Probe 1's first version replaced the
nil case with a comparator that was INCONSISTENT — `a` nil returns 1 and
`b` nil also returns 1 — and the sort happened to leave the order intact,
so it read as an uncaught probe. Then probe 13's pattern matched twice and
the substitution silently did nothing, which reads identically. `sub` now
fails loudly on a match count it did not expect and prints `PROBE DID NOT
APPLY`. Three sessions, three ways for a probe harness to report a
false negative: it did not build, its output was binary, its edit never
landed.

## A finding this work turned up, unrelated to performance

**`fmt.Println(m)` and `for k := range m` already disagree** on the order
of a struct-keyed map, and did before this session:

```
map[P{A: 0}:0 P{A: 1}:1 P{A: 2}:2 ... P{A: 10}:10 P{A: 11}:11]   fmt
0 10 11 1 2 3 4 5 6 7 8 9                                        range
```

`sortMapKeys` orders by RENDERED TEXT, so `P{A: 10}` precedes `P{A: 1}`
because `'0' < '}'`. fmt orders field-wise, numerically. Both are
deterministic; they are simply different orders for the same map.

Nothing here changes that — matching fmt is what this session's principle
required, and `keyCmp` is now exactly the field-wise comparator that
`sortMapKeys` would need to adopt if the two are ever to agree. Left in
Open as a decision, not taken as a side effect.

## New tests

- `TestAStructKeyedMapMatchesFmt`, the randomised parity test above.
- `TestAStructKeyedMapDeclinesWhereFmtUsesAnAddress`, the four declines,
  each asserting the decline AND that the output still equals fmt's.
- `TestMapValueRenderMatchesAppendValue`, holding the reflect-side
  renderer against the interface-side one directly — a stronger
  equivalence than holding each against fmt separately — with
  `time.Duration`, `time.Month` and `float32` as the cases that would fail
  a Kind switch.
- `TestRenderingAStructKeyedMapAllocatesLessThanFmt`, over `map[P]int` and
  `map[P]P` at three sizes. Asserted as a COMPARISON with fmt, and as
  "under half", rather than as a pinned count, for the reason the arena
  tests give.
- `TestAScriptPrintsAStructKeyedMapFieldLikeFmt`, end to end through the
  interpreter, printing a struct that holds the maps.
- `BenchmarkStructKeyedMap`, with the `stringkey` row that measures the
  decline.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End-to-end through `cmd/grsh`: a struct holding six map fields
prints every one identically to what Go's fmt prints for the same map
directly — including a nil key, a nested struct key, struct values, a
scalar-keyed map that declines, an empty map and a nil map — and a
300-entry map crosses three arena chunks. `unsafe` in `structs.go` is
still five aliasing expressions; this change adds none.

## Method notes

- **A decline can be the deliverable.** Two implementations of a
  scalar-keyed map renderer were written, measured and deleted, and the
  benchmark now keeps a row proving the decline was right. That row is
  worth more than the code would have been: the next person to have the
  same idea can see it was tried.
- **When a cost climbs, it is the sort.** `map[string]int` is flat in
  ns/entry and `map[P]int` is not. That single comparison located the
  whole problem before any code was written, and it is the same shape of
  reading that found the quadratic in `sortMapKeys` two sessions ago.
- **Copying an internal package is defensible exactly as far as a test
  makes divergence loud.** Without the randomised parity test this change
  should not ship; with it, the coupling to `internal/fmtsort` is a
  maintenance signal rather than a silent bug waiting for a Go release.
- **Kind is not Type.** A `reflect.Kind` switch is the natural way to
  write a renderer and it is wrong for every named type with a `String`
  method. The bug was avoided only because `appendValue`'s existing type
  switch had to be matched exactly.
- **A test that passes with the implementation REVERSED is testing
  something else.** The script-level map test was measuring Go's fmt,
  because a bare map never reaches grsh's renderer. That is the same
  failure as last session's control that stopped mirroring its
  implementation, found this time by a probe rather than by reading.

## Open

- **`for k := range m` and `fmt.Println(m)` order a struct-keyed map
  differently** — rendered text against field-wise. Both deterministic,
  neither wrong, and they disagree. Making them agree means pointing
  `sortMapKeys` at `keyCmp`, which now exists; it is a language decision
  and a behaviour change to ranging, so it is not taken here.
- **A scalar-keyed map field still renders through fmt**, and should:
  measured twice, both shapes lose. Closed by decision rather than by
  code.
- **Two allocations an entry remain**, one per key and one for anything
  the value renderer does not fast-path. The key one is
  `decodeMintedKey`'s `Interface()` copying a three-word `ScriptKey` out
  of an ADDRESSABLE scratch Value — `sortMapKeys` avoids it only because
  `MapKeys` hands back non-addressable ones. Removing it means aliasing
  the minted type back to a `ScriptKey`, which is `intoKeyStore` in
  reverse and would be the sixth unsafe expression. Not taken for what is
  left of the gap.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time. Unchanged.
- **A range over a struct-keyed map costs ~62-87ns a key at one field.**
  Unchanged.
- **Ties in `sortMapKeys` are still resolved arbitrarily.** Unchanged.
- **`growForRest`'s estimate is untested**, deliberately.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is a
  narrow call** — 13% margin at eight fields.
- **A re-declared `P` does not find its own earlier keys.** Unchanged --
  and it is now also the reason a map can hold two `*StructType`s and
  decline.
- **`[]map[P]int{{{1}: 2}}` cannot elide the key literal.** Unchanged.
- **`%T` on a container prints storage.** Unchanged.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** --
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

~~**`map[K]V` fields still render through fmt.**~~ Closed for struct keys,
where the gap was 5x per entry and grsh had the machinery to close it; and
closed by decision for scalar keys, where two implementations measured no
better than fmt.
