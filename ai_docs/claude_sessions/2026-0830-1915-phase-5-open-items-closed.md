# Session: Phase 5's Open List, Closed

Session: 5963c4a9-9344-439f-808f-17cbe6852d40
Date: 2026-08-30

The four items Phase 5 left standing. Three were work; the fourth found a
bug that only a browser could have shown, which is now twice this feature
has taught the same lesson.

## `export` leaked, and that was fixable

"One process, one working directory" was written as one item, and it is
two. The serialization — one student's `sleep 30` blocking another's next
command — genuinely needs a process per visitor. The environment did not.

`evalGate` restored the cwd and explicitly declined to restore the
environment, on the argument that no chapter exports anything and a
snapshot would cost more than the leak. Both halves of that are weak. The
cost is one `os.Environ()` and a map per evaluation, next to a fork. And
the leak is not confined to the tutor's own lessons: the playground exists
precisely so students type things nobody wrote a chapter for, and exec
hands every child `os.Environ()`, so one tab's `export` reached another
tab's subprocesses.

`Driver.locked` now reinstates the environment the same way it reinstates
the cwd. The restore is a three-way reconcile — remove what is not ours,
correct what differs, add back what is missing — rather than a clear and
re-set, because there is no atomic way to empty an environment and a
moment with no `PATH` in it is a moment a concurrently forking child can
see. Most evaluations change nothing and it does no syscalls at all.

`TestDriversKeepSeparateEnvironments` covers the three branches with the
three cases that produce them: same name different values, a name only one
driver ever had, and a name the neighbour deleted. Checked it bites by
commenting the restore out — all three fail, which is the point of writing
three.

## `grsh-tour`'s main was untestable by construction

Not "untested" — untestable. `serve` blocks until a signal, `OpenStore`
takes the developer's own `~/.grsh_tutor.db`, and `browse` opens a window
on whatever machine runs the tests. So main is now
`run(args, stdout, stderr) int` with those three as variables, and the
part worth pinning is reachable.

The exit code is returned rather than `os.Exit`ed, so teardown always
runs. That is the whole reason this file exists: the shutdown order is
what main owns, and getting it wrong leaks a playground directory and a
live shell session on every Ctrl+C.

`TestRunClosesPlaygroundsWhenTheServerStops` stands in for the signal —
the stub starts the real listener, visits the page as a browser does,
confirms the playground exists, then returns. Everything after `serve` in
`run` then happens for real. `tour`'s own test proves `Close` does the
work; this proves main calls it.

The rest: exit codes, the loopback refusal naming its own escape hatch,
`-allow-remote` actually working, `-open` on and off, the launcher for
this GOOS, and `-progress` closing the store on both the normal exit and
the early one where `New` refuses an address.

## Ctrl+C had to go on the document

The reason this sat open is a good one. The page disables its input while
a command runs, and **a disabled input receives no key events** — so the
one moment Ctrl+C means anything is the one moment the obvious listener is
deaf. It goes on `document`.

The rest follows a terminal:

- A non-empty selection is left alone. Ctrl+C is also copy, and a
  transcript you cannot quote is a worse loss than a stop button that
  needs the mouse.
- Idle, it discards the half-typed line locally — no round trip, and no
  abandoned draft in the session's history.
- Busy, it posts `/interrupt`. A second press while still busy escalates
  to `Driver.Kill` via `?hard=1`. Separate requests, not an automatic
  retry: nothing here should decide on a student's behalf that a program
  has had long enough.

The stop button is the same path, escalation included.

Also fixed on the way: `visitor.d` was read by the signal path without a
lock while `restart` wrote it under one. A real race, unexercised so far.
It has its own tiny mutex now, held for a load and a store and never
across a call — the signal path still must not wait for the eval lock.

## The transcript bound, and the bug under it

The complaint was that 256KB drops the head silently. The fix is two
things: cut on a line boundary — past the end of any escape sequence and
any rune, since neither contains a newline — and say so, with a notice
prepended to the replay whose trailing reset also repairs the colour the
cut left open.

Then running it showed the fix was worse than the bug.

macOS `base64` writes the whole encoding **unwrapped**. So the buffer is
one enormous line, and "advance to the next newline" finds the one at the
END of it: the trim discarded everything and kept 85 bytes. A student who
floods their transcript reloads into an empty terminal.

Caught by measuring a real replay in Chrome after a real flood, not by any
test — every test wrote newline-terminated output, where the search
succeeds immediately and the bug cannot appear. The walk is bounded now
(`lineScan`, and capped at a quarter of the buffer so it scales down with
a small one); past the bound it rune-aligns and accepts a possibly broken
escape, which is the right trade against losing the transcript.

The regression test's second case is `strings.Repeat("é", limit) + "\n"`,
and the trailing newline is the entire point. Without it the search fails
and the fallback runs for the right reason by accident; with it the search
*succeeds* and returns an offset near the end, which is the broken case.

The page got a bound too, and it is a different bound for a different
reason: the server's caps what a RELOAD redraws, while a tab left open for
hours only ever grows — every write another node, a leak with a scrollbar.
It trims whole child nodes, so there is no colour state to repair.

## Verified in a browser, because that is where these live

- Idle Ctrl+C: line cleared, `^C` in the transcript, no request.
- Busy Ctrl+C: killed a live `sleep 30`, input re-enabled.
- Escalation: against `/bin/sh` running `trap "" INT; sleep 25`, the first
  press sent `/interrupt` and the script survived; the second sent
  `/interrupt?hard=1` and it died. Transcript reads `^C` then `[killing…]`.
- Page trim: 534,003 chars → 439,967 with one `trimmed` notice at the top.
- Server trim: a 400KB unwrapped flood replays 262,216 bytes with the
  notice — the number that was 153 before the fix.
- Shutdown: SIGTERM left no `grsh-tutor-*` directory behind.

## Verification

```
gofmt -l .
go build ./... && go vet ./... && go test ./... -count=1
go test -race ./internal/tour ./internal/tutor ./cmd/grsh-tour -count=1
go list -deps ./cmd/grsh | grep -E 'rweb|element'   # still empty
```

## Files

```
internal/tutor/drive.go          evalGate comment rewritten; env field; locked
                                 restores it; restoreEnv
internal/tutor/drive_test.go     TestDriversKeepSeparateEnvironments
internal/tour/sink.go            trimmed flag; trim() with lineScan; replay notice
internal/tour/visitor.go         dMu guards the driver pointer; interrupt/kill
internal/tour/tour.go            /interrupt takes ?hard=1
internal/tour/tour_test.go       interrupt stops a real command; escalation
                                 routing; two trim tests
internal/tour/assets/app.js      document-level Ctrl+C; escalation; page trim
internal/tour/assets/app.css     .trimmed
cmd/grsh-tour/main.go            run(args, stdout, stderr) int; three seams
cmd/grsh-tour/main_test.go       new — shutdown order, exit codes, flags, store
ai_docs/plans/…-plan.md          Phase 5: env fix, fourth bug, the two closures
```

## Open

- **One process, one working directory** — the serialization half only.
  Real isolation is a process per visitor, which is a different program.
- **The tour has no test that runs a browser.** Two of the four bugs in
  this feature were invisible to Go and found by hand. Nothing pins them
  from a cold start; the assertions that exist (`Content-Encoding`, the
  trim's line shape) were each written after a person looked at a page.
- **`grsh-tour -addr host:0` prints `http://host:0`.** The URL goes out
  before the listener exists, so the port the kernel picked never reaches
  the user. Nobody passes port 0 on purpose; the tests do, and one of them
  now asserts the wrong-looking string.
