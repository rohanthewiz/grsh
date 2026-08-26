# Session: Round 4 — Two Caps, and the Bug One of Them Uncovered

Session: fe20c2f0-8a25-4016-a795-6b64a431718c
Date: 2026-08-26

## Goal

Clear two items off the open list from the previous session: the
unbounded `{expr}` AST cache in `call.go`, and the `string(line)`
conversion the REPL paid on every memo hit.

Three commits — the third was not on any list. Chasing what the fragment
cache is actually keyed to surfaced a defect in `source`.

- `808eab6` — bound the `{expr}` fragment cache
- `caa4b02` — convert the buffer to a string once per frame
- `3c9bde7` — make `Interp.Run` re-entrant

## Fix 1: the fragment cache, and what a cap is worth here

`parseFragment` caches parsed `{expr}` interiors so an interpolation in a
loop body does not re-invoke go/parser per iteration. The planned cap had
never been added.

Before adding it, the question worth asking was whether the cache could
actually run away. It mostly cannot: the key is `(fragment source, script
line)` and **both come from the script's own text**, so within one `Run`
the entry count is bounded by the number of distinct `{expr}` sites that
execute, and the whole cache is dropped whenever `fset` is replaced.
`s.Src` reaches `EvalGoExpr` from the shell side table, which transform
populated — there is no path that synthesizes fragment text at runtime.

So the cap is for the generated script, where "bounded by the source" is
not a small number. The comment says exactly that rather than implying a
leak was fixed, and notes what it does **not** bound: the fileset still
grows one file per parse.

Eviction is a wholesale `clear`, not an LRU. Steady state is a handful of
entries at a hit rate near 1; at that shape a clear costs one re-parse
per live site per 1024 new ones, while an LRU would add a list and a
per-hit write to a path whose entire job is to be cheaper than parsing.

Three tests, driving `parseFragment` directly — it is pure interpreter,
so unlike the shell leg above it, it needs neither the side table nor a
process:

- **the hit returns the identical AST pointer**, not an equal one. That
  reuse is the reason the cache exists and had no test at all.
- the count never exceeds the cap across 3× the cap in distinct
  fragments, and the cache still hits on the other side of a flush.
- a failed parse leaves nothing behind.

## Fix 2: one buffer conversion per frame

The display engine re-derives the whole frame from the buffer on every
refresh and hands each callback a `[]rune`. Every consumer wanted a
string — highlighter, hint lane, ghost scan, and `Pending` on accept —
and each ran its own `string(line)` to reach its own memo.

That made the cheapest possible frame the expensive one. A cursor-only
refresh changes nothing, so every memo hits, and the frame still
allocated ~1.2 KB and spent 2.7µs encoding text it already had.

`runeIntern` converts once and hands the **same** string to all of them.
Two things fall out, and the second is half the win:

- the conversion is skipped when the buffer is unchanged. `runesEqual`
  decodes the cached string against the incoming runes instead of
  expanding them into a second buffer, so the check needs no storage and
  walks one byte per rune on the ASCII path. A cached **rune count**
  makes the common miss — a keystroke always changes the length — one
  integer compare.
- the downstream memos get a pointer-equal string, so their `src ==
  lastSrc` short-circuits in the runtime instead of memcmp'ing the whole
  buffer. That is why there is one intern per frame rather than one per
  consumer.

`hint.render` lost its second conversion the same way: `runePrefix`
slices the interned string at a rune index instead of re-encoding
`line[:pos]`.

| | before | after |
|---|---|---|
| cursor-only refresh | 2740 ns, 1152 B, 2 allocs | **319 ns, 0 B, 0 allocs** |
| typing, `short` | 3046 ns, 18 allocs | **2633 ns (−14%), 15** |
| typing, `pending` | 34496 ns, 43118 B, 767 | **31181 ns (−10%), 41388 B, 764** |

The saved bytes are exactly 3× the buffer on both typing shapes (1728 B
on `pending`, ~960 B on `pending-literal`) — the three conversions, gone.

### The benchmarks had to move with the code

`highlight`/`hint` keep their rune-taking wrappers for callers that hold
only runes, and the keystroke benchmarks now go through the intern
because that is what `hintProvider` does. Left alone they would have
measured a per-lane conversion the display path does not pay, and hidden
the memo-hit floor entirely.

## The measurement that lied

A first 3-run A/B said the typing path got **slower** — `short` 2693 →
3229 ns. There was no mechanism for that: the miss path does one
conversion where it used to do three, and the rune-count prefilter exits
in O(1) on a keystroke.

It was thermal drift. The two runs were sequential in one window, and the
"after" run inherited a warm machine. Ten counts each, same benchtime:

```
short     before median 3046   after 2633
pending   before median 34496  after 31181
```

At three counts the only trustworthy signal was the allocation and byte
deltas, which are deterministic. The rule that fell out: **for a change
this size, believe allocs first and ns only at n≥10.**

## Fix 3: `source` was reporting errors against the wrong file

Found while establishing that fragment keys are static: `source` calls
back into `Interp.Run` on the **same** interpreter (shellexec's
`SourceFn` → `Session.RunFile`), and `Run` installed its fileset over the
caller's and never put it back.

So for the entire remainder of the outer script, every position resolved
against a file its nodes did not come from.

**It is not a missing location, it is a confident wrong one.** With the
fix removed, the new test reports an error on line 3 of the outer script
at `ok.grsh:3:51` — a column that does not exist, in a one-line sourced
file that had already finished running. Where the sub-file is shorter
than the line being reported it degrades to `loc[:0:1]` instead. Both
name an innocent file.

That distinction was worth the one tool call it cost. The first repro
happened to produce `:0:1`, and reporting "positions are lost" would have
undersold it — a wrong attribution sends someone reading the wrong file.

### What is per-parse and what is not

Three fields are keyed to a particular parse and are saved and restored
around the nested run:

| | why |
|---|---|
| `fset` | positions in the outer file mean nothing in another fset |
| `exprCache` | entries carry positions in `fset`, so it moves with it |
| `errChain` | an error unwinding through the outer script is in flight while a `defer` that sources runs |

The frame stack and call chain need no such care: both are pushed and
popped symmetrically, and the recursion limit counting **across** a
source is right — those frames are genuinely on the stack.

### The test case that keeps the fix honest

`TestErrorPositionsSurviveASource` (in `errors_test.go`, next to the
existing `//line` gate) covers both legs — `errAt` reads the fileset
directly; a `{expr}` fragment is parsed *into* it and its line info
remapped from the enclosing node, so a stale fset corrupts the fragment
as well as the report — two levels of nesting, and the sourced file still
reporting **its own** lines while it is the one running.

That last case is the point. A "fix" that simply declined to install the
nested fileset passes the first three and fails only that one.

The two legs also report through different surfaces: a Go error comes
back from `RunSource`, while a `{expr}` that fails during word expansion
is printed and sets a status, the way a failing command does. The test
gathers both and asserts on the position, since which surface carried it
is not what it is about.

## Method notes

- **Break every guard, both ways.** Five confirmations this session: a
  per-lane `string(line)` restored (frame alloc guard fails at 1),
  eviction removed (cache reaches 1025), `runePrefix` byte-indexed (fails
  on `日本語`, and only there), and the `Run` save/restore removed (three
  of four source cases fail — the fourth passing is designed in).
- **Ask what the key is made of before capping a cache.** Ten minutes
  tracing `s.Src` back to the side table changed the cap from a leak fix
  into documented insurance, and the comment now says which one it is.
- **A harness that reads state after the call is a guard too.**
  `fragmentHarness` segfaulted the moment `Run` started restoring
  `in.fset` — the only reader of that field outside a run, which is
  itself the answer to "is this safe to restore?"
- **The wrong location is worse than no location.** Worth measuring what
  the broken form actually prints rather than describing it from the
  first repro.

## Verification

`gofmt -l`, `go vet ./...`, `go test -race ./...` green after each
commit, including the 91 golden scripts and the pty e2e. Frame and
memo-hit benchmarks re-run at n=10 for the numbers above; a live
`{expr}`-in-a-loop script and a live `source` chain checked by hand.

## Open

- **Ghost-text history scan** — linear, no index, ~44µs/keystroke at 10k
  units (measured again this session, unchanged). Still on no round's
  list, and now the largest single item in a typing frame.
- **Round 2 leftover** — the mini `--explain` in the hint line.
- **The for-clause vs range closure asymmetry** (`3 3 3` against
  `10 20 30`) is still pinned as a deliberate tripwire, still unsettled.
- Carried from the last session, unfixed and pinned: elided nested
  composite types rejected, `inspectMaxItems` bounding rows but not
  width, `StructVal` assignment sharing storage, and an error from a bare
  statement call being discarded.
