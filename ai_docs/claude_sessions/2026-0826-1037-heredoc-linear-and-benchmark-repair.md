# Session: Heredoc Accumulation, and a Benchmark That Had Stopped Measuring

Session: 9c0c89c6-71d5-42a9-a629-d77af7bceda4
Date: 2026-08-26

## Goal

Continue Round 3 from the open list: fix the heredoc accumulation
quadratic at `classify.go`. That landed, and running the full benchmark
suite afterward turned up a second problem the previous session's own
optimization had created — `BenchmarkPending` and `BenchmarkPreview` were
no longer measuring anything.

Two changes, both in `internal/classify`.

## Fix: heredoc body accumulation is linear

### The shape

`joinShell` applies shell continuation rules, then swallows raw heredoc
body lines into the chunk. The body loop did:

```go
text += "\n" + lines[i]
```

One allocation per body line either way — but each one holds EVERY byte
accumulated so far, so total copying is quadratic in body length. A
512-line body burned 4.98MB to produce a ~20KB chunk.

### The change

Hoist `scanHeredocs(text)` above the accumulation, return early when there
are no heredocs, and append into a `strings.Builder` otherwise.

Hoisting the scan is not merely a fast path, and the code says so: the scan
must run on `text` — the continuation-joined FIRST logical line — and never
on the accumulated chunk, because body lines are raw data and must not be
searched for further `<<` operators. Computing `hds` once, before the
Builder exists, is what makes that structural rather than incidental.

The no-heredoc early return matters too: the overwhelmingly common case
returns `text` unchanged, so a plain shell line pays no Builder and no
second copy of the string. `BenchmarkFile/shell` is unmoved.

### Numbers (`BenchmarkFile/heredoc`)

| body lines | ns/line before | after | B/op before | after |
|---|---|---|---|---|
| 8 | 152.5 | 146.3 | 3,616 | 2,704 |
| 32 | 110.8 | 64.5 | 23,136 | 4,976 |
| 128 | 213.8 | 43.9 | 321,186 | 20,656 |
| 512 | 704.3 | 38.9 | 4,979,640 | 94,640 |

ns/line now FALLS with n (fixed per-call setup amortizing) instead of
rising. 18.1x wall time and 52.6x allocation at 512.

### The guard asserts bytes, not time

`TestJoinShellHeredocLinear` measures `runtime.MemStats.TotalAlloc` around
50 `joinShell` calls at 64 and 512 body lines, and fails above a 16x ratio.

**An alloc COUNT would have caught nothing.** The old loop made exactly one
allocation per body line, so allocs/op is linear under both shapes; only
total BYTES shows the quadratic. Bytes is also deterministic enough that a
modest bound does not flake the way a timing ratio would — which is why
this differs from `TestConsumeGoLinear`'s wall-clock ratio.

Verified non-vacuous by reverting the fix: **62.2x** (79.9KB vs 4.97MB).

A package-level `joinSink` keeps the result live so the store is not
eliminated.

### Test added for the case the Builder actually spans

`TestHeredocMultiChunkText` pins the chunk text for `cat <<A <<-B`. One
command can open several bodies, read in order, each with its own
delimiter — and the Builder now spans separate iterations of the outer
`hds` loop. A mis-seeded or re-seeded buffer would duplicate the command
line or drop the first body. `repl_test.go:38-39` already had two-heredoc
cases but asserts only `NeedsMore`, so it could not see any of that.

## Repair: two benchmarks that had degenerated into cache lookups

Found by reading the full suite output after the heredoc fix, not by
looking for it.

`BenchmarkPending` and `BenchmarkPreview` reported **7.8ns and 2.16ns, 0
allocs, at every size** — 8 lines and 512 lines alike. Both build one
classifier and one `src` outside the loop:

```go
for b.Loop() { c.Pending(src) }
```

Since the speculative memo landed in `6488b47`, every iteration after the
first hit `specCache`. Both are listed in the file header as scaling
guards. They were timing a string compare.

This is the failure mode worth remembering: **an optimization can silently
disarm the benchmark that was supposed to police it**, and the evidence
looks like a win (0 allocs, single-digit ns) rather than like breakage.

### The repair

- `goCompositeVariants(n, k)` — k sources of the same shape and line count,
  differing only in the **LAST** element's value. Varying the tail rather
  than the head is deliberate: it is where a typing user's edits land, so a
  future prefix-reusing cache would be exercised the way the editor would
  exercise it, instead of being defeated by an artificial change on line 1.
  The varied value is fixed-width so every variant is the same byte length.
- `specVariants = 8`, **not 2.** Two defeats today's one-entry cache, and
  that is exactly the trap: a cycle sized to the cache starts hitting again
  the moment someone grows the cache — the same rot, re-introduced.
- `assertSpecMisses` asserts the property instead of trusting it. Primes a
  full lap, then walks the cycle checking `c.spec.src` never already equals
  the source about to be passed. Verified non-vacuous: with
  `specVariants = 1` it fails with *"variant 0/1 would hit the memo — this
  benchmark is timing a cache lookup, not a classify"*.

### Numbers, now real work

| n | Pending ns/line | Preview ns/line | allocs/op |
|---|---|---|---|
| 8 | 238.9 | 243.2 | 44 |
| 32 | 173.8 | 218.6 | 91 |
| 128 | 167.9 | 169.2 | 289 |
| 512 | 155.0 | 161.8 | 1063 |

The two track each other closely across the sweep, which is what their
comments have always claimed and what neither could previously show.

### BenchmarkSpeculateMemoHit — the honest version

Added alongside, mirroring `BenchmarkKeystrokeMemoHit` in `internal/repl`.
It prices the second and third derivation of a buffer the first already
classified — the thing the cache exists to make cheap.

It primes with `src` and measures against `strings.Clone(src)`: equal
content, **different backing array**. Not cosmetic. The REPL reaches the
memo through `string(line)` conversions off the editor's `[]rune` buffer
(`repl/highlight.go`, `repl/hint.go`), so the two strings never share a
pointer and the compare cannot take Go's pointer-equality fast path.
Priming with the same variable reports an O(1) hit for what is really a
full memcmp of the buffer — which is precisely why the broken
`BenchmarkPreview` read a flat 2.16ns.

Measured properly the hit is **8.0ns at 8 lines, 44.5ns at 512** — visibly
per-byte, and still a ~1800x floor under a real classify at 512.

### Header note

Added to the file header, so the next person sees it before writing a
benchmark rather than after: driving a memoized path with one fixed source
measures the memo; a new benchmark must either vary its input every
iteration or say in its name that the hit is the thing being priced.

## Method notes

- **Read the whole suite after a targeted fix.** The benchmark rot was two
  columns away from the number being checked and would not have surfaced
  from running only `BenchmarkFile/heredoc`.
- **Pick the metric the defect actually lives in.** Bytes for the heredoc
  quadratic (allocs are linear either way), time for `consumeGo`. Choosing
  the convenient metric would have produced a test that passes forever.
- **Break it on purpose, both times.** The heredoc guard and the memo-miss
  guard were each confirmed by reintroducing the defect. A guard nobody has
  seen fail is a guess.
- One tool call was wasted on a too-clever in-place "revert" via string
  substitution; copying the fixed file to the scratchpad and rewriting the
  block plainly was faster and reversible.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` — green throughout,
including the 91 golden scripts and the pty e2e tests. Full
`-bench . -benchmem` run over `internal/classify` confirms no other shape
regressed (`shell`, `go-block`, `go-composite`, `ConsumeGo` all unchanged
from the post-`6488b47` figures).

## Open

Unchanged from the previous session except that heredoc accumulation is now
closed:

- **Interpreter env double-wrap** — `NewEnv` (`env.go:15`) allocates its map
  eagerly, `evalFor` wraps per iteration (`interp.go:325`), the
  `*ast.BlockStmt` handler wraps again (`interp.go:232`); same in range
  (`:352`) and if/else (`:225`, `:228`). 13 allocs/iteration, 22 with one
  nested `if`. **Caveat stands:** `internal/interp` (~2,500 LOC) and
  `internal/transform` (244 LOC) have no unit tests, only indirect golden
  coverage, and this change alters evaluation scoping — Round 4's safety net
  arguably comes first.
- **Ghost-text history scan** — linear, no index, 48µs/keystroke at 10k
  units. Not on any round's list.
- **`string(line)` on memo hits** — ~3µs / 2 allocs even when nothing
  changed. Now partly quantified from the classify side:
  `BenchmarkSpeculateMemoHit` shows the compare alone is 44.5ns at 512
  lines, so the bulk of that 3µs is the `[]rune`→`string` conversion and
  its allocation, not the lookup.
- **`{expr}` AST cache is unbounded** (`call.go:114-132`) — the planned
  ~1024-entry cap was never added.
- **Round 2 leftover** — the mini `--explain` in the hint line.
