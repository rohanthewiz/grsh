# Session: Tuning keyArrFanout by Measurement

Session: db6e9cdc-78d1-4795-acba-db6b5571f2cc
Date: 2026-08-28

## Goal

The last open item from the previous session:

> **`keyArrFanout` is set at 4 by judgement, not measurement.** Nothing
> says where the enumeration stops paying; the tests make it safe to
> move.

It is now 8, and the sentence it was set on turned out to be the wrong
question.

## The premise was wrong

The old comment said the cutoff belongs "where the enumeration stops
paying", which quietly assumes the two encode paths converge. They do
not. Both paths measured in ONE binary -- `keyArrFanout` made a `var` in
a throwaway worktree so a single build could force either path over the
same `*StructVal` -- minimum of ten runs, Apple M3:

| fields | 1 | 2 | 3 | 4 | 5 | 6 | 8 | 12 |
|---|---|---|---|---|---|---|---|---|
| reflect | 49 | 67 | 81 | 100 | 117 | 132 | 159 | 224 ns |
| literal | 16 | 20 | 23 | 26 | 31 | 34 | 41 | 56 ns |

A flat ~75% saving out to twelve fields and one allocation fewer at every
arity: reflect costs ~15ns per field against ~3ns, and then `Interface()`
copies the array out. There is no crossover to find. So the cutoff is a
TRADE -- what an added case gives a key of that arity, against what it
costs every key -- and the job became measuring the second half.

## The first cost measurement conflated two things

`buf[4]` with 5 cases against `buf[12]` with 13 cases showed small keys
paying 0.8-2.3ns, and that comparison moves the buffer AND the switch at
once. Split into three variants holding two things fixed:

- **buffer alone**, 4 slots -> 12 slots at 5 cases: +0.8ns at one field,
  +1.6ns at four. About 0.18ns per unused slot -- the buffer is sized by
  the constant and zeroed whole on every call.
- **switch alone**, 5 cases -> 13 cases at 12 slots: nothing measurable.

So the switch is free and the buffer is the whole tax. That is what makes
8 defensible: ~0.7ns off a 15ns one-field encode, buying a five- to
eight-field key ~117-159ns down to ~31-41ns.

## The variant that would make the tax zero, and why it was dropped

A per-case array (`var a [N]any` inside each case, filled through a
generic `fillKeyArr(sv, &a, a[:])`) has no shared buffer at all, so
raising the cutoff would cost small keys nothing. Measured at every
arity it ties or loses everywhere except 0 fields -- the generic fill
does not inline, and the call costs about what the buffer's zeroing did.
Verified equivalent to the shipped path first (same array type, same
field order, arities 0-12) so the comparison was of two shapes and not
two jobs. Not worth the churn; the shared buffer stayed.

## End to end

Two builds differing ONLY in the constant, interleaved with the order
rotated, minimum of six runs each:

| shape | delta | allocs |
|---|---|---|
| `map-key-struct-hit-6` (new) | **-19.7%** | -1 per crossing |
| `encode/6` micro | **131 -> 33 ns** | 2 -> 1 |
| one-field key shapes | +0.2%, -1.5% | unchanged |
| untouched controls | within +/-1.9% | unchanged |

The 0.7ns is real in-binary and sits under the between-build noise. The
one-field shapes are benchmarked beside the wide one deliberately: they
are what pays for the extra cases, so a tax would show up in them.

## The guard pass

Four mutations on the new cases, backups rather than `git checkout`:
case 5 dropping its last field, case 6 swapping two middle fields, case 7
building a 6-long array, case 8 collapsing every field into slot 0. All
four die -- `TestStructMapKeysAtEveryArity` now walks 0-9, the whole
enumeration plus both edges, and case 7's length is also caught by
`TestKeyEncodingMatchesTheDeclaredArrayType`.

The fifth mutation found something instead of confirming something:
**lowering the constant below the last case does not compile.** `buf` is
sized by `keyArrFanout`, so the higher cases index past its end --
`invalid argument: index 4 out of bounds [0:4]`. Raising it past the last
case is merely slow (fall through to reflect); lowering it is a build
error. The compiler guards the direction the test cannot see, and the
constant's comment now says so.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, plus a full run
on a cleared testcache. No behavior changed -- a key of any arity encoded
correctly before and encodes identically now -- so nothing pinned old
behavior and `docs/LANGUAGE.md` needed nothing. Both measurement
worktrees removed.

Changed: `keyArrFanout` 4 -> 8 with cases 5-8 and a comment carrying the
tables and the method; both arity tests extended to 0-9;
`BenchmarkKeyCrossing` gained arity 6; `BenchmarkStructContainer` gained
`map-key-struct-hit-6`.

## Method notes

- **Check the question before answering it.** "Where does the
  enumeration stop paying" presumes a crossover. Sweeping both paths
  first showed there is none, which turned a search for a break-even into
  a trade with two sides to price.
- **A comparison that moves two things measures neither.** Buffer size
  and case count had to be varied one at a time; the combined number was
  real and told me nothing about which knob to turn.
- **Make the constant a var in a throwaway tree.** Both paths in one
  binary over the same value beats two builds, whose difference is
  partly the scheduler.
- **Price the shape you are about to reject.** The per-case array was the
  obvious fix for the buffer tax and lost; measuring it is why the
  simpler code shipped.
- **A mutation can find a property rather than a hole.** The build error
  on lowering the cutoff was worth more than the survival check it came
  from.

## Open

- **`unsafe` is in `internal/interp`**, one expression in `intoKeyStore`,
  guarded by a runtime layout check and a test. Unchanged.
- **The reflect encode path is still 2 allocations and ~15ns/field.** It
  now serves only 9+ field keys. An unsafe eface construction would drop
  one allocation but not the per-field `Set`, so the enumeration remains
  the real fix.
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

~~**`keyArrFanout` is set by judgement, not measurement.**~~ Closed here.
