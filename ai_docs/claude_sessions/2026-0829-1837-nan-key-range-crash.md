# Session: The NaN Key Range Crash

Session: 386169f0-194f-4d70-9941-13d827b725b5
Date: 2026-08-29

Takes the first Open item of `2026-0829-1809-every-map-key-in-fmts-order.md`,
which surfaced the crash on the way past and deliberately left it:

> **Ranging a map with a NaN key crashes** in `rangeOver`, which reads
> values with `MapIndex` and cannot find a key that does not equal
> itself. Pre-existing, surfaced by this session's float test, NOT fixed:
> the fix is one `MapRange` pass carrying values beside keys, which is a
> cost on every map range and a decision of its own.

The decision turned out to be avoidable. The cost is not on every map
range; it is on the maps that can hold a NaN, and it is decided by TYPE.

## The crash, and it was three crashes

`rangeOver` walked `rv.MapKeys()` and read each value with
`rv.MapIndex(k)`. A NaN is a live entry of the map that no lookup can
ever find, so `MapIndex` handed back the zero `Value` and the range died
reading it:

```
grsh: grsh internal error: reflect: call of reflect.Value.Interface on zero Value
```

Reproduced before touching anything, in all three ways a NaN reaches a
map key -- and it is worth naming all three, because the first is the
only one the Open item had in mind:

```
map[float64]int{1: 1, nan: 2}                 -- a bare float key
map[any]int{1: 1, nan: 2}                     -- an interface boxing one
map[P]int{P{X: 1}: 1, P{X: nan}: 2}           -- a script struct field
```

## The fix: one MapRange pass, for the maps that need one

`mapKeysAndVals` reads keys and values together. An iterator hands over
the entry it is standing on and never has to find it, so a NaN key's
value arrives with it.

`vals` is then carried in step with `keys` through every sort. `keyOrder`
and `textOrder` gained a `vals` slice their `Swap` moves; a new
`scalarOrder` does the same job for the float and interface paths,
because **`slices.SortFunc` cannot move a second slice** -- it swaps the
elements of the one slice it was given, and `sort.Interface` is the only
shape that can do more. That is the whole reason a second sorter exists.

## Which maps pay is decided ONCE, BY TYPE

`mayNotEqualItself(t)` answers from the key type alone: float or complex,
interface, or a composite built out of either. Everything else -- int,
string, bool, pointer, channel -- is equal to itself always and keeps the
`MapKeys` + `MapIndex` route untouched.

Asking the type rather than the keys is forced: asking the keys means a
pass over them, and the pass is the thing being decided on.

`BenchmarkRangeMapEntries` prices the whole acquisition -- get the keys,
order them, reach every value -- both ways over the same maps. 64
entries, ns per entry, Apple M3, minimum of eight runs:

```
key         paired   by key
 int          58.1     58.6     same route, so same number
 string       70.6     70.1     same route
 float64      74.5     68.5     +9%, and two allocations
 struct P    108.1    142.7     -24%, and one allocation
```

**The first draft of the comment at that function was wrong, and the
benchmark is what said so.** It claimed the paired pass "buys back the n
hash lookups" as though that settled it. It buys them back, but a float
hashes in a few instructions and the slice costs more than 64 of them --
so the float row LOSES by 9%. A minted struct key is four words with an
interface field in it, and there the same trade wins by 24%. The comment
now carries the table and says which way each row goes.

That also settles a refinement not taken. A `map[P]int` whose `P` holds
only ints still takes the paired pass, because the minted key stores its
fields as `any` and the type cannot say what went into them. The struct
row is why that is left alone: the conservative answer is the FASTER one
there, so narrowing it would be a pessimisation dressed as a narrowing.

## Six comparators became six functions

`orderScalarKeys` had its comparators written as closures inside a `Kind`
switch. There are two callers now -- keys alone, and keys beside values
-- and two copies of a comparator is two chances to sort differently. So
the switch became `scalarKeyCmp(kind)`, returning a named function that
both paths call.

The worry was that a func value returned from a helper would cost an
indirect call the inline closure did not. It does not: `slices.SortFunc`
calls its comparator through a func value either way, and its body is far
too big to inline the closure into. `BenchmarkSortMapKeys` before and
after, ns per key, minimum of six runs:

```
keys        4     16     64    256   1024
 int   8.3/8.2  12.8/11.0  20.4/19.9  28.7/26.1  38.8/39.0
 string 9.2/9.7 15.6/14.8  26.6/28.3  37.7/37.4  50.5/50.6
```

Noise in both directions, zero allocations still.

## The same defect was two files over

`inspect.go` -- the REPL's `?name` -- had the same `MapKeys` plus
`MapIndex` pair, and `?m` on a NaN-keyed map panicked the prompt.
Confirmed with a throwaway test before fixing, then fixed with the same
helper: `keySorter` gained a `vals` its `Swap` moves, and `structNameIn`
switched to `MapRange` because it wants only the values.

This is a small scope extension over "fix rangeOver", taken because it is
the same defect with the fix already in hand and reachable by a user at a
prompt.

## The probe pass, and a probe that was a coin flip

Nineteen probes, all nineteen caught by a named test: the triage removed
entirely, each of float/interface/struct dropped from
`mayNotEqualItself`, each of the four `Swap`s dropping its values, both
scalar paths sorted unpaired, the range reading by key again, the value
slice left short, four orderings removed or reversed, and the two inspect
sites reverted.

**One probe first read as UNCAUGHT and the probe was right -- the TEST
was the weak thing.** "keySorter.Swap drops the values" passed a single
run, because with three keys the map's own randomised order already is
the sorted order about one time in six. A mispairing bug that only shows
on a non-identity permutation reads as a pass that often.

Both new pairing tests were hardened rather than the probe re-run:
`TestInspectAMapWithANaNKey` inspects five keys eight times, and
`TestAScriptRangesAMapWithANaNKey` builds and ranges five fresh maps
inside the script. Re-probed with `-count=3` after; all four `Swap`
probes then failed on the first try.

That is a fifth entry in this project's running list of ways a probe
reports a false negative -- and the first where the fault was in the test
rather than in the mutation. The four before it: the mutation was a
no-op; the thing mutated was a tuning choice; a widening that preserved
order; and a one-key shortcut worth nothing on the path probed.

## A pre-existing gap the tests walked into

Writing `map[P]string{P{X: 1}: "a", ...}` for a `P` with a `float64`
field does not order field-wise. `%T` on that field says `int`: an
untyped constant is not converted to the field's declared type, so the
field holds two dynamic types across the keys, `keyCmp` declines, and the
order falls back to the rendered text.

Nothing to do with this fix. The test writes `1.0` and `-2.0` and says at
the literal why.

## New tests

- `TestAScriptRangesAMapWithANaNKey` -- the three shapes end to end, five
  fresh maps each, ranged and compared as whole lines because a struct
  key renders with a space inside it and splitting on whitespace compares
  two wrong lists that print identically in a failure message.
- `TestARangedMapPairsEveryValueWithItsOwnKey` -- six maps, ten runs
  each, covering all three value-carrying sorters. **Every value is its
  own key's text**, so the pairing is checkable without a lookup, which
  is the point: a NaN key is exactly the key no lookup can find.
- `TestMayNotEqualItselfNamesTheKeysMapIndexCannotFind` -- the type
  table, including the minted key type. A wrong NO is a crash and a wrong
  YES is only slower, and only a table makes the NO side visible.
- `TestInspectAMapWithANaNKey` -- the `?name` half.
- `BenchmarkRangeMapEntries` -- the table above, with `bykey` rows
  running the old shape against the same maps.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. End to end through `cmd/grsh`, three runs of each of the three
shapes: no crash, every range matching its print, and identical across
runs.

## Method notes

- **An Open item can be smaller than it was written.** It said the fix
  was "a cost on every map range". It is a cost on maps whose key type
  can hold a NaN, which is a question answerable once, from the type, and
  the two commonest maps a script writes answer no.
- **The benchmark contradicted the comment, and the comment lost.** The
  claim that skipping hash lookups pays for itself is true for a struct
  key and false for a float. Writing the table down turned a plausible
  sentence into two measured rows going opposite ways.
- **Two callers of one comparison is one comparator.** The paired and
  unpaired sorts cannot drift into two orders if there is only one
  function to drift.
- **A probe run once is a coin flip when the input is randomised.** The
  harness must run a pairing probe more than once, or the test must make
  an accidental pass negligible. The second is better: it fixes the test
  for everyone who runs it later, not just for the probe pass.

## Open

- **`P{X: 1}` stores an int in a `float64` field.** An untyped constant
  is not converted to the declared field type, so such a key declines to
  the text order rather than sorting field-wise. Found by this session's
  tests, NOT fixed, stepped around at the literals.
- **Pointer, channel, complex, array and native-struct keys stay
  unordered.** Unchanged. Note that complex and array keys now take the
  paired pass -- they can hold a NaN -- while still not being ordered.
- **A one-field struct key past ~256 entries orders 18% slower** than the
  text order it replaced. Unchanged.
- **A scalar-keyed map field still RENDERS through fmt.** Unchanged.
- **Two allocations an entry remain** in `appendStructKeyedMap`.
  Unchanged.
- **A nested `[][]T` still renders through fmt**; a `[]error`'s elements
  still reach fmt one at a time. Unchanged.
- **Ties in the text-order fallback are still resolved arbitrarily.**
  Unchanged.
- **`growForRest`'s estimate is untested**, deliberately.
- **`valBlockFanout` at 16 is cheap to raise**; **`keyArrFanout` at 8 is
  a narrow call** -- 13% margin at eight fields.
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

~~**Ranging a map with a NaN key crashes.**~~ Closed, for a range and for
the REPL's inspector, and without putting a cost on the maps that cannot
hold one.
