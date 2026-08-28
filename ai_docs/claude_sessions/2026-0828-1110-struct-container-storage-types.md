# Session: Per-Struct Container Storage Types

Session: a8e49dea-6ffb-4f67-a24f-44d01ae4d829
Date: 2026-08-28

## Goal

Close two items the last two sessions left Open:

- `m[missing]` on a struct-valued map yields nil, not the zero struct.
- `x.([]P)` cannot distinguish `[]P` from `[]Q`.

They looked like two problems. They were one.

## The one fact that was missing

Every script struct erases to `*StructVal`, so a container of structs is
a container of that one type. The erasure is invisible while a value is
in hand — a `*StructVal` knows its `StructType` — and expensive the
moment it is NOT:

```
m["missing"]    a miss has no value to read the type off, so the zero
                could only be nil
x.([]P)         []P and []Q were the SAME reflect.Type
append(xs, q)   a Q dropped into a []P with no complaint  (found, not sought)
```

The third row was not on the list and is the same bug. Naming the shared
cause — **the container does not know what it holds** — is what turned
two separate patches into one change.

## First answer, shipped and then superseded

The assertion is answerable from the CONTENTS: a `*StructVal` carries its
`StructType`, so walk the value against the descriptor and reject a
container holding the wrong struct. That shipped first, with tests and
four guard mutations, and it is a real improvement over `AssignableTo`.

What it could not do is decide an EMPTY container, and it could not touch
the map miss at all — a slot with no value in it has nothing to ask. So
the walk was the best answer available from values, and the map miss
proved values were not enough.

## The blocker that was not one

The map miss needs the TYPE to name the struct, which needs a distinct
`reflect.Type` per struct. Five candidate ways to mint one were rejected
in a row for the same reason: a runtime-minted type has no methods, so a
`[]P` would print as grsh's storage through `fmt`, `json`, and every Go
library the value reaches. `reflect.StructOf`'s own doc says it "does not
generate wrapper methods for embedded fields", which read as the end of
it.

It is not. `StructOf` DOES promote an **embedded** field's methods:

```go
reflect.StructOf([]reflect.StructField{{
    Name: "ScriptStruct", Type: carrierType, Anonymous: true,
    Tag:  reflect.StructTag(`grsh:"P|X:int"`),
}})
```

`NumMethod() == 1`, `Implements(fmt.Stringer)`, and a `[]P` prints as
`[P{X: 1}]`. The whole design turns on this, and it was one probe away
the entire time. **A documented limitation is worth ten minutes of
probing before it is accepted as a wall.**

## The trap behind the door

The first working version embedded `*StructVal` directly. It printed the
map miss correctly and then killed the process on the next line:

```
fatal error: runtime: type offset base pointer out of range
```

from `itabInit`, the first time `fmt` asserted the value to an interface.
Not a panic — no `recover` reaches it.

Bisected to a one-line cause: **promotion breaks when the embedded type
has an UNEXPORTED method**, and `*StructVal` has `copyStruct`. (Only when
the embedded type is from another package, which every runtime-minted
type is. A `main`-package probe passed and hid it.)

Hence `ScriptStruct`: one field, one exported method, embeds nothing so
it cannot inherit one, and no other job. Keeping `*StructVal` free of
unexported methods forever is not a promise this package can make;
keeping a purpose-built two-line type free of them is.

`TestMintedTypePromotesExactlyOneMethod` is the canary, and it checks the
CAUSE before the effect: promotion pulls an unexported method in as a
second method, so the count fails cleanly where the formatting check
would take the test binary down with it.

## Interning: what bounds the leak

`reflect.StructOf` never frees, and `declareType` runs on every EXECUTION
of its statement — a `type P` inside a loop makes a StructType per
iteration. Keying the tag on those would mint a type per iteration
forever.

So the tag is the struct's SHAPE: name, field names, and each field's
resolved type, with a nested struct spelled out rather than named
(`Out|I:{In|N:int}`) so two `Out`s over different `In`s do not collide.
The table is bounded by distinct struct shapes in the SOURCE.

The price, stated where it lands: two identical declarations of the same
name share a storage type, so a `[]P` from one accepts a `P` from the
other. `p.(P)` and `p == q` still tell them apart, since those compare
StructTypes. `TestRepeatedDeclarationMintsOnce` pins the bound — 50
executions, one mint.

Because interning is by shape, `convertTo` checks `sv.Type.storeT != pt`
rather than comparing StructTypes. The write check and the intern rule
then draw the same line by construction and cannot drift.

## The boundary

Two functions, and the invariant that a minted value never reaches a
script:

- **in** — `convertTo`. Every write into a slot already routed through it
  to reach the element type: literal, append, index-assign, map-assign,
  make's fill, argument conversion. One case covers all of them, and it
  is where a `[]P` stops accepting a `Q`.
- **out** — `fromStore`, at seven read sites: index, map read, comma-ok,
  both range legs, the inspector's two, and `convertTo`'s own slice loop.

`fromStore` leads with a Kind check, so every scalar, pointer, slice and
map leaves before the lookup; only struct-kind values pay it, and in a
container those are almost always ours. The owner table is copy-on-write
behind an `atomic.Pointer` — written once per shape, read on every
element.

`TestMintedValuesDoNotEscapeToScripts` probes all seven paths with `%T`,
which is the sharpest instrument available: it is the one place the
storage is visible to a script at all.

## What is still answered by value

A struct map KEY is not minted. A key must be Go-comparable with
field-wise equality, which is what `StructKey` is for, and every script
struct becomes that one type — so `map[P]int` and `map[Q]int` ARE one
`reflect.Type`. The element walk from the first pass survives here,
narrowed to keys as `keysMatch`.

Minting keys too would need a second carrier (`StructKey` has an
unexported `structVal` method — the same trap) plus changes to the key
encode/decode/sort paths, and would buy only the empty-map assertion.
Not taken; the split is documented rather than hidden.

## Behavior

| case | before | now | Go |
|---|---|---|---|
| `m["miss"]` on `map[string]P` | `nil` | **`P{X: 0}`** | zero struct |
| `deep["o"]["miss"]` nested | `nil` | **`P{X: 0}`** | zero struct |
| `m["miss"]` on `map[string][]P` | nil slice | nil slice | nil slice |
| `x.([]P)` on a `[]Q` | true | **false** | false |
| same, empty container | true | **false** | false |
| `x.(map[string]P)` on a `map[string]Q` | true | **false** | false |
| `x.([][]P)` on a `[][]Q` | true | **false** | false |
| `append(xs, Q{})` onto `[]P` | worked | **error** | compile error |
| `xs[0] = Q{}` on `[]P` | worked | **error** | compile error |
| `append(xs, nil)` onto `[]P` | worked | worked | typed nil |
| `x.(map[P]int)` on a `map[Q]int` | true | **false** | false |
| same, EMPTY map | true | true | false |
| inspector on an empty `[]Job` | `[]struct` | **`[]Job`** | — |
| `fmt.Println(xs)` | `[P{X: 1}]` | `[P{X: 1}]` | — |

The empty `map[Q]int` row is the one remaining divergence, and it is the
key leaf above.

## Guard pass

Seven mutations, all bite:

- slices stop minting → assertion, inspector and escape tests
- `zeroInSlot` stops repairing → the map-miss tests
- `fromStore` stops unwrapping → six of seven escape paths
- slot identity check removed → all five rejection routes
- signature ignores field types → the intern test
- `keysMatch` always accepts → the key assertion tests
- carrier no longer embedded → the promotion canary

The first sweep of M1 read as under-caught; the grep was truncating, not
the mutation surviving. **Check the filter before believing a guard did
not bite.**

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, testcache
cleared. New `store_test.go` (canary, intern-by-shape, leak bound, escape
probe), rewritten container-assertion tests, new golden
`testdata/scripts/struct_container_types.grsh`.

Four tests pinned the OLD behavior and were rewritten to the corrected
expectation, each with the reason in the comment:
`TestStructMapMissYieldsNil` → `...YieldsZeroStruct`, `TestStructMapKeys`,
`TestInspectNamesScriptStructContainers`, and the `[]P`/`[]Q` divergence
inside `TestTypeAssertionOnScriptStruct`.

New `BenchmarkStructContainer` prices the read boundary against native
controls: +1.1% slice index, +3.3% map hit, +2.7% range. Nothing else
moved; the pre-existing benchmarks came back inside noise. Its
`map-miss-struct` shape can only run on this tree — at HEAD the miss is
nil and `.X` on it errors.

`docs/LANGUAGE.md`: the map-miss limit removed, a container-knows-its-
struct bullet added, and two new Semantics notes — `%T` shows storage,
and identical re-declarations share a container type.

## Method notes

- **Name the shared cause before patching either symptom.** Two Open
  items, one missing fact. Fixing them separately would have produced the
  content walk twice and the store hole never.
- **Probe a documented limitation before accepting it.** "StructOf does
  not generate wrapper methods for embedded fields" is what made the
  whole design look impossible for five rejected candidates. Ten minutes
  of probing showed it does. Reading beat recalling, and probing beat
  reading.
- **A probe that is not faithful can hide the trap.** The first probe put
  the embedded type in `main` and passed. The crash needs a cross-package
  embedded type — which every runtime-minted type has. Reproduce the real
  shape, including package boundaries.
- **Ask when the fork is real.** The mint changes the storage model and
  turns silent stores into errors; leaving the miss as nil was a
  legitimate call. That is the user's, not a default to assume — and the
  interning question had a leak on one side of it that the user should
  see before it ships.
- **Guard the cause, not the effect,** when the effect is a process kill.
  A test that dies takes the run's output with it.

## Open

- **An EMPTY `map[P]int` asserts to `map[Q]int`.** The last leaf still
  answered by value; a non-empty one is exact. Minting keys would close
  it and needs a second carrier.
- **`%T` on a container prints storage**, and prints it more verbosely
  than before (the signature is in the tag). Unfixable without
  intercepting `fmt`; every grsh-produced message names the script's own
  type.
- **`x.([]P)` where `[]P` is nested under another container's KEY** —
  `[]map[P]int` — still cannot see the key, because `Elem` drops KT.
  Unchanged, and the same limit elision has.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.

~~**`m[missing]` on a struct-valued map yields nil.**~~ Closed here.
~~**`x.([]P)` cannot distinguish erased element types.**~~ Closed here,
except the key leaf above.
