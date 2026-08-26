# Session: Round 6 — Every Pin Closed

Session: 314751de-cd4c-48b3-8a3b-29cf1f2533cf
Date: 2026-08-26

## Goal

Round 5 closed Round 2 and left five pinned divergences carried forward.
This round settles all five. Three were bugs with a right answer; two
were decisions the pins explicitly deferred, and the user chose both.

- `a408b6b` — bound the inspector's row WIDTH, not just its row count
- `e814281` — a nested composite literal may elide its type
- `1ba03b5` — a struct assigned to a second name is a second struct
- `cbb0f0a` — a statement-position call aborts on a non-nil error
- `224b81e` — the for-clause variable is per-iteration (Go 1.22)

## Fix 1: half a budget is not a budget

`inspectMaxItems` said "must never dump megabytes" and capped the number
of rows. Nothing capped how wide one row got, so twenty rows of a
megabyte each still cleared the bar it set. Worse, `oneLine` quoted
strings and returned **before** the file's only truncation — so the one
value type a user is most likely to hold a lot of was the one type never
cut.

Two named budgets now, and every rendering path ends in one:

```
inspectMaxWidth  = 60     a row inside a container or struct
inspectMaxString = 2048   a string inspected AS the top-level value
```

The top-level budget is wide because seeing the content is the point of
asking, and finite because a variable holding a fetched page is exactly
the case that scrolls the terminal away. The `(len N)` already printed
beside it makes the elision unambiguous.

`quoteBounded` cuts the **content** and re-quotes, so the closing quote
survives — `"abcdef"...` reads as a string with more after it, where
cutting the quoted form can leave a split escape and an open quote. How
much content fits is *measured* rather than computed, because quoting
expands by a factor that depends on the bytes.

Both cutters are rune-aware. A byte cut landing mid-rune renders U+FFFD,
which reads as corrupted data rather than as elision — the inspector
misdescribing the one value it exists to describe.

`maxWidth` now counts runes, matching what `fmt`'s `%-*s` pads in. The
symptom of counting bytes was **not** misalignment (fmt pads to whatever
number it is handed) but a column wider than any key in it — so the test
asserts the padding is minimal, not merely equal.

## Fix 2: elision falls out of one parameter

`[][]int{{1, 2}}` is legal Go and was rejected. `evalComposite` read the
type off the node it was building, so a literal with a nil `Type` arrived
with nothing to build from and could only refuse.

The construction moved into `compositeOf`, which takes the type as a
**parameter**. That single change is what makes elision work at any
depth: each level hands its element type down, and a literal with no type
of its own is built against what it was handed. The recursion needs no
depth limit — it is bounded by the nesting the parser already accepted.

`elidedElem` is the one-line type switch at every element position, so
anything that is not an untyped literal costs a type assertion and
nothing else.

Struct fields elide too, which is why `declareType` now **keeps** the
type it resolves instead of discarding it once the zero value is taken:
`P{Tags: {"a"}}` has to know `Tags` is a `[]string`.

Two positions still cannot supply a type, both pinned:

- `[]any{{1}}` — an interface element type says nothing about what to
  construct. Go rejects it for the same reason, so the message says that
  rather than repeating the generic one.
- `[]P{{1}}` for a script struct — `typeOf` models only native types, so
  `[]P` does not resolve at all. **Script struct types being
  second-class in TYPE position is the larger gap**; elision inherits it
  rather than causing it.

The map-key path is symmetry, not capability: no composite type can
currently *be* a key (arrays unsupported, slices and maps not
comparable). The comment says so — an earlier draft had a test claiming
to cover it that tested nothing.

## Fix 3: copy on store, never on read

`StructVal` is a pointer, so `b := a` gave two names to one struct.

The rule is copy when a value **enters** a storage location. The
asymmetry with reads is the entire design:

```
b := a         store — b must be a separate struct
xs[0].X = 1    read  — must reach the element in place, as Go does
p.Move()       read  — a pointer receiver must see the instance
```

Copying on read breaks the last two; copying on neither was the bug. The
store sites are a binding (`:=` and `var`), an assignment target of any
of the three kinds, a parameter, a slice or map literal element, and a
range variable — each with its own test, since each is one line that
could go missing without the others noticing.

The copy belongs in `evalArgs` and **not** in `callClosure`, because a
pointer receiver has to share: `callStructMethod` prepends the receiver
*after* `evalArgs` has run, precisely so it bypasses this.

`copyStruct` descends into struct-typed **fields** — part of the value in
Go — and stops at slices, maps and closures, which a Go copy shares too.
That fixed a second bug in passing: the value-receiver copy was a flat
`copy(vals, sv.Vals)`, so a method could reach through a nested struct
field and mutate the instance it was insulated from.

### Two copies that were not doing anything

Both found by breaking the guard and watching nothing fail:

- **`setStructField`** — `setLValue` is its only caller and copies every
  value it writes, so this duplicated on every field write.
- **`structComposite`** — a literal reaches storage through some store,
  and the copy *descends*, so the store that isolates the literal
  isolates its fields with it. A slice or map literal is the opposite
  case and does copy its elements: storing a slice copies the reference
  and stops, so nothing downstream would ever isolate what they alias.

Removing them is safe for a reason the tests then confirm — breaking
`setLValue`'s copy fails the `struct_field` case, and breaking the
descent fails `struct_literal_field`. The reasoning and the coverage
point at the same two lines.

### Cost

The loop shapes are unmoved and allocate byte-for-byte what they did
before: the type assertion is free when no struct is involved.
`BenchmarkStructCopy` prices the shape it is not free at:

| shape | ns/iter | allocs | |
|---|---|---|---|
| scalar | 279 | 11758 | the loop, before any copy |
| flat | 334 | 15772 | one copy per store |
| nested | 390 | 19812 | the copy descends one level |

## Fix 4: the inconsistency was with grsh's own rule

`mayFail()` succeeded silently while `v := mayFail()` on the same
function aborted. Go discards the error in **both** places — but Go also
does not abort on the assignment form, and grsh does, deliberately.
Adding a variable to a call should not be what decides whether a failure
is noticed.

```
mayFail()          aborts
v := mayFail()     aborts
_ = mayFail()      ignored — naming it is the opt-out
err := mayFail()   binds err, the script decides
```

Only the last result is inspected, so `error` and `(T, error)` behave
alike.

`$(...)` stays exempt for the reason it always was: a non-zero exit is
data there, reported through `status()`. That exemption **is** reachable,
though reaching it takes some doing — a bare `$(...)` on its own line
classifies as a SHELL line and never becomes a `__capture` call at all,
even nested inside a Go block. It has to sit in a line the classifier has
already called Go:

```
for i := 0; i < 1; i++ { $(false) }
```

which is what the new golden script does. The golden covers the shapes
that *continue*; the abort lives in `errors_test.go`, where the reported
position can be checked too — the golden harness only tolerates an
`ExitErr` and this is an ordinary positioned error.

The cost of the rule: a bare `errors.New("x")`, which does nothing either
way, now aborts. Nothing can tell that apart from a call that failed.

## Fix 5: post moves to the top, and that is the mechanism

The clause variable is per-iteration now, so range and the for-clause
agree. `0 1 2`, not `3 3 3`.

The post statement runs at the **top** of every iteration but the first.
That is not a rearrangement — it is the whole thing. An `i++` at the
bottom advances the cell the iteration's own closures just captured, and
prints `1 2 3` where Go prints `0 1 2`.

```
holder ─copy─> iter ─post─> ─cond─> ─body─> ─copy back─> holder
                 ^
                 the cell closures capture; never touched
                 again once the body returns
```

On the shared-cell path the two orders are indistinguishable — cond,
body, post either way — so the restructuring is free where it does not
apply. `continue` reaches the copy back; break and return skip it, which
is correct because the clause variable does not outlive the loop.

Only a `:=` clause gets copies, matching Go: `for i = 0; ...` assigns to
a binding the loop does not own.

### The gate, and why it is sound

The copy is taken only when a closure could observe it. A `*ast.FuncLit`
anywhere in the ForStmt is the only way a cell can outlive its iteration,
and that is a **property of this interpreter**, not a guess: values copy
into containers (structs included, as of this round), only a `Closure`
retains an `*Env`, and grsh has no address-of operator, so there is no
`&i` to smuggle a cell out.

The scan covers cond and post as well as the body. Artificial — every
natural place to build a closure is the body — but a closure built in a
condition captures exactly as one in a body does, and scanning only the
body silently fell back to one shared cell there. It has a test now,
because the first guard pass reported MISSED on exactly that line.

So the four existing loop shapes are unmoved and
`TestLoopAllocationShape` passes at its old counts. The shape that pays
is new and priced:

```
closure-in-body   19 -> 23 allocs/iter,  423 -> 520 ns/iter
```

The two-sided allocation bound earned its keep: run against the old
`evalFor` it printed *"fell to 19 from 23"*, which is exactly the message
it exists to print.

## Method notes

- **A guard harness that does not verify its own edit reports a test gap
  that isn't there.** Two "MISSED" results in the first round were python
  heredoc escaping silently failing to match the anchor — a no-op edit
  reads identically to a test that does not bite. Every guard run now
  asserts the file actually changed before running the tests. This is the
  same lesson as Round 5's `grep FAIL:` hiding a build error, in a new
  costume: **the harness has to fail loudly too, not just the code.**
- **A guard harness that restores only on success poisons everything
  after it.** One break made a loop never terminate; the two-minute
  command timeout killed the run mid-case, leaving the mutation in the
  tree. `go build` passed (the mutation compiled), so the damage was
  invisible, and the *next* three guard results were measured against
  broken code — reported as hangs, which read as harness flakiness rather
  than as a dirty tree. Restore is in a `finally` now, and the harness
  hashes the file before and after the whole run.
- **A "MISSED" is a claim about the tests, and it is worth one minute of
  checking which.** Of six misses this session: two were bad mutations,
  one was a bad test, one was a genuinely untested reachable path
  (`$(...)` in statement position), one was a redundant copy that
  *should* be deleted, and one was a real gap in the capture scan. Only
  two of the six meant "write a better test."
- **Reasoning that a line is redundant, and testing that it is, are
  different claims — get both.** Two copies came out on the argument that
  the store already covers them. The argument is only trustworthy because
  breaking the *upstream* copy fails the same tests the removed line
  would have.
- **Guards broken, both ways:** string rows unbounded, byte cuts, top-level
  string unbounded, byte-counted padding, struct/closure rows unbounded,
  elision refused, elements/fields not inheriting, `FieldTypes` nil'd,
  non-composite element accepted, every one of the ten store sites
  un-copied, flat copy, over-copy, receiver copied for pointer receivers,
  statement errors discarded, capture un-exempted, first result inspected,
  per-iteration copy removed, gate removed, post at the bottom, copy-back
  dropped, assign-form copied, body-only scan, first var only. Every one
  failed the test it was supposed to.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green after each
commit, including the 91 golden scripts (92 now) and the pty e2e. Loop
and struct benchmarks A/B'd at n=5 for the numbers above. Real binary
smoke-tested on a script mixing all five changes plus shell lines.

## Open

- **Every pinned divergence is now closed.** Rounds 2 and 6 both fully
  landed; nothing is carried as "known and unfixed."
- **Script struct types are second-class in TYPE position.** `[]P`,
  `map[string]P`, a struct-typed field's zero value — none resolve,
  because `typeOf` models only native types. This surfaced twice this
  round (elision `[]P{{1}}`, and `FieldTypes` being nil for a nested
  struct field) and is the largest single gap left in the interpreter.
  It is a feature, not a divergence, so it is not a pin.
- **`absorb` still allocates two fresh slices per accepted command**
  (205KB at 10k units, ~35us). Deliberately **not** fixed: the only way
  to remove it is double-buffering the index, which doubles resident
  memory to save an allocation that happens once per command, off the
  keystroke path, in the shadow of a process spawn. Recording the
  judgment rather than the churn.
- The `--explain` hint lane and the ghost index from Round 5 are
  untouched by this round.
