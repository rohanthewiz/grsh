# Session: Struct Map Keys

Session: f09f8e39-647c-4189-b4b6-d8eb5a6dbb12
Date: 2026-08-28

## Goal

Two features in one session, the second following from the first.

- `75f831f` — Struct equality compares fields, not identities
- `e957c80` — Struct map keys, via a comparable stand-in

The equality work is written up in
`2026-0828-0436-struct-equality-field-wise.md`; this doc covers map keys,
which that session left open.

## The premise the last session got wrong, twice

Round 7 said field-wise equality "would make both work" — `==` and map
keys. The equality session corrected that: the interpreter owns the `==`
operator but it does not own the MAP. `reflect.Map` hashes and compares
keys with Go's own runtime equality, which this package cannot reach
into, so a map keyed on the erased `*StructVal` compares POINTERS however
clever `==` gets.

That correction is what pointed at the actual design. The fix is not to
teach the map about structs. It is to **hand the map a key whose
Go-native equality is already the answer we want.**

## StructKey

```go
type StructKey struct {
	T *StructType
	F any // [len(T.Fields)]any
}
```

`T` keeps `P{1}` and `Q{1}` from colliding. `F` holds the field values,
which Go compares element-wise and recursively — exactly what
`structEqual` does by hand, except the runtime does it. A struct-typed
FIELD encodes to its own `StructKey`, or it would be compared by identity
again one level down.

**An array, not an encoded string.** The key has to travel BACK: `range
m` yields keys, and the script must get its `P`, not grsh's storage.
Holding the values as themselves makes the return trip a copy instead of
a parse — so there is no decoder that could disagree with the encoder,
and no field's int-or-rune ambiguity to resolve. The rebuilt struct is
fresh per iteration, which is what makes `for k := range m { k.X = 9 }`
unable to corrupt the map's hashing.

Alternatives weighed:

| approach | why not |
|---|---|
| intern a canonical `*StructVal` per value | needs a hashable encoding ANYWAY, plus a table, plus an unbounded leak |
| a distinct reflect type per struct (StructOf + tag) | elision would work at any depth, but reflect cannot attach `String()`, so every printing path breaks |
| encode to a string, decode by parsing | the decoder is a second source of truth that can drift |
| **StructKey + array, decode by copy** | chosen |

The interning row is the one that settles it: interning needs the
encoder, so it is strictly the encoder plus a leak.

## One hook in, two out

Encoding needed exactly ONE site, and that was not luck — every key
crossing in the interpreter already routed through `convertTo` to reach
the map's key type:

```
m[k]            evalIndex
v, ok := m[k]   the comma-ok read
m[k] = v        setIndexed
delete(m, k)    the builtin
map[P]V{k: v}   the composite literal
```

Decoding needed two: `range`, and the nested field inside a rebuilt key.
The second only shows up when a key HAS a struct field and the script
reaches through it — which is why the guard pass found it and the first
round of tests did not.

## TypeDesc pays back the word it saved

Round 7's descriptor was two words, and its single-leaf invariant held
for one specific reason: **a script struct was refused in map-KEY
position**, so the element and key edges could never fork. This feature
removes that refusal, so it owes the word back.

```go
type TypeDesc struct {
	RT reflect.Type
	ST *StructType // the struct at RT's element leaf
	KT *StructType // the struct at RT's key leaf
}
```

```
map[P]int         RT map[StructKey]int          ST nil  KT P
map[string][]P    RT map[string][]*StructVal    ST P    KT nil
map[P]Q           RT map[StructKey]*StructVal   ST Q    KT P
```

TWO leaves is still bounded rather than the start of a general tree, and
typeOf enforces it: a key must be comparable, which rules out a map or
slice inside a key, so a key edge is one hop and never leads to another.
A struct NESTED as a field of a key is not a leaf here at all — the VALUE
encoder handles it, not the type descriptor.

`Key()` hands KT down as the key descriptor's ST, and `IsStruct()` accepts
both storage types. That is what makes construction still route through
`structComposite`: the script builds an ordinary `*StructVal` for the key
position and `convertTo` encodes it at the map boundary. The script never
sees a `StructKey`.

## What Elem() dropping KT costs

`Elem()` drops KT, because whatever sits at the element edge has its own
key edge or none. A map nested inside another container therefore knows
its key is a struct but not WHICH, and only ELISION is lost:

```
[]map[P]int{{P{1}: 2}}     works
[]map[P]int{{{1}: 2}}      "a struct map key must name its type here"
```

The generic message would have said "interp.StructKey is not a composite
type", which is both a leak and a lie — a struct IS a composite type. It
gets a targeted message and a hint instead.

## Pinned divergences

- **nil is a usable key.** Go rejects `m[nil]` for a struct key type.
  grsh has real typed-nil structs, so the zero `StructKey` is their honest
  encoding — and both routes must agree, since `m[nil]` is answered by
  `convertTo`'s zero while `m[xs[0]]` reaches the encoder.
- **grsh sorts struct keys** on their rendered form when ranging. The
  `*StructType` POINTER inside the key would otherwise order them
  differently every run. `fmt`'s own map printing sorts by its own rule,
  so the two orders can differ — same as for string keys already.
- **`[]map[P]int` cannot elide the key literal**, above.

## Refusals stay in one place

`typeOf` refuses a key type Go refuses by reusing `noCmp` — the same
verdict `==` uses — so the two answers cannot drift apart:

```
invalid map key type P: field Tags has type []string
```

The `any` field is the gap noCmp cannot close, being statically
comparable and dynamically anything. That is Go's own runtime-panic case,
and `keyValue` reports instead of letting the map panic while hashing.

## Cost

Allocation COUNTS unmoved on every loop and struct benchmark. Byte totals
move 24–48 bytes on ~400KB — the third `TypeDesc` word in each declared
type's `FieldTypes`, once per declaration. Wall clock within ±0.8% at
n=5. Binary +17KB, no new dependencies.

## Method notes

- **A guard pass is a map of what your tests actually reach.** Six of 22
  reported MISSED or BUILD-FAILED, and every one named a real gap:
  `structKeyOf`'s nil branch is reachable ONLY through a typed nil,
  because `m[nil]` is answered by `convertTo`'s zero and never calls the
  encoder; the nested decode needs a range over a key that HAS a struct
  field; three type-rendering paths had no assertion at all. The third
  session running where MISSED meant "your test covers a different line."
- **Two BUILD-FAILED results were bad mutations, not covered code.**
  Leaving a loop variable unused proves nothing. A guard that does not
  compile has to be rewritten, not scored.
- **Measure the obvious dependency before taking it.** Carried over from
  the equality session and paid again: the instinct to reach for a
  stdlib helper is worth one `go build`.
- **`git stash` for an A/B benchmark is a hazard.** A 2-minute tool
  timeout fired while the tree was stashed, leaving the working tree at
  baseline and the work in `stash@{0}`. Recovered with `git stash pop`,
  but a worktree would have made the failure mode impossible. Do that
  next time.
- **Guards broken:** typeOf's key swap, its incomparable refusal, KT on
  the map descriptor, convertTo's encode, the nested-key message, Key()
  carrying KT, IsStruct's second storage type, String()'s KT replacement,
  its leftover-erasure fallback, scriptTypeName's second erasure,
  structKeyOf's field encoding, its type field, its nil branch,
  keyValue's recursion, its comparability check, fromKeyValue,
  structVal's decode at depth, StructKey.String, range's decode,
  sortMapKeys' struct branch, the inspector's key name, and
  declareType's keyArr. Twenty-two guards, all bite; tree hashed before
  and after, restored clean.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, including 49
golden scripts (a new `struct_map_keys.grsh`) and the pty e2e. Print and
range ordering confirmed deterministic across six separate processes,
including a map with heterogeneous `any`-typed keys. Real binary
smoke-tested on a script mixing `$(...)`, a shell line, a nested loop
building struct keys, key snapshotting, and `range`.

`docs/LANGUAGE.md` updated for both features.

## Open

- **`var n map[string]int; n == nil` answers false**, and so does a nil
  slice. Confirmed against baseline as PRE-EXISTING and unrelated to
  either feature this session — `safeEqual` sees a non-nil interface
  holding a nil map. Deliberately not folded in; it is its own fix,
  touching every reference type.
- **`x.([]P)` cannot distinguish `[]P` from `[]Q`**, and `map[P]int` and
  `map[Q]int` are now the same erased type for the same reason. Contents
  never collide, since the encoded key carries the `*StructType`.
- **`m[missing]` on a struct-valued map yields nil**, not the zero
  struct. Unchanged, pinned.
- **An `Equal` method is not consulted** by `==`. Go does not either.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays. The
  map-literal key path is now genuinely exercised, so arrays would slot
  into a route that works.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged since Round 6, still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.
