# Session: Closing the Struct-Key Crossing Cost

Session: 7e85aee5-5659-4366-8745-2714bda31960
Date: 2026-08-28

## Goal

The one number the last session left open:

> **A struct-keyed lookup costs ~+17% against HEAD**, allocations
> unchanged. It is the wrap; `reflect` has no way to build a value of a
> runtime type without `New`.

That diagnosis was half right, and following it alone would have closed
less than half the gap.

## What was actually expensive

Priced against `e1b9aa3` — the commit BEFORE keys were minted — with the
three binaries interleaved and their order rotated, minimum of twelve
runs:

| shape | HEAD vs pre-mint | now vs pre-mint | allocs |
|---|---|---|---|
| `map[P]int` read | **+20%** | **+2%** | −1 per crossing |
| `map[P]int` write | **+21%** | **+2%** | −1 per crossing |
| `range` over struct keys | **+8%** | **−1.5%** | −1755 |
| element + native shapes (controls) | ±2% | ±2% | unchanged |

The controls drift ±2–4% run to run on this machine, so all three key
shapes are back inside the noise band and the range shape is genuinely
below where it started.

## Three costs, three different fixes

**Encode — `structKeyOf`, 78ns → 22ns for a three-field key, one
allocation fewer.** This was the biggest single piece and the last
session had recorded it as "unchanged, ~67ns" — true, and not the same as
"irreducible". The key array's TYPE is chosen at runtime, so the encoder
went through `reflect.New` plus a `Set` per field plus `Interface()` —
and that last call COPIES the array before boxing it, because the value
is addressable. Field counts up to `keyArrFanout = 4` now build a Go
array literal from a stack buffer instead, no reflect at all.

Each case RETURNS, so an arity with no case falls through to the reflect
path. Raising the constant without adding a case costs speed and never
correctness — which is the only reason a fanout constant is safe to leave
for someone else to tune.

**Wrap — `intoKeyStore`, 45ns → 21ns.** The last session's diagnosis, and
the fix is the same observation the design already rests on: a minted key
type is exactly one embedded `ScriptKey` and nothing else, so it IS a
`ScriptKey` under another name — same size, alignment and pointer map.
`reflect.NewAt` aliases it in one step instead of `reflect.New` plus two
`Set`s.

This is the first `unsafe` in non-test code. `mintKeyType` now ASSERTS
the layout at declaration rather than trusting it, so a second field
added to `mint` panics there instead of silently mis-typing every key,
and the canary test states the same invariant where a reader will meet
it.

**Range — one decode per key instead of two.** `sortMapKeys` decoded
every key to render it for ordering, then the loop threw that away and
decoded it again. It now returns the decoded keys alongside the sorted
slice and the range loop uses them. That is why the range shape ends up
FASTER than pre-minting: the duplication predates the minting and the
minting only made it more expensive.

`fromKeyStore` lost its last caller in the process and folded into
`decodeMintedKey`, which takes no guard: keys are read a whole map at a
time, and every such loop already knows the key type for the whole map.

## The micro-benchmark lied about the range path

The first version of the decode benchmark measured a key straight from
`intoKeyStore` and showed a change saving one allocation per ranged key.
The script benchmark then showed 4 allocations saved out of an expected
1000.

`reflect` boxes a struct out of an ADDRESSABLE value by copying it to the
heap first, and out of a non-addressable one for free. A key from
`intoKeyStore` is addressable; a key from `MapKeys()` — which is every
key the range path touches — is not. The allocation the benchmark was
"saving" had never been paid.

**A micro-benchmark has to reproduce the CALLER's value, not just its
type.** The committed `BenchmarkKeyCrossing` sources its decode input
from `MapKeys()` and the doc comment says why, with the wrong result
recorded so nobody re-derives it.

## The guard pass found real gaps

Fourteen mutations. Four survived the first pass and each named something
the tests could not see:

- **A 3-field fast path that drops its last field survived** — no test
  used a struct map key with more than two fields. An arity-specific
  encoder makes every arity its own branch.
- **A 4-field path that swaps two middle fields survived** even after the
  arity test existed. A key stored and sought through the SAME wrong
  order still matches itself, so no lookup can see a permutation. Only
  decoding the key back against the field NAMES can.
- **The reflect fallback collapsing every field into slot 0 survived** the
  first arity test, because that test varied only the LAST field — which
  is exactly the field that survives the collapse. Both ends of the field
  list have to be probed.
- **A 0-field key encoded as `[1]any` survived** and always will from a
  script: every key of a map takes the same branch, so a length
  disagreement between the two paths is self-consistent and invisible.
  `TestKeyEncodingMatchesTheDeclaredArrayType` pins it at the unit level
  instead, which is what makes the fanout safe to change.

`TestStructMapKeysAtEveryArity` walks 0–5 fields (0 for the empty array,
4 for the fanout itself, 5 for the first arity past it), gives every field
a distinct value, probes a hit and a miss at BOTH ends, then prints the
map so the decode is checked against the field names.

The one deliberate survivor is removing the layout panic in
`mintKeyType`: the canary test asserts the same invariant, so the panic
is redundant in CI and is there for production declarations.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, testcache
cleared. No behavior changed — this is entirely a cost change — so no
test pinned old behavior and `docs/LANGUAGE.md` needed nothing.

New: `TestStructMapKeysAtEveryArity`,
`TestKeyEncodingMatchesTheDeclaredArrayType`, the layout assertion inside
`TestMintedTypePromotesExactlyOneMethod`, and `BenchmarkKeyCrossing`.
`BenchmarkStructContainer`'s doc comment carries the new table and says
how it was measured.

## Method notes

- **A recorded diagnosis is a hypothesis, not a finding.** "It is the
  wrap" was accurate and incomplete; the encode was the larger half and
  had been written off as unchanged. Re-measure the whole path before
  optimising the part someone already named.
- **Benchmark against the commit the regression is measured from.**
  Comparing to current HEAD showed 6–7% and looked like most of the job;
  the pre-minting commit showed the remaining 13%. Building a worktree at
  the old commit and copying the CURRENT bench shapes into it is what
  makes the two comparable.
- **Rotate the order of interleaved runs.** With the new binary always
  last, the untouched controls read +3.7% — a thermal or scheduling
  artifact that would have been reported as a regression. Rotating put
  every control back inside ±2%, and the controls are the only reason the
  key numbers can be believed.
- **A micro-benchmark must reproduce the caller's value.** See above.
- **A fast path per arity needs a test per arity, and lookups cannot see
  a permutation.** Round-trip through the DECODE, not just the map.
- **Backups, not `git checkout`, for mutation testing** — carried over
  from last session and used throughout.

## Open

- **`unsafe` is now in `internal/interp`**, in exactly one expression,
  guarded by a runtime layout check and a test. Worth knowing about
  before the carriers are touched again.
- **A re-declared `P` does not find its own earlier keys.** Unchanged.
- **`[]map[P]int{{{1}: 2}}` cannot elide the key literal.** Unchanged.
- **`%T` on a container prints storage.** Unchanged.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.
- **`keyArrFanout` is set at 4 by judgement, not measurement.** Nothing
  says where the enumeration stops paying; the tests make it safe to
  move.

~~**A struct-keyed lookup costs ~+17% against HEAD.**~~ Closed here, along
with the range shape it did not name.
