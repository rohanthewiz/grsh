# Session: Script Struct Types in TYPE Position

Session: c61c3cac-9d06-402d-8010-761472c1f492
Date: 2026-08-28

## Goal

Round 6 closed every pinned divergence and left one feature gap named as
the largest remaining: **script struct types were second-class in TYPE
position**. `[]P`, `map[string]P`, a struct-typed field's zero — none
resolved, because `typeOf` modelled only native types. Elision inherited
the gap rather than causing it (`[]P{{1}}` was rejected with "unknown
type P").

This session closes it.

- `c1b2574` — Script struct types work in TYPE position

## The obstacle: reflect cannot mint a named type

Everything in the interpreter's type machinery is a `reflect.Type`.
`typeOf` returned one, `compositeOf` descended one, `make` switched on
one. A script struct has no `reflect.Type` — `reflect.StructOf` builds
anonymous structs, and there is no runtime equivalent of a type
declaration.

Four designs were weighed:

| approach | why not |
|---|---|
| `reflect.StructOf` per script struct | changes the runtime value from `*StructVal`; every type assertion in the interpreter breaks |
| distinct type per struct via a struct TAG | values become wrapper structs; same breakage |
| wrap containers (`StructSlice{Elem, V}`) | every reflect-generic path — len, index, range, append, slice, print — needs a special case; fights the design where reflect IS the runtime |
| **erase to `*StructVal`, carry identity beside it** | chosen |

So: **`[]P` is stored as `[]*StructVal`.** Every script struct erases to
the same storage type, and the identity travels in a companion field.

## TypeDesc, and why one leaf is enough

```go
type TypeDesc struct {
	RT reflect.Type // storage type
	ST *StructType  // the script struct at RT's leaf, if any
}
```

```
P                 RT *StructVal               ST P
[]P               RT []*StructVal             ST P
map[string][]P    RT map[string][]*StructVal  ST P
[]int             RT []int                    ST nil
```

`Elem()` and `Key()` carry `ST` down **unchanged**, and it is consulted
only where `RT` bottoms out at `*StructVal` — which is exactly what
`IsStruct()` tests. So `map[string][]P`'s key descriptor holds a
meaningless `ST` and nobody ever asks.

One leaf suffices, and that is **enforced, not hoped for**: `typeOf`
refuses a script struct in map-KEY position, so the element and value
edges can never fork toward two different structs. The refusal is not
bureaucratic — erasure makes the key a *pointer*, and a pointer is
comparable, so `map[P]V` would build happily and then compare identities.
Every lookup with a freshly built key would miss. There is no struct
equality to build on yet, so refusing is the only honest answer.

The descriptor is two words and travels by value. Resolving a type
allocates nothing, so the composite-literal path costs what it cost when
it was a bare `reflect.Type` — which the unmoved loop benchmarks confirm.
An earlier draft carried `elem`/`key` pointers and needed an interning
cache to stay allocation-free; collapsing to the single-leaf invariant
deleted the cache and its unbounded-growth question with it.

## Elision falls out, again

Round 6 moved composite construction into `compositeOf`, which takes the
type as a **parameter**. That is still what does the work here:

```go
if d.IsStruct() {
	return in.structComposite(env, d.ST, n)
}
switch d.RT.Kind() { ... }
```

`elidedElem` hands the element descriptor down without caring which kind
it is, so `[]P{{1, 2}}` and `Order{Head: {"crate", 1}}` build with no new
mechanism. The one-parameter change from last round paid for itself a
second time.

## Two places needed more than passing the descriptor along

### make FILLS

`reflect.MakeSlice` on `[]*StructVal` leaves nil pointers. Go's zero for
a struct element is a **struct**, so `make([]P, 3)` would hand back three
nils that panic on the first field access.

`make([][]P, 3)` is deliberately NOT this case: its element is a slice,
and a nil slice IS Go's zero.

### newZero DUPLICATES

A struct-typed field resolves now, so `type Out struct { I In }` zeroes
to a real `In{}` — one `*StructVal` held by the TYPE, in `t.Zero`.
Copying the slice alone would hand every instance that same nested
struct.

**This is the line the guard pass caught.** Breaking it, the obvious test
(`a := Out{}; b := Out{}; a.I.N = 7`) still passed — because `:=` is a
store site, and `copyOnStore` descends and isolates the nested struct
anyway. Almost every caller of `newZero` is covered that way.

`make`'s fill is the one that is not: it writes each instance **straight
into the slice**, with no store in between.

```
make([]Out, 2)      without the copy:  xs[0].I.N = 7  ->  7 7
                    with it:                              7 0
```

So the test that covers it goes through `make`, and the comment names
that caller specifically. A guard reporting MISSED was worth one minute
of checking which — the same lesson as last round, and again the answer
was neither "bad mutation" nor "write a better test" but "the test you
have is testing a different line."

## Two divergences, pinned

**`m[missing]` on a struct-valued map yields `nil`**, where Go yields the
zero struct. The erased element type cannot say WHICH struct, so
`reflect.Zero` would produce a typed nil that panics on the first field
access. Untyped nil is the honest answer: it prints as `<nil>`, compares
equal to `nil`, and the comma-ok form is exact either way.

**`x.([]P)` cannot tell a `[]P` from a `[]Q`** — both are
`[]*StructVal`, and the slice carries no per-element promise. `x.(P)`
itself **is** exact: it compares the declared type, not the erased one,
or every struct would satisfy every assertion.

## Fallout the erasure creates

A typed nil `*StructVal` is reachable for the first time —
`append(xs, nil)` converts nil to a typed nil element. Five methods that
could not previously receive one now guard and report instead of
dereferencing: `String`, `copyOnStore`, `structField`, `setStructField`,
`callStructMethod`.

The inspector needed `displayType`. `?xs` on a `[]P` would otherwise
print `[]*interp.StructVal` — grsh's own internals, at the one surface
whose entire job is describing a value. The name is read off an ELEMENT,
since every instance carries its `StructType`; an empty container is the
only case where nothing on the value knows, and it falls back to the
neutral word `struct`. Error messages get the same treatment through
`scriptTypeName`, which has no element to read and says "a struct".

## Cost

Every loop shape allocates **byte-for-byte** what it did, and
`BenchmarkStructCopy` is unmoved on scalar and flat. The nested shape
*fell* by 22 allocs/run — declaring `type Out struct { I In }` no longer
builds and discards a positioned error for the field it could not
resolve.

The shape that pays is new and priced. `BenchmarkStructZero` isolates a
literal of a struct with a struct FIELD, where `newZero` now builds a
nested zero the literal may immediately overwrite — which is Go's own
order (zero the struct, then assign the fields), and what makes
`var o Out` have a usable `o.I` at all:

| shape | allocs/run before | after | |
|---|---|---|---|
| flat | 12774 | 12774 | control: a struct of scalars |
| nested | 17814 | 19792 | +2 per literal, ~2% wall clock |

## Fixed in passing

`map[[]int]int{}` reached `reflect.MapOf`, which panics on a
non-comparable key type; the top-level recover turned that into an
unpositioned **"grsh internal error"**. It is an ordinary positioned
script error now. The `Comparable()` check does NOT subsume the script
struct refusal above — `*StructVal` is comparable — which is why both
checks exist.

## Method notes

- **A MISSED guard is a claim about which line a test covers.** The
  `newZero` copy looked untested; it was covered-by-accident at every
  store site and genuinely uncovered at the one caller that bypasses
  them. Reasoning about redundancy and testing for it disagreed, and the
  disagreement was the finding.
- **Collapsing an invariant beats caching around it.** The recursive
  descriptor needed interning to stay allocation-free, and interning
  needed a growth bound. Proving that a script struct can only ever sit
  at ONE leaf — by refusing the position that would break it — reduced
  the descriptor to two words and deleted both problems.
- **The harness discipline held.** Restore in a `finally`, hash the tree
  before and after the whole run, and assert the edit actually applied
  before running the tests. Fourteen guards, one restore check, no dirty
  tree.
- **Guards broken:** make's fill removed, `newZero`'s duplication
  removed, the map-key struct refusal removed, the `Comparable` check
  removed, the type assertion falling back to `AssignableTo`, the
  struct-map miss returning a typed nil, `copyOnStore`'s nil guard,
  `structField`'s nil guard, `elidedElem` refusing a struct element,
  `compositeOf` not routing a struct, `ST` dropped at a slice level,
  `ST` dropped at a map level, the inspector printing the erasure, and
  `var` taking reflect's zero instead of the type's. Every one bit.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green, including the 94
golden scripts (a new `struct_types.grsh`) and the pty e2e. Benchmarks
A/B'd at n=5 by stashing the tree. Real binary smoke-tested on a script
mixing `$(...)`, a shell line, a struct-typed field, `[]P`, `map[string]P`
and `append`.

## Open

- **Struct EQUALITY.** `p == q` compares identities, and it is why a
  script struct cannot be a map key. Comparing field-wise would make both
  work and is the natural next step from here.
- **`x.([]P)` cannot distinguish `[]P` from `[]Q`.** A consequence of
  erasure at one level up; `x.(P)` is exact. Pinned with a test.
- **`m[missing]` on a struct-valued map yields nil**, not the zero
  struct. Pinned with a test.
- Still unsupported in type position: pointer types (`*P` beyond method
  receivers), qualified types (`time.Duration`), fixed-size arrays. None
  of these are struct-specific.
- **`absorb` still allocates two fresh slices per accepted command** —
  unchanged from Round 6, and still deliberately not fixed.
- The `--explain` hint lane and the ghost index from Round 5 remain
  untouched.
