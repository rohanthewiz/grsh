# Session: Aliasing the Key Array Box

Session: db6e9cdc-78d1-4795-acba-db6b5571f2cc
Date: 2026-08-28

Continues `2026-0828-1650-tuning-keyarrfanout-by-measurement.md`, which
closed with this as its own open item:

> **The reflect encode path is still 2 allocations and ~15ns/field.** It
> now serves only 9+ field keys. An unsafe eface construction would drop
> one allocation but not the per-field `Set`, so the enumeration remains
> the real fix.

Both halves of that turned out to be wrong: the per-field `Set` goes too,
and what the two changes together buy is large enough to force the
previous session's conclusion to be re-derived.

## One allocation is the floor, and the second was pure cost

A `[N]any` is more than a word, so an `any` holding one must point at a
heap copy. One allocation is not a choice; the literal path has always
paid exactly that. The reflect path paid two:

- `reflect.New` allocated the array.
- `Interface()` allocated a SECOND one and copied into it, because the
  value it boxes is ADDRESSABLE and an interface's contents must not
  change underneath it.

Nothing can write through that array -- `structKeyOf`'s fill loop is its
only writer and it has finished by then -- so the array is already the
immutable thing an interface requires and the copy bought nothing.

## reflect now allocates and does nothing else

- **Fill** through `unsafe.Slice` aliasing the array: ordinary interface
  assignments instead of a reflect `Set` per field, ~3ns a field against
  ~15ns. The slice is sized from `keyArr.Len()`, never from `len(Vals)`,
  so an oversized instance lands on a bounds check exactly as
  `arr.Index(i)` used to instead of writing past the array.
- **Box** through `boxKeyArr`: the array is handed to an interface where
  it already lives. `N` is chosen at runtime, so the type word has to be
  borrowed from a value that already carries it -- a boxed zero cached on
  the StructType as `keyArrZero`.

The `if ev == nil { continue }` guard disappeared with the reflect `Set`
it existed for: assigning a nil interface is just an assignment.

| | before | after |
|---|---|---|
| `encode/10` micro | 193.3ns, 2 allocs, 320B | **55.0ns, 1 alloc, 160B** |
| 10-field key read, script | 639.5ns | **487.6ns (-23.8%)**, -1 alloc/crossing |
| every other shape | -- | within +/-0.8% |

One allocation moved the other way: `keyArrZero` costs one per DECLARED
TYPE, at declaration.

## The unsafe is two words, and it is written to keep the write barrier

`eface` mirrors an interface's two words. Only the REINTERPRETATION is
unsafe: every pointer is written by an ordinary assignment to that typed
struct, so the write barrier is emitted normally. Storing straight into
an `any`'s words through a pointer -- the other way to write this trick
-- skips the barrier and breaks under a concurrent mark. That distinction
is the whole reason the helper is shaped the way it is.

`mintKeyArrZero` probes the alias once per declaration: it writes a mark
into a fresh array and reads it back THROUGH the box, so a wrong type
word, a wrong data word, or an interface that stops being two words in
this order fails loudly at the declaration rather than silently. Same
spirit as `mintKeyType`'s layout check on `intoKeyStore`.

## The previous session's conclusion had to be re-taken

`keyArrFanout` was set at 8 against a general path costing 117-224ns at
five to twelve fields. That path no longer exists, so its numbers no
longer justify anything. Re-swept, both paths in one binary:

| fields | 1 | 2 | 4 | 6 | 8 | 10 | 12 |
|---|---|---|---|---|---|---|---|
| general | 23.6 | 27.3 | 31.5 | 39.7 | 44.1 | 55.0 | 57.5 ns |
| literal | 16.2 | 20.0 | 24.9 | 32.5 | 38.2 | 47.3 | 54.2 ns |

Still no crossover -- a literal is ~6-7ns cheaper at every length, being
a stack array and one convT against `reflect.New` plus a runtime type
lookup -- so the enumeration stays. But 31% at one field and 13% at eight
is a far narrower margin than the 75% it was set with, and the constant's
comment now says so, with the old numbers marked as what they were.

## The guard pass

Five mutations, all killed:

- **`boxKeyArr` drops the data word** -- dies in the declaration probe,
  before a key is ever encoded.
- **`eface`'s two words declared in the wrong order** -- same, and the
  panic prints an empty type name, which is what a nil type word looks
  like.
- **The fill slice sized from the instance** rather than the declared
  array -- dies on `TestKeyEncodingRefusesAnOversizedInstance`. This is
  the mutation worth having: the difference is invisible until something
  builds a malformed instance, and then it is a silent write past the
  array.
- **The fill writes the mirrored slot** -- dies in three places.
- **`keyArrZero` borrowed from an array of the wrong length** -- dies.

New tests: `TestKeyArrayBoxAliasesRatherThanCopies` (asserts the box
aliases by writing through the array afterwards and watching the box
change -- the hazard the design avoids, used as the evidence no copy
happened), `TestWideKeysSurviveGC`, `TestWideKeyWithNilField`,
`TestKeyEncodingRefusesAnOversizedInstance`,
`TestKeyEncodingAcceptsAShortInstance`. Both arity tests now walk 12 as
well as 9.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green on a cleared
testcache. No behavior changed -- a key of any arity encodes to the same
bytes -- so nothing pinned old behavior and `docs/LANGUAGE.md` needed
nothing. Both measurement worktrees removed.

## Method notes

- **A recorded diagnosis is a hypothesis** -- again. Last session's
  "unsafe would drop one allocation but not the per-field Set" was
  written without trying it; `unsafe.Slice` drops the Set too, and the
  two together were worth 3.5x rather than the 2x the note implied.
- **A change that rebuilds a path invalidates every constant tuned
  against it.** The fanout was measured yesterday against numbers this
  change deleted. Re-sweeping was not optional bookkeeping; without it
  the comment would assert a 75% margin that is now 13-31%.
- **Shape the unsafe so the compiler still emits the barrier.** Writing
  the pointer into a typed struct and reinterpreting the struct is safe
  where writing through a pointer into the interface is not, and the two
  look nearly identical on the page.
- **Bound a slice by the type, not by the instance.** The version sized
  from `len(Vals)` passes every test that exists and corrupts memory the
  day an instance is malformed; the test that kills it had to be written
  for a case nothing currently produces.
- **Keep one bench shape per path.** `map-key-struct-hit-6` is on the
  literal path and `-hit-10` on the general one, so a change to either
  shows in exactly one shape.

## Open

- **`unsafe` in `internal/interp` is now three expressions**: the
  `NewAt` alias in `intoKeyStore`, and `unsafe.Slice` plus the eface
  reinterpretation in the general encode path. Each has a runtime check
  or a test stating its invariant.
- **`keyArrFanout` at 8 is a narrower call than it was.** The margin is
  now 13% at eight fields. If the general path ever gets cheaper again,
  the top cases are the first thing to drop.
- **`decodeMintedKey` is still 2 allocations** -- 39ns at one field,
  95ns at ten. The decode side has had no equivalent pass.
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

~~**The reflect encode path is 2 allocations and ~15ns/field.**~~ Closed
here: one allocation, which is the floor, and ~3ns a field.
