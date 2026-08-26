# Session: Round 5 — The Ghost Gets an Index, and --explain Gets a Prompt

Session: 897eaa14-53b1-45b6-860b-6500aca1ad82
Date: 2026-08-26

## Goal

The two remaining named items: the linear ghost-text history scan (the
largest single item in a typing frame since the frame intern landed), and
the Round 2 leftover — the mini `--explain` in the hint line.

- `fe27230` — index the unit history behind ghost text
- `b191e78` — `--explain` in the hint line

## Fix 1: the ghost scan, and the worst case it was hiding

`suggester.match` walked the whole unit store newest-first per keystroke
and returned the first entry the buffer was a strict prefix of. Linear
in a history the user grows without bound, and history is the only input
to the display path that is not bounded by the buffer:

```
100 units    0.6us      1000 units   6us      10000 units   53us
```

The units are now kept lexicographically sorted (`ghostIndex`), so every
unit sharing a prefix is one contiguous run that two binary searches
bound.

### The measurement that changed the design

The old benchmark typed `zzz-no-such-command` — a prefix matching
nothing, chosen as the worst case for a linear scan. It is, and the
index answers it in 51ns instead of 53us.

But adding the other prefix shape — one every unit matches — showed the
first version of the index was **worse than what it replaced**:

| 10000 units, per keystroke | scan | sorted only | + block summaries |
|---|---|---|---|
| buffer matches nothing | 53us | 55ns | 51ns |
| buffer matches every unit | 14ns | 23us | 238ns |

The scan's 14ns is real: newest-first means a prefix shared by the
*newest* unit is answered on the first comparison. The sorted run has to
be searched for its newest member, and when the prefix is one rune of a
command you run constantly, that run is the whole store.

So each block of 64 entries carries the index of its newest member, and
the range walk reads one summary per whole block, touching individual
entries only in the two partial blocks at the ends of the run. Bounded
by `blockSize` whatever the history holds.

That row is still a trade and the file says so. What it buys is that the
*shape* of the history stops deciding whether the ghost keeps up: the
scan's 14ns applied only when the newest unit matched, and it paid the
full 53us for a prefix whose matches are old — which is most of what a
long history holds.

### What else fell out of sorting

- **Dedupe.** Re-running a command moves its existing entry's recency
  instead of adding a second one. Ghost text only ever shows the newest
  match, so the older occurrences were unreachable already; dropping
  them shrinks the searched run as well as the memory.
- **Multi-line units never enter.** They are unrenderable as ghost text
  (the file comment has always said so), so the index is a subset of the
  store rather than a copy of it.
- **Recency is the store position**, not a counter. That is what makes
  the bulk and incremental paths produce byte-identical indexes, which
  is a test.

### One merge, not two code paths

`absorb` merges the store's new tail into the sorted slice. The obvious
alternative — insert each new unit at its sorted position — is fine in
the steady state (one unit per accepted command) and catastrophic at the
first prompt, where the whole history file arrives at once: ~1GB of
memmove for a 10k-unit file, on the first keystroke of the session.

A sorted merge is O(n+m) at both ends, so the steady state needs no
special case; it is already the cheap end of the same path.

```
10k units   load 725us / 246KB   (once, first keystroke of the session)
            fold-in 35us / 205KB (once per accepted command)
```

Both are off the per-keystroke path — the second only just, which is why
the comment records that it is linear.

### The frame

Medians of 5, same run, before/after:

| frame shape | before | after |
|---|---|---|
| short | 2379 ns | **1264** |
| pathcmd | 3259 ns | **1597** |
| goline | 4110 ns | **2489** |
| pending | 32560 ns | 33437 |
| pending-literal | 11957 ns | 11897 |

~1.6us off every single-line shape, which is the 500-unit scan the frame
benchmark was paying. The two multi-line shapes are unchanged, and
correctly so: a buffer containing a newline exits `match` before the
history is touched at all.

## Fix 2: `--explain` where you can act on it

`--explain` prints one row per chunk from `RunSource`, **after** a unit
has been evaluated. In a script that is the whole story. At a prompt it
answers the question too late, and answers it about a line that has
already scrolled away.

So under the flag the hint lane carries the classifier's verdict for the
line the cursor is on, live, in the same terms the batch output uses:

```
shell · rule=default          rule 7 — nothing claimed it, so: a command
go · rule=declared-ident      rule 6a
shell 1-2 · rule=default      a backslash continuation: ONE chunk
go 1-3 · rule=define          an open composite literal: also one chunk
go · rule=incomplete          the best-effort tail of an unfinished unit
```

The span is the half a script's output gives away for free (it prints
`name:3-5`) and a prompt does not. That several physical rows are one
logical line is a classifier decision, and it is invisible in the buffer.

Three smaller decisions:

- **Composed last.** Turning the flag on adds a segment; it never moves
  the three that were already there.
- **Blank and comment lines are skipped**, exactly as the batch output
  skips them — they carry a Kind and no rule.
- **It is free.** The breadcrumb's `Pending` has already speculated this
  exact source and `Preview` reads that same memo (`classify.speculate`),
  so the lane costs a scan of a chunk slice. `shellLineAt` and the new
  lane now share one `chunkAt`.

An open **block** and an open **literal** report differently — `go ·
rule=keyword` versus `go · rule=incomplete` — because the first
classified fine and is merely deep, while the second failed mid-
expression and came back as a tail chunk. That distinction is now a test;
it was a surprise while writing one.

### The wiring test

`--explain` reaches the hinter through `main` → `Options.Explain` →
`Session.Explaining` → `newHinter`, a chain where every unit test sees
one link. `TestReefExplainHintEndToEnd` drives the real binary on a pty
under the real flag and types **without pressing Enter**, which is the
whole point of the lane. `startShell` grew a `startShellArgs` sibling for
it.

## Method notes

- **Benchmark the shape your change is good at AND the one it is bad
  at.** The old ghost benchmark had one prefix, picked as the linear
  scan's worst case. Sorting made that case 1000x better and a case
  nobody had measured 1600x worse. The second prefix shape is now in the
  suite permanently, with a comment saying why the two are different
  problems now when they were the same one before.
- **A grep for `FAIL:` hides a build failure.** One guard-break in this
  session "passed" because removing the code left `fmt` unimported; the
  harness filtered the compile error out. Breaking a guard has to fail
  *loudly*, and the filter has to let a build error through.
- **A test that passes can still be wrong.** `TestGhostIndexPrefixRunBounds`
  was written asserting `suggest("jobs") == ""` on a store containing
  `jobs -l`. It failed, correctly — the expectation was, and the comment
  next to it described a case the data did not contain. Caught only
  because a truncated `head -40` hid the failure for one round.
- **Guards broken, both ways:** exact-match entry not skipped, block
  summary pointed at the block head, dedupe keeping the oldest of a run,
  multi-line units admitted, run upper bound not searched, explain lane
  ungated, blank chunks admitted, span dropped, explain composed first,
  and the pty test run without the flag. Ten; every one failed the tests
  it was supposed to.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green after each
commit, including the 91 golden scripts and the pty e2e. Ghost, absorb
and frame benchmarks re-run at n=5 with a clean A/B for the numbers
above.

## Open

- **Round 2 is now closed.** Every item on it has landed.
- **The for-clause vs range closure asymmetry** (`3 3 3` against
  `10 20 30`) is still pinned as a deliberate tripwire, still unsettled.
- Carried, unfixed and pinned: elided nested composite types rejected,
  `inspectMaxItems` bounding rows but not width, `StructVal` assignment
  sharing storage, and an error from a bare statement call being
  discarded.
- Smaller, newly visible: `absorb` allocates two fresh slices per
  accepted command (205KB at 10k units). Once per command, so not on any
  budget that matters today — noted because it is the one linear thing
  left on the ghost path.
