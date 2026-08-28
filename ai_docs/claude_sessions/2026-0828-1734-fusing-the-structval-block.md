# Session: Fusing the StructVal Block

Session: fdbd518d-38cc-4f44-add0-4dbd5ae329de
Date: 2026-08-28

Continues `2026-0828-1712-aliasing-the-key-array-box.md`, taking its next
open item:

> **`decodeMintedKey` is still 2 allocations** -- 39ns at one field, 95ns
> at ten. The decode side has had no equivalent pass.

The item was scoped too narrowly. The two allocations were not decode's;
they were every script struct's.

## The pair was in all three constructors

`decodeKeyArr` paid `make([]Value, n)` and then `&StructVal{}`. So did
`newZero` -- on every struct literal and every `make([]P, n)` element --
and so did `copyStruct`, on every store and every value receiver. Three
call sites, one shape, and the decode benchmark was simply the one place
it had been priced.

Fixing the named item alone would have left two thirds of the cost in
place on paths that run far more often than a key decode.

## One object instead of two

`newStructVal(t, n)` returns a `*StructVal` whose `Vals` backing array
lives in the SAME allocation:

```go
type valBlock[A any] struct {
	sv   StructVal
	vals A
}
```

`A` is always a `[N]Value`. It is a type parameter only because N cannot
be one -- an array's length is part of its type. Instantiating at the use
site is what makes the slicing legal: inside a generic function
`b.vals[:]` does not compile, but at `valBlock[[6]Value]` the field is
concrete.

`sv` sits at offset 0, so the returned pointer is the block's base
pointer and `Vals` is an interior pointer just past it -- which keeps the
block alive exactly as long as the StructVal was going to be alive
anyway.

Bytes are unchanged at every arity: a StructVal is 32 bytes, and 32+16n
lands in the same size class that 32 and 16n did apart. The whole saving
is one trip through the allocator.

`n` is a parameter rather than `len(t.Fields)` because `copyStruct` sizes
from the INSTANCE -- an oversized malformed value is copied as it is, not
silently truncated into a plausible good one.

## The fanout is 16, and the reason is not keyArrFanout's

`keyArrFanout` is 8 because its cases share one stack buffer that every
call zeroes whole, so each added case taxes every key. That trade does
not exist here: the cases are independent and the switch is a jump table,
so a case costs BINARY SIZE and nothing else.

Fused against unfused, both in one binary, fixed iteration count:

| fields | 1 | 2 | 4 | 6 | 8 | 10 | 12 | 14 | 16 |
|---|---|---|---|---|---|---|---|---|---|
| saved | 6.6 | 9.8 | 10.2 | 5.5 | 14.2 | 6.9 | 9.3 | 4.1 | 11.0 ns |

No crossover, identical bytes on both paths. Cases 9-16 together cost 976
bytes of binary (0.009%), bought `decode/10` 19.8% (95.1 -> 76.3ns), and
left `decode/1` unmoved (29.8 -> 30.2ns, inside the noise). That last
number is the one that licenses the extra cases: they are free to the
arities scripts actually write.

16 stops for want of a reason to go further rather than for a cost.

## Nothing in the switch can see the constant

The cases are literal, so `valBlockFanout` is a CONTRACT rather than an
input. `TestStructValFusesUpToTheFanout` walks `1..valBlockFanout`
asserting one allocation each, which is the only thing that can keep the
two in step -- a constant raised without a case would put those arities
back on the two-allocation path and cost nothing visible.

## What a script feels

Two interleaved builds differing only in this function, six runs each:

| shape | time | allocs |
|---|---|---|
| `StructZero/nested` | **-11.1%** | -25.3% |
| `StructCopy/nested` | **-8.9%** | -20.2% |
| `StructZero/flat` | -6.6% | -15.7% |
| `StructCopy/flat` | -5.7% | -12.7% |
| `range-slice-struct` | -3.5% | -8.9% |
| `range-map-key-struct` | -3.4% | -10.5% |
| `map-miss-struct` | -2.2% | -7.3% |
| `map-hit-struct` | +0.4% | ~0 |
| `slice-index-struct` | +0.4% | ~0 |
| geomean, 16 shapes | **-2.7%** | **-6.7%** |

The two that got slower build nothing in their loops -- they read fields
out of structs that already exist -- and their allocation counts did not
move, so 0.4% there is code layout rather than this change. Recorded as
measured rather than rounded away.

`decode` on its own: 39.4 -> 30.0ns at one field, 52.9 -> 41.9 at three,
68.9 -> 55.0 at six, 95.1 -> 76.3 at ten, one allocation throughout.

## The guard pass

Five mutations, all killed:

- **A case slicing the wrong length** (`b.vals[:5]` in case 6) -- dies in
  `TestStructMapKeysAtEveryArity` with an index-out-of-range panic.
- **A case built on an oversized block** (`[7]Value` for case 6) -- dies
  in the fanout test on the allocation count.
- **`valBlockFanout` raised to 17 without a case** -- dies in the fanout
  test, which is the whole reason it exists.
- **`copyStruct` sizing from the TYPE** rather than the instance -- dies
  in `TestCopyStructSizesFromTheInstance`.
- **`newZero` dropping its nested-struct duplication** -- dies in
  `TestStructTypedFieldZeroIsPerInstance`, unchanged from before.

New tests: `TestStructValFusesUpToTheFanout`,
`TestStructValsDoNotShareABlock` (the hazard fusing introduces -- two
instances handed one block would pass every test that writes and reads
the same instance, so this writes through one and reads the other),
`TestFusedStructValSurvivesGC`, `TestCopyStructSizesFromTheInstance`.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. No behavior changed -- same values, same bytes, same pointer
identity per instance -- so nothing pinned old behavior and
`docs/LANGUAGE.md` needed nothing. Measurement worktree removed.

No new `unsafe`: the count in `internal/interp` stays at three
expressions.

## Method notes

- **Price the item, then look for its shape elsewhere.** The open item
  named one function. Grepping `&StructVal{` found the same two-allocation
  pair in two hotter ones, and the script-level numbers came almost
  entirely from those.
- **A threshold inherits nothing from a neighbouring threshold.**
  `valBlockFanout` looks like `keyArrFanout` and is set by a completely
  different argument, because the per-case tax `keyArrFanout` trades
  against does not exist here. Copying 8 across would have left ~20% on
  the ten-field decode for no reason.
- **Measure the cost of MORE cases, not just the benefit.** 976 bytes and
  an unmoved `decode/1` are what made 16 defensible; without the second
  number the extra cases would have been a guess.
- **A constant no code reads needs a test that reads it.** Otherwise the
  enumeration and the constant drift silently and everything still passes.
- **Report the regressions.** Two shapes read +0.4% with p < 0.05. Their
  allocation counts say it is layout, and saying so is more useful than a
  geomean that hides it.

## Open

- **`decodeMintedKey`'s remaining cost is per-field work**, ~4.6ns a
  field through `fromKeyValue` and `arr.Index(i).Interface()`. There is no
  allocation left to remove; a cheaper decode means a cheaper per-field
  read.
- **`valBlockFanout` at 16 is cheap to raise** -- ~122 bytes of binary per
  case, no per-call tax -- if a script ever wants wider structs.
- **`unsafe` in `internal/interp` is three expressions**: the `NewAt`
  alias in `intoKeyStore`, and `unsafe.Slice` plus the eface
  reinterpretation in the general encode path. Each has a runtime check or
  a test stating its invariant.
- **`keyArrFanout` at 8 is a narrow call.** The margin is 13% at eight
  fields. If the general encode path gets cheaper again, the top cases are
  the first to drop.
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

~~**`decodeMintedKey` is still 2 allocations.**~~ Closed here: one
allocation, and the same fix took one out of every struct literal, every
copy and every zero.
