# Session: Round 3 — Benchmarks, then the Two Classify Quadratics

Session: 72bf7f3e-07d5-470b-b6b8-6834c25787e2
Date: 2026-08-26

## Goal

Open Round 3 (Performance). The plan said "benchmarks FIRST" and the repo
had none, so: build the suite, read it, then fix what it actually says is
slow.

Three commits, all pushed except the last:

- `73c83db` Round 3: benchmark suite + baseline
- `ee19c1b` Round 3: make consumeGo linear
- `6488b47` Round 3: cache the speculative classify  *(not yet pushed at
  the time of writing; see the wrap commit)*

## Where Round 3 actually stood

Two of the plan's five perf items were already dead on arrival:

- **`{expr}` AST cache — already landed** at `call.go:114-132`
  (`parseFragment` keys `in.exprCache` on `src@line`). The plan's ~1024
  entry cap was never added; the cache is unbounded. Not a problem today,
  worth remembering.
- **Heredoc concat — half fixed.** `shellparse/parse.go:132` already uses
  a `strings.Builder`. `classify.go:298` still does `text += "\n" +
  lines[i]`. Still open.

Also settled two things the last session left ambiguous: **vi mode is
done** (`editor_reef.go:75` passes `inputrc.WithApp("grsh")`, and grsh
deliberately scopes its own bindings across `emacs`/`vi-insert`/
`vi-command` — `editor_reef.go:199-207`, asserted in
`editor_reef_test.go:39-54`). There is no grsh-authored vi keymap code; it
relies entirely on the user's inputrc. The mini `--explain` in the hint
line was never built.

## The benchmark suite

Three files, 15 benchmarks, plus `ai_docs/perf/round3-baseline.txt` with a
benchstat header. Three design choices carry it, and they are the reason
the numbers were readable enough to act on:

**One op = one keystroke.** The repl benchmarks advance the buffer by one
rune per iteration instead of re-rendering a fixed buffer. ns/op then reads
directly as "how long is the shell busy after I press a key", comparable
against a frame budget with no arithmetic. It also defeats the memos the
way typing does — the highlighter, hinter and suggester all memoize on the
buffer string, so a fixed buffer would have measured a map lookup. Calling
`reset()` to defeat the memo instead would model a state the editor is
never in mid-line.

**Derived metrics via `ReportMetric`.** `ns/line` for classify, `ns/iter`
for the interpreter. ns/op rising with n proves nothing (more input, more
work); **ns/line rising with n is the quadratic signature**, and it reads
straight off the output.

**Size sweeps**, so the fixed per-unit toll (classify + transform + parse)
separates from the per-iteration cost instead of being baked into one
figure.

### What the baseline said

- `consumeGo` on one unbalanced logical line: 619 ns/line at 8 lines,
  **16,256 ns/line at 512** — one pass costing 8.4ms and 22MB. Controls
  flat: shell 139 ns/line, go-block 764.
- Typing in a 20-line pending block: 58µs and 87KB **per keystroke**.
- Heredoc bodies: 3.6KB → 4.98MB of allocation from 8 to 512 lines.
- Enter on a multi-line unit: 170µs vs 90µs for `RunSource` alone.
- Interpreter: 304 ns / 13 allocs per for-loop iteration; one nested `if`
  takes it to 491 ns / 22.

Two costs that were not on the Round 3 list at all: the ghost-text
suggester is a **linear history scan with no index** (48µs/keystroke at 10k
units, and history only grows), and a pure memo hit still costs ~3µs / 2
allocs from `string(line)` conversions in both the highlighter and hinter.

## Fix 1: consumeGo is now linear (`ee19c1b`)

Old shape: *join `lines[i:j+1]`, lex the whole thing, complete? if not
`j++` and do it all again.* n joins and n full lexes for one n-line logical
line.

New: lex the source **once**, forward, testing completion at each line
boundary against state carried across lines (`goLineState` holds the three
nesting counters plus the last two significant tokens). `File` indexes the
source by line into a `goSrc`; each Go logical line lexes straight out of
those bytes, and sub-slicing is free.

### The wrong fix, and the benchmark that catches it

Handing `consumeGo` a `lines[i:]` join would be linear for a composite
literal and **quadratic for a file of many short Go lines** — each call
re-joins the whole remainder. `BenchmarkFile/go-block` exists for exactly
this and stayed flat at ~790 ns/line. Worth keeping in mind for any future
change here: the two shapes trade off against each other, and only having
both makes the tradeoff visible.

### Start-line attribution is what preserves behavior

Only two Go tokens span physical lines: a raw string and a general comment.
The old code lexed a TRUNCATED fragment, so a raw string opened on line j
came back as an unterminated `STRING` belonging to line j — and `STRING` is
semicolon-insertable, so the logical line ended there. Lexing the whole
source yields a COMPLETE `STRING` token that still starts on line j: same
kind, same line, same verdict. Comments are dropped by both.

So a multi-line raw string still terminates the logical line at its opening
line. That is arguably wrong Go and is **pre-existing** — deliberately left
alone, because this was a performance change.

### Equivalence is tested, not argued

`golines_ref_test.go` keeps the old implementation verbatim as an oracle:

- `TestConsumeGoMatchesRef` — 47-entry corpus through both
- `TestConsumeGoMatchesRefAtOffset` — repeats from every non-blank start
  line. The head facts (`headCloses`, `headTok`, `bareBrace`) and the
  scanner offset are exactly the kind of thing that works at `i=0` and is
  off by one everywhere else.
- `FuzzConsumeGoMatchesRef` — **2.17M executions, zero divergence**
- `TestConsumeGoLinear` — asserts the complexity so nobody has to remember
  to read a benchmark: 8x the lines must not cost more than 16x the time
  (it was ~63x).

### Numbers

| 512-line composite literal | before | after | |
|---|---|---|---|
| `consumeGo` | 8.36ms | 41µs | 204x |
| allocations | 22.5MB | 27.7KB | 814x |
| `Pending`/`Preview` | 8.69ms | 80µs | 108x |

**ns/line is now flat at ~80 across 8→512** — that is the actual claim; the
speedup figure is just what flatness buys at one size.

A late catch: the first version indexed the source unconditionally, which
cost pure-shell buffers a full `[]byte(src)` copy for an index they never
read (`shell/512` B/op 91,616 → 110,048). Made lazy; allocation went back
to byte-identical with the baseline.

## Fix 2: the speculative classify cache (`6488b47`)

Three derivations per frame classified the SAME source independently — the
highlighter's `Preview`, and the hint lane's `shellLineAt` (another
`Preview`) plus `breadcrumb` (a `Pending`). Each cloned the scope chain and
re-classified the whole buffer.

Now a **one-entry memo** on the `Classifier`, shared by `Pending` and
`Preview`. One entry matches the access pattern exactly: several reads of
the current buffer, then the buffer changes and the old answer is dead.

### Invalidation is local, not inferred

Three guards, each tested:

- **`File`** clears it — it advances scope, depth and the block stack
- **`Predeclare`** clears it — it seeds the root scope
- **`Clone`** never inherits it — the clone is about to be mutated

`RunSource` needs no guard: it REPLACES the live classifier with a fresh
clone rather than mutating it, so the memo goes with the discarded object.

### The lesson worth keeping

The guards were verified non-vacuous by removing each one. **With
invalidation deliberately broken, the entire rest of the suite still
passes** — 91 golden scripts, the pty e2e tests, everything. A stale entry
does not crash; it just paints the buffer with the previous frame's
classification.

Any cache added to a display path needs its own staleness test written at
the same time as the cache. Nothing else will catch it.

The probe that does catch it: `count.Field` classifies as shell before
`count := 1` and as Go after (rule 6a). A stale cache leaves the
highlighter painting it as a missing command.

`PendingInfo.Constructs` is now copied rather than aliased, since the memo
behind it outlives the call, with a test that one caller's mutation cannot
reach the next.

### Numbers, per keystroke (frame = highlight + hint + ghost)

| shape | before | after | allocation |
|---|---|---|---|
| short | 3.18µs | 2.35µs | 2483 → 1467 B |
| goline | 6.47µs | 4.12µs | 6100 → 3382 B |
| pending | 58.8µs | 36.1µs | 72KB → 43KB |
| pending-literal | 21.8µs | 13.9µs | 17.5KB → 13KB |

Combined with fix 1, **a keystroke inside an open 20-line map literal went
91.0µs → 13.9µs (6.5x)**, allocation 196KB → 13KB.

Measured in isolation the highlight and hint benchmarks each look a hair
worse (one extra allocation for the cache entry) because a benchmark
driving one component alone has nobody to share with. The frame benchmark
is where the sharing is real.

### Deliberately not done

`RunSource` classifies the same source a **fourth** time on Enter. Reusing
the memo there means handing out the mutated clone for the caller to commit
as live session state — a much sharper edge than a read-only memo — and
`BenchmarkREPLUnit` prices it at 7.4µs on a multi-line unit, once per
command, against ~36µs saved on every keystroke. The benchmark stays so the
tradeoff can be re-checked.

Also note: `Pending`/`Preview` now WRITE to the classifier where they
previously only cloned it. Safe under the contract the REPL's other
per-frame memos already rely on (single read loop; evaluation on the same
goroutine once `Readline` returns). An embedder driving `Preview` from
another goroutine was already racing over the classifier pointer and still
needs its own serialization — documented on `specCache`.

## Method notes

- **A pending "block" and a pending "literal" are different shapes.** The
  first keystroke benchmark used 20 complete statements inside an open
  func, which was never the quadratic case — the consumeGo fix barely moved
  it. Added `pending-literal` (one unfinished logical line) alongside it.
  Reporting the block number as the win would have been wrong.
- **Measure the "before" by stashing, not by memory.** `git stash push --
  <files>` restored the old pair to get an honest before/after for a
  benchmark shape added after the fix.
- **`cat > /dev/null` with no argument blocks on stdin** and backgrounded a
  whole tool call. Trivial, cost a round trip.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` — all green throughout,
including the 91 golden scripts and the four pty e2e tests. Plus manual
runs of the built binary over multi-line map/slice/func literals, mixed
shell+Go source, and the reclassify-after-declaration case.

## Open

- **Heredoc accumulation** — `classify.go:298` still `text += "\n" +
  lines[i]`. 3.6KB → 4.98MB from 8 to 512 body lines. Next.
- **Interpreter env double-wrap** — `NewEnv` (`env.go:15`) allocates its
  map eagerly, `evalFor` wraps per iteration (`interp.go:325`), and the
  `*ast.BlockStmt` handler wraps again (`interp.go:232`). Same in range
  (`:352`) and if/else (`:225`, `:228`). 13 allocs/iteration, 22 with one
  nested `if`. **Caveat:** `internal/interp` (~2,500 LOC) and
  `internal/transform` (244 LOC, the `//line` machinery every error message
  depends on) have no unit tests — only indirect golden coverage. This
  change alters evaluation scoping, so Round 4's safety net arguably comes
  first.
- **Ghost-text history scan** — linear, no index, 48µs/keystroke at 10k
  units. Not on any round's list.
- **`string(line)` on memo hits** — ~3µs / 2 allocs even when nothing
  changed.
- **Round 2 leftover** — the mini `--explain` in the hint line.
