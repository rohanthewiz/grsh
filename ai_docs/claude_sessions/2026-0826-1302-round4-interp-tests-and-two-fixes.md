# Session: Round 4 — A Safety Net for internal/interp, and the Two Fixes It Unblocked

Session: b5a87343-fa7a-4f9a-aa46-9ca54829dbe0
Date: 2026-08-26

## Goal

Open Round 4 by writing the unit tests `internal/interp` never had. The
env double-wrap fix had been deferred across two sessions for exactly one
reason — the package had no coverage of its own, only the indirect reach
of the golden scripts, and the change alters evaluation scoping.

Three commits, in the order the net made possible:

- `be626c9` — 272 tests, no production code touched
- `9485866` — collapse the redundant Env wraps
- `8f28f3e` — render the call chain once, not once per frame

## The harness

Tests drive `Interp.Run` **directly**: source wrapped in the `__main` func
transform would emit, parsed with `go/parser`. Neither classify nor
transform is in the loop, so a failure here is an interpreter failure and
nothing else — which is the point of having them alongside the golden
scripts, where every stage is implicated at once.

Go functions are injected per test (`eval(t, src, extra)`), which is what
lets the reflect boundary be driven with signatures a test controls: a
`(T, error)` returner, a panicker, a `[]string` taker.

The shell legs are deliberately absent — they need a populated side table
and a real process, and they already have coverage elsewhere.

## scope_test.go — the file that mattered

Every construct that introduces a scope wraps the current Env, and several
wrapped TWICE. The fix is obvious; the risk is that the OBSERVABLE scoping
moves with it. These pin what a wrap-removal would perturb: what `:=`
shadows, how long a binding lives, and which cell a closure captures,
across block / for / range / if / switch / closure.

Two divergences from Go are pinned **deliberately**, flagged as tripwires
rather than silently encoded:

| | grsh | real Go |
|---|---|---|
| for-clause closures | `3 3 3` (one shared cell) | `0 1 2` (per-iteration, 1.22+) |
| range closures | `10 20 30` (per-iteration) | same |

The asymmetry is real. Pinning both is what makes it visible; whichever
way it is settled, it should be settled on purpose.

## The allocation guard, and why it is two-sided

`TestLoopAllocationShape` measures the **marginal** per-iteration
allocation by differencing two trip counts.

Both counts sit above 255, and that is not incidental. The interpreter
boxes every int into a `Value`, and Go serves 0..255 from a shared table
without allocating — so a low trip count spends most of its iterations in
the free range and the difference reports the boxing cliff rather than the
scope cost. A first attempt with lo=200 gave a consistent, puzzling +1.15
offset; that was the cliff. With lo already past it, the 256 cheap
iterations appear in both runs and cancel, and the result is a clean
integer.

**This also means the naive figure was wrong.** The 13 and 22 recorded in
earlier sessions came from allocs-per-run ÷ trip count. The true marginal
is 14 and 23.

The bound is two-sided on purpose. The upper side is the ordinary
regression guard. The **lower** side exists so an improvement cannot land
silently: it forces the new baseline into the commit that earns it, next
to the scoping cases that say the semantics did not move.

It did that job the same day.

## Fix 1: collapse the redundant Env wraps

Four constructs built a scope nothing could ever be declared into:

- **if / for / switch init scopes**, built unconditionally. They exist only
  to hold `if v := f(); ...`; a condition cannot declare, so with no init
  the enclosing scope serves.
- **if and for then wrapped the body again.** `Body` is always a
  `BlockStmt` (`Else` a block or another if — go/parser admits nothing
  else) and each opens a scope on the way in. In `for`, that redundant wrap
  cost two allocations per ITERATION.
- **range built its per-iteration scope even when the clause declared
  nothing** — `= range` assigns to bindings further out, `for range xs`
  binds nothing. That cannot change between iterations, so it is decided
  once, outside the loop.
- **`Env.vars` now allocates on first `Define`.** The scopes that remain
  are mostly empty by design — a loop body that only assigns outward still
  needs a scope so a declaration reaching it lands locally — and that case
  now costs one allocation instead of two.

What stayed, and why: the `BlockStmt` scope (the one that actually
isolates a body), `runCaseBody`'s per-case scope (a `CaseClause` body is a
bare statement list, so nothing else wraps it), range's scope when it does
declare (it is what gives each closure its own `v`), and `callClosure`'s
parameter scope.

| shape | allocs/iter | ns/iter |
|---|---|---|
| plain | 14 → **11** | 279.6 → **229.4** (−18%) |
| nested-if | 23 → **15** | 461.6 → **316.9** (−31%) |
| range | 11 → **10** | 228.1 → **210.2** (−8%) |
| closure-call | 24 → **20** | 522.9 → **442.9** (−15%) |

`NewEnv` alone: 2 allocs / 64 B / 27.8ns → **1 alloc / 16 B / 10.7ns**.
`EnvDefine` unchanged at 4 allocs / 78ns — a scope that is used pays what
it always did, which is exactly the point of deferring only the empty case.

Every existing scoping case passed **unchanged**. Six were added for the
branches the fix introduced — a for with no clause, a range that declares
nothing, a tagless switch — since those sides did not exist before.

## Fix 2: the quadratic error unwind

Found by the test suite's own runtime: `TestRunawayRecursionIsCaught` took
2.15 seconds, and a probe showed 9,000 levels of *legal* recursion running
in 14ms. All of it was the unwind.

`callClosure` wrapped every error with an `in_func` field on the way out of
every frame, and `serr.newSErr` copies the whole accumulated field list
into a fresh error on each wrap. O(N²) in call depth.

The absolute numbers were worse than the timing suggested: five failing
runs at depth 4000 allocated **7.9 GB**. One error four thousand calls deep
was building ~1.6 GB of copied field slices to say "division by zero". A
recursion typo at the prompt froze the shell for over two seconds.

### The shape of the fix

```
depth   3  raised here      <- chain is complete, render it
        2  passes through
        1  outermost frame  <- exactly one serr.Wrap
```

The two halves have to happen at different frames. Only the innermost
failing frame still has the full chain (each frame pops its own name as it
returns), and only the outermost can know nothing further will be added.

`Interp.depth` is gone — `callChain`'s length IS the depth, so the
recursion limit and the chain cannot drift apart.

| | before | after |
|---|---|---|
| runaway recursion test | 2.15s | **0.01s** |
| alloc growth, depth 500→4000 (8x) | 57.4x | **8.3x** |
| depth 4000, five runs | 7.9 GB | **11.5 MB** |

Happy path untouched: closure-call holds at ~445 ns/iter, 19,504
allocs/op. The chain append costs nothing measurable.

### The rendering got better, not just cheaper

Consecutive repeats collapse, so a runaway reads `f x10000` rather than
ten thousand identical fields. Recursion is precisely the case that
produces an unreadable chain and precisely the case that compresses to
nothing — so what survives the collapse is then capped at both ends, for
the mutually recursive chain that alternates and collapses not at all.

Order is now outermost-first (`outer > mid > inner`), reading as a call
path; it was innermost-first before.

### The subtlety worth remembering

`popFrame` preserves the in-flight chain across its deferred calls. A
defer that fails while the body has ALSO failed has its own error dropped
— and must not leave its frame in the surviving error's chain. Without the
guard, `TestCallChainIgnoresADroppedDeferError` reports `f > boom`, naming
a function that has nothing to do with the error being shown.

This is only reachable when the body error arose *directly* in that frame
rather than through a nested closure; a nested one snapshots first and
wins. Narrow, and wrong, and cheap to fix.

## Method notes

- **Break every guard on purpose, both sides.** The safe optimization
  (dropping `evalFor`'s wrap) trips the floor 14→12, 23→21 and leaves
  scope_test.go passing. The unsafe variant (dropping the `BlockStmt`
  wrap) fails four scoping cases. Putting one wrap back trips the ceiling.
  Three separate confirmations, each cheap.
- **Do not write a number you have not measured.** The unwind guard's
  comment first claimed "47.9x (2.4MB against 116MB)" from a plausible
  guess. The real figures are 57.4x and 7.9GB. Running the break test to
  get them cost one tool call and changed the story materially.
- **The test suite's own runtime is evidence.** The quadratic unwind was
  found because one test took 2.15s, not because anyone went looking.
- **Pick the metric the defect lives in.** Bytes for the unwind, for the
  same reason `TestJoinShellHeredocLinear` uses bytes: a wall-clock ratio
  over a few milliseconds flakes, and the defect is copying.
- **Measurement artifacts hide in the units.** The int-boxing cliff at 255
  silently shifted every per-iteration figure by ~1. It only surfaced
  because differencing two trip counts produced a stubbornly non-integer
  result — which a single-point measurement would never have revealed.
- One tool call was lost assuming `evalSwitch` lived in `interp.go`; it is
  in `expr.go`. The batch asserted before writing, so nothing was
  corrupted.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green after every commit,
including the 91 golden scripts and the pty e2e tests. The full
`internal/interp` benchmark suite was run before and after each fix.

The suite went from 2.4s to 0.34s along the way, which is the unwind fix
paying for itself in the feedback loop.

## Open

- **Ghost-text history scan** — linear, no index, 48µs/keystroke at 10k
  units. Not on any round's list.
- **`string(line)` on memo hits** — ~3µs / 2 allocs even when nothing
  changed. `BenchmarkSpeculateMemoHit` puts the compare at 44.5ns at 512
  lines, so the bulk is the `[]rune`→`string` conversion and its
  allocation, not the lookup.
- **`{expr}` AST cache is unbounded** (`call.go`) — the planned ~1024-entry
  cap was never added.
- **Round 2 leftover** — the mini `--explain` in the hint line.

### Recorded during this session, not fixed

- **Elided nested composite types are rejected.** `[][]int{{1,2}}` fails
  with "composite literal needs an explicit type here"; the inner `[]int`
  must be written out. Pinned in `TestElidedNestedCompositeTypeIsNotSupported`.
- **`inspectMaxItems` bounds rows but not width.** `oneLine` quotes strings
  and returns before the 60-char truncation, so one long string field
  prints in full — undercutting the "never dump megabytes" intent stated on
  the constant. Pinned in `TestInspectDoesNotTruncateLongStrings`.
- **`StructVal` assignment shares storage** where Go copies, and an error
  returned from a bare statement call is discarded. Both pinned as known
  behavior with the reasoning attached.
