# Session: Phase 5 — The Web Tour

Session: 9797baac-a58e-4c04-b7e7-bf9faddde8f1
Date: 2026-08-30

Phase 5 of `ai_docs/plans/interactive-tutorial-plan.md`, the one marked
*(optional, later)*. The same eight chapters in a browser, with no second
copy of the curriculum, the verifiers, or the engine.

## The engine was never coupled to the REPL

The plan's premise was that the embedding API would make a web tour nearly
free. Half right. The transport was cheap; the reason it was cheap is
different from the reason the plan gave.

The engine is a `repl.Interceptor` — four hooks around an input unit. A
REPL is not the only thing that can call them. So Phase 5's real work was a
second host that reproduces `repl.loop`'s call sites exactly:

```
repl.loop                          Driver
───────────────────────────────    ────────────────────────────────
drain job notifications            advance()
ic.Done() → leave the loop         advance()  → jump / finish
ic.BeforePrompt(w)                 advance()
rd.Readline()                      Submit(line)
sess.Pending → keep reading        Submit: accumulate, report Pending
ic.Command(src) → next unit        Submit
replCommand / hist / sess.Eval     Submit, via repl.UnitLog
ic.AfterEval(src, err)             Submit
```

`repl.UnitLog` — built in Phase 3 for the content self-check — turned out
to be exactly the seam this needed. The capstone's `session save` and
chapter 3's `?count` never reach `Eval`, and a web host that dispatched
them itself would have been a second implementation free to drift from the
prompt's.

Two deviations from the sketch, both deliberate:

**A separate binary.** `cmd/grsh-tour`, not `grsh tour`. Reaching rweb from
grsh's `main` links a web framework into every copy of the shell for the
one session in a thousand that takes the tour. Verified rather than
assumed: `go list -deps ./cmd/grsh | grep -E 'rweb|element'` is empty.

**`runner.Session`, not `grsh.NewSession`.** The plan said the public
embedding API. It cannot grade: the verifiers read `VarInfo`,
`LastStatus`, `Preview` and `Idents`, none of which the public API
exposes. `internal/tour` uses the same constructor the terminal tutor does.

## Two surfaces, two streams

`POST /input` takes one physical line. SSE carries two event types:

- `out` — the bytes the shell and the engine wrote, escape codes intact,
  rendered to spans by ~40 lines of SGR parsing in the page.
- `state` — `tutor.View`, the engine's state as data.

The transcript renders the first; the sidebar renders the second. Neither
reads the other, which is the point: the sidebar is drawn from the View,
never scraped out of the text, so rewording a lesson cannot break the UI.

The step panel is routed to `io.Discard` (a new `engine.panels` field) —
the sidebar already says it, and printing it twice is the tell that gives
away a terminal wearing a costume. Everything else the engine prints is a
*reply* to something the student just did, so ticks, hints and the outro
stay inline where they belong.

`View` withholds on purpose. Hints and the solution are **absent** until
earned, not present-but-hidden: anything in the View is readable in the
page source, so `:hint` would be theatre. Writing that exposed a real bug —
the first draft used "the hints have run out" as the condition, which a
step with zero hints satisfies on its very first prompt. The engine gained
an `answered` flag that records the moment it actually shows an answer.

Chapter 2 keeps its lesson. `--explain`'s live verdict lives in the
prompt's hint lane, and a page has no prompt to decorate, so
`Driver.Classify` hands the verdict over and the page draws its own lane
under the input (debounced, 120ms).

## The constraint that is not fixable at this layer

A grsh session's working directory **is** the process's working directory —
`cd` calls `os.Chdir`, deliberately (see `internal/shellexec`). Two
visitors therefore cannot both stand in their own playground at once.

`evalGate` is the answer: one process-wide mutex, and each driver re-enters
its own `cwd` for the duration of an evaluation. Between evaluations the
process cwd belongs to whoever ran last, which nothing reads.

The cost is stated rather than hidden: one student's `sleep 30` blocks
every other student's next command for thirty seconds. That is acceptable
for what this is — a local tool serving a few tabs on one machine — and
isolating students properly means a process each. The environment is *not*
restored the same way (`export` is also process-global); no chapter exports
anything, and snapshotting the environment per eval would cost more than
the leak does.

`TestDriversKeepSeparatePlaygrounds` is the test that matters: interleaved
commands each see their own fixtures, and a `cd` in one does not move the
other. `TestDriversRunConcurrently` runs the same thing under `-race` from
the goroutines a web host actually serves on.

It binds to loopback and refuses anything else without `-allow-remote`.
That is a reminder, not a boundary — the thing runs shell commands as the
user. `requireLoopback` resolves the host rather than matching on
"localhost", because the interesting mistake is `--addr :7654`, which is
every interface and looks like nothing at all.

## Three bugs that only running it could find

**1. `Content-Encoding: text/plain`.** rweb labels SSE responses with a
media type where a content *coding* belongs. Go's http client ignores the
header (it only ever auto-decodes gzip), so every Go test passed while the
page in a browser showed a connected stream that never delivered a byte —
Chrome and curl both discard a body whose coding they do not recognise.
Overridden to `identity` after `SetSSE`. The regression test asserts the
header directly, because nothing about the events themselves can catch
this from Go.

**2. rweb's `Run` installs its own SIGTERM handler** and returns when one
arrives. A second handler in `main` seemed obvious and was wrong: Go
delivers a signal to every registered channel, so the two raced — and ours
lost, because `Run` returned and `main` fell off the end while the handler
was still tearing down. Every Ctrl+C leaked a playground. Shutdown is now
"`Run` returned", and teardown runs whatever ended the server.

**3. The table of contents ticked every chapter before the current one.**
Jumping to chapter 4 marked 1–3 complete — congratulating a student for
work they never did. `View` now carries real per-chapter completion,
tracked across chapter switches and seeded from the store when `-progress`
is on. It also ticks the *current* chapter the moment it is finished,
rather than when the student happens to leave it.

A fourth, found the same way: a 404 from a dead session (server restarted,
or a tab reclaimed) left the page silently ignoring every keystroke —
indistinguishable from a shell that stopped answering. The page now says
so, and a bfcache-restored page reloads itself once.

## Progress persistence is off by default

`-progress` opts in. A long-running tour server holding the shared
`~/.grsh_tutor.db` open would silently downgrade `grsh tutor` to a
`nopStore`, and a browser tab keeps its own session alive for as long as it
is open anyway. `tutor.OpenStore` is the new exported opener, and its
doc comment says why it is a call and not a default.

## The page is embedded, not generated

`element` is the right tool when the markup depends on data. Here it does
not: every dynamic part — transcript, chapter list, step — is drawn by the
browser from the SSE stream, so the server's HTML is a fixed skeleton with
holes in it. As Go it would be a worse version of itself, with none of an
editor's help for the CSS and JavaScript that are most of the file.

Layout note: the two rows are flex, not `calc(100% - 41px)`. The bar is
sized by its own content, and a subtracted constant is wrong the moment
anything in it changes font.

## Tests

- **`TestDriverRunsTheCurriculum`** — the highest-value one, and the reason
  the tour ships no content of its own: every shipped step's canonical
  solution, submitted through the headless driver one physical line at a
  time, must tick that step over. Worth having alongside
  `TestContentSolutionsPass`: that one proves the lessons are right, this
  proves the driver reproduces `repl.loop` faithfully enough to grade them.
  A driver that dropped the capture reset, mis-ordered `Command` against
  `Eval`, or lost the continuation buffer passes the first and fails here.
- `TestDriverContinuation`, `TestDriverWithholdsTheAnswer`,
  `TestDriverJumpsChapters`, `TestDriverClassifies`,
  `TestDriverMarksOnlyFinishedChapters`, `TestDriverEndsOnExit`,
  `TestDriverQuits`.
- **`internal/tour`** — the HTTP surface end to end over a real listener on
  a kernel-chosen port: session minting, `/state`, meta-commands through
  `/input`, the SSE stream and its replay-on-reconnect,
  `/classify`, `/reset`, 404 for a stranger,
  `TestTourSendsADecodableStream` (the header, see above),
  `TestServerCloseRemovesPlaygrounds`, `TestRequireLoopback`.

Verified in a real browser too: typing `ls` → real output → graded → 2/6,
`:menu 3` with a fresh playground, a multi-line `for` block echoed with
`▸`/`…` markers, the miss → hint escalation. Chrome's screenshots came
back colour-inverted and the tab got cache-stuck partway through, so the
remaining checks went through the HTTP API rather than fighting the
tooling further.

## Verification

```
gofmt -l .                       # clean
go build ./... && go vet ./...
go test ./... -count=1           # all green
go test -race ./internal/tour ./internal/tutor -count=1
go list -deps ./cmd/grsh | grep -E 'rweb|element'   # empty
```

## Files

```
internal/tutor/drive.go          Driver: the engine for a host with no line editor; evalGate
internal/tutor/view.go           View: the engine's state as data
internal/tutor/drive_test.go     the curriculum through the web path, and the gate
internal/tutor/tutor.go          engine.panels, engine.answered, chapterOpts/newChapter
internal/tutor/commands.go       :sol records that it answered
internal/tutor/progress.go       OpenStore, for a host outside the package
internal/repl/host.go            UserMessage(src, err) — the loop's error rendering, exported
internal/tour/tour.go            rweb server: routes, visitors, the loopback guard
internal/tour/visitor.go         one student: a Driver, a lock, a greeting
internal/tour/sink.go            transcript: io.Writer in, SSE + bounded replay out
internal/tour/assets/            index.html, app.css, app.js
internal/tour/tour_test.go       the HTTP surface end to end
cmd/grsh-tour/main.go            flags, the loopback refusal, shutdown
go.mod                           + rweb v0.1.26 (element rides along)
README.md                        "The same tutorial in a browser: grsh-tour"
ai_docs/plans/…-plan.md          Phase 5 done
```

## Open

- **One process, one working directory.** The eval gate makes concurrent
  visitors correct, not fast, and `export` still leaks between them. Real
  isolation is a process per visitor, which is a different program.
- **`grsh-tour` has no test of its own** (`cmd/grsh-tour` is flags plus
  wiring). The shutdown rule it exists to get right is pinned in
  `TestServerCloseRemovesPlaygrounds` instead.
- **The `Content-Encoding` override is a workaround in our code** for
  something that belongs upstream in rweb. Worth a patch there; the
  override is harmless if it lands, since `SetHeader` replaces.
- **No keyboard interrupt from the page.** The stop button posts
  `/interrupt`; Ctrl+C in the input does nothing, because the browser
  never gives it to us. `Kill` is wired on the Driver but unexposed —
  nothing has needed the escalation yet.
- **The transcript is bounded at 256KB** and drops the head. A student who
  scrolls back that far loses the earliest chapters of a long session.
