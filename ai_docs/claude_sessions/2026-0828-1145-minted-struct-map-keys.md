# Session: Minted Struct Map Keys

Session: dfab5f25-bdc6-4857-ab66-dfe2c2271945
Date: 2026-08-28

## Goal

Close the one leaf the last session left open:

> **An EMPTY `map[P]int` asserts to `map[Q]int`.** The last leaf still
> answered by value; a non-empty one is exact.

## The shape of the fix

The element side was solved last session by minting a reflect.Type per
struct and holding it in the container. The key side was left alone
because `StructKey` — the comparable stand-in a struct becomes in key
position — has an UNEXPORTED method (`structVal`), and embedding a type
with one into `reflect.StructOf` kills the process from `itabInit`.

The trap is only about EMBEDDING. A named field promotes nothing:

```go
type ScriptKey struct{ K StructKey }        // one exported method
func (k ScriptKey) String() string { return k.K.String() }

reflect.StructOf([]reflect.StructField{{
    Name: "ScriptKey", Type: keyCarrierType, Anonymous: true,
    Tag:  reflect.StructTag(`grsh:"P|X:int"`),
}})
```

So the key carrier is the exact analogue of `ScriptStruct`, and the whole
design is the element design with a second carrier and a second table.
`map[P]int` and `map[Q]int` are now different reflect.Types, and the
assertion is answered by the type whether or not a key is present.

**The design was probed before it was written** — a standalone two-package
program that minted the type, used it as a map key, looked up with a
freshly built key, and printed the map through `fmt`. One promoted
method, comparable, field-wise equality, no crash. That is the lesson from
last session applied on purpose rather than learned again.

## What the tests caught that the probe could not

With `KT` retired and `TypeDesc.Key()` reading the struct off the minted
type via `keyOwnerOf`, half the key tests broke: elided key literals
(`map[P]int{{1}: 10}`) built keys that no lookup could find.

The cause is a fact the element side does not have to care about:

- Minted types are interned by SHAPE, so the registry holds the FIRST
  `*StructType` that minted a shape.
- `StructKey` identity includes that `*StructType` POINTER.

So a key built from the registry's P misses every entry stored under the
caller's P. In the test binary the two are different declarations from
different tests; in a script they are different iterations of a loop or
different REPL lines. **It is already true at HEAD** — probed directly:

```
type P struct { X int }
m := map[P]int{}; m[P{1}] = 1
type P struct { X int }
m[P{1}]                          # 0, at HEAD and now
```

So `KT` stays, narrowed to ONE job: carrying the caller's own StructType
down to key CONSTRUCTION. Recognition — assertions, type names, the
inspector — reads the minted type instead and needs nothing.

The tempting fallback (use `keyOwnerOf` when `KT` is nil, so a nested
`[]map[P]int{{{1}: 2}}` could elide) is exactly the silent-miss bug. It
is refused in code, explained where `Key()` is defined, and pinned by
`TestStructMapKeyElisionNeedsTheTypeWhenNested` — guard mutation M5 does
nothing but add that fallback, and that test is what bites.

## A latent bug found on the way

`typeOf`'s map case branched on `k.ST != nil`, but ST names the struct at
an ELEMENT leaf too. So `map[[]P]int` took the struct-key branch and built
a map keyed by P — accepting a declaration Go rejects, then failing at the
first write with `cannot use [P{X: 1}] ([]P) as struct`, pointing at the
wrong line and the wrong thing. `k.IsStruct()` is the correct test.
`TestSliceOfStructIsNotAMapKey`.

## The canary nearly hid the guard pass

Mutation M1 (stop minting key types) produced a nil `keyT`, which the
extended canary dereferenced — a nil panic that took the package's whole
output with it, so M1 looked like it bit two tests instead of fourteen.
Same failure mode the element canary was written to avoid, reintroduced by
extending it. Fixed with an explicit nil check and `Fatal` before the
first dereference. **A canary must fail cleanly on the thing it is
watching for.**

## The price, measured

A struct-keyed crossing got dearer, and the element side did not move.
Interleaved A/B against HEAD, median of six runs, allocations identical
everywhere:

| shape | delta |
|---|---|
| `map[P]int` read | **+19%** |
| `map[P]int` write | **+17%** |
| `range` over struct keys | **+7.5%** |
| slice index / map hit / map miss / range, struct elements | flat (±3%) |

A micro-benchmark located it exactly: encoding the key is ~67ns and
unchanged, the registry lookup is ~6ns, and the rest is the wrap —
`reflect.New(kt).Elem()` plus two `Set`s where handing the map a bare
`StructKey` cost one `reflect.ValueOf`. About +42ns per crossing.

Two things came out of measuring rather than guessing:

- `intoKeyStore` sets StructKey's two fields INDIVIDUALLY. Setting the
  struct whole costs the same time and one more allocation, because a
  three-word struct boxes to the heap while a pointer and an
  already-interface field box for free. The allocation count is what
  matches HEAD.
- `convertTo` checks the ELEMENT registry before the key one. Both sit
  behind the same Kind guard, so this costs a key one failed lookup and
  saves every element write one.

The number lives in `BenchmarkStructContainer`'s doc comment, next to the
three new key shapes that produce it.

**The first A/B was wrong.** HEAD and the new tree were measured in
different sessions, and the +11% it reported became +19% when the runs
were interleaved. Machine noise on these shapes is larger than the effect;
a baseline from another session is not a baseline.

## Behavior

| case | before | now | Go |
|---|---|---|---|
| `x.(map[P]int)` on an EMPTY `map[Q]int` | true | **false** | false |
| same, with a nil key | true | **false** | false |
| `x.([]map[P]int)` on `[]map[Q]int` | true | **false** | false |
| same, empty | true | **false** | false |
| `m[Q{1}]` on a `map[P]int` | 0 (silent miss) | **error** | compile error |
| `m[Q{1}] = 3`, `delete(m, Q{1})`, `map[P]int{Q{1}: 2}` | silent | **error** | compile error |
| `map[[]P]int{}` | accepted, failed later | **error at the type** | compile error |
| inspector on an empty `map[P]int` | `map[struct]int` | **`map[P]int`** | — |
| `map[[]map[P]int]string` message | `[]map[struct]int` | **`[]map[P]int`** | — |
| `m[5] = 1` on a `map[P]int` | `as struct` | **`as P`** | — |
| `fmt.Println(m)`, `range`, nested keys, `m[nil]` | unchanged | unchanged | — |

## Guard pass

Nine mutations, all bite:

- key types stop being minted → 14 tests
- `fromKeyStore` stops unwrapping → range, come-back, escape, golden
- `convertTo` drops the key write-check → the collision test
- key types registered in the ELEMENT table → 6 tests
- `Key()` guesses from the shape registry → the nested-elision test
- `typeOf` branches on any struct leaf → the slice-key test
- an incomparable struct mints a key type → the leak test
- `sortMapKeys` stops recognizing minted keys → the ordering test
- the inspector ignores the key type it looked up → the inspector test

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, testcache
cleared. Five tests pinned the OLD behavior and were rewritten with the
reason in the comment: `TestMapKeyAssertionReadsKeys` →
`...UsesTheType` (plus two new nested cases),
`TestStructMapKeysDoNotCollideAcrossTypes` (now four refusals and a
positive), `TestInspectStructKeyedMap`, `TestStructKeyTypeNamesInMessages`,
and one golden line. New: `TestSliceOfStructIsNotAMapKey`,
`TestIncomparableStructMintsNoKeyType`, key coverage inside the canary,
the intern test, the leak-bound test and the escape probe, and two golden
script sections.

`docs/LANGUAGE.md`: the container bullet now claims both leaves and the
nested case, and the re-declaration note says where a struct map KEY lands
on that split.

## Method notes

- **Probe the constraint before designing around it.** The whole key side
  was left open last session because `StructKey` has an unexported method.
  Ten minutes of probing showed the trap is EMBEDDING, and a named field
  sidesteps it entirely — the same lesson as last session's
  `reflect.StructOf` doc, applied deliberately this time.
- **A registry keyed by shape cannot answer a question about identity.**
  `keyOwnerOf` names a struct of the right shape, which is enough to
  RECOGNIZE a type and never enough to BUILD a value whose identity is a
  pointer. The tests found this; the probe could not, because a probe has
  one declaration.
- **Refuse the tempting fallback and pin the refusal.** The nested-elision
  fallback looks like a free improvement and is a silent-miss bug. A test
  that pins an ERROR is what keeps someone from adding it later.
- **Never `git checkout` a file with uncommitted work.** The mutation
  harness restored files that way and reverted a real change mid-pass;
  the pass then reported four SURVIVED that were only build failures.
  Backups, not the index.
- **Check the baseline was taken in the same conditions.** Two benchmark
  sessions on the same machine disagreed by 8 points.

## Open

- **A struct-keyed lookup costs ~+17%** against HEAD, allocations
  unchanged. It is the wrap; `reflect` has no way to build a value of a
  runtime type without `New`. Element containers did not move.
- **A re-declared `P` does not find its own earlier keys.** Unchanged from
  HEAD, now documented in LANGUAGE.md: containers accept across identical
  declarations, keys and `==` do not.
- **`[]map[P]int{{{1}: 2}}` cannot elide the key literal.** The assertion
  and every use are exact; only elision needs a StructType the descriptor
  no longer carries, and guessing one is the bug above.
- **`%T` on a container prints storage**, now for keys too, and more
  verbosely than before.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

~~**An EMPTY `map[P]int` asserts to `map[Q]int`.**~~ Closed here.
~~**`x.([]P)` nested under another container's KEY.**~~ Closed here: the
type carries the key struct at any depth, so only elision is limited.
