# Session: Phase 1 — The Tutor Engine Skeleton

Session: 455e57cf-d6a3-48db-b358-2646d32e2afe
Date: 2026-08-30

Phase 1 of `ai_docs/plans/interactive-tutorial-plan.md`: `grsh tutor`, an
interactive tutorial that runs **inside the real REPL** rather than
simulating one. The phase's stated job was to prove the architecture end
to end, and it does — a real binary on a real pty walks the demo lesson,
grades a wrong answer, and exits 0 on its own.

## The seam: an interceptor, not a fork

`internal/repl.loop` is the one place input units complete, and its
continuation / Ctrl-C / EOF handling is subtle and already correct. The
tutor rides along on it instead of copying it:

```go
type Interceptor interface {
	BeforePrompt(w io.Writer)        // once per input *unit*
	AfterEval(src string, err error) // grade, after the loop reports errors
	Done() (code int, done bool)     // interceptor ends the session itself
}
```

`loop` grew a sixth parameter; `Run` is unchanged for its existing caller
and now delegates to `RunOptions(sess, repl.Options{...})`, which carries
`Interceptor`, `Quiet`, `Ephemeral` and `NoRC`. Both entry points share
one body, so the tutor exercises exactly the code path a real user gets.

Two departures from the plan, both deliberate:

- **`Done` was added.** The plan named two methods. With only two, a
  finished lesson has no way to leave the loop — the student would be
  dropped at a prompt with nothing left to do. `Done` is polled at the top
  of each iteration, *before* the panel, so the closing banner is the last
  thing on screen.
- **`AfterEval` fires for `replCommand`-handled units too** (`?name`,
  `session save`). Those never reach `Eval`, but "now inspect that
  variable" is a lesson step the curriculum plans to teach (chapter 3),
  and the capstone is literally `session save`.

Hook sites sit outside the continuation and interrupt branches on
purpose: a lesson panel belongs to an input *unit*, not to each physical
line typed into one.

## The tee: what the plan expected to cost more than it did

The plan flagged as a risk that `internal/runner.Options` might not expose
`Stdout`/`Stderr` the way root `grsh.Options` does. It already did
(`internal/runner/session.go:31`), so the tee was pure addition — no
runner change at all.

`internal/tutor/capture.go` is the buffer half. Two constraints shaped it:

- Writes arrive from `os/exec`'s copier goroutines while `Eval` runs, so
  every method takes a mutex. The embedding contract in `session.go`
  already warns hosts that writers must be goroutine-safe.
- A step that prints a lot must not grow the process without bound, so it
  keeps a 64 KB tail window, sliding with `copy` rather than reallocating.

The engine calls `Reset` in `BeforePrompt`, which is what makes grading
per-attempt rather than cumulative. `TestCaptureIsPerAttempt` guards it
directly: it rewrites step 2's verifier to demand output step 1 already
produced, so a leaking buffer would pass step 2 with the student typing
something else entirely.

## Grading reads, never writes

`Attempt` carries the input, the captured output, the eval error, and the
session — and nothing that would let a verifier run something. No hidden
`Eval` in the student's session: that would pollute `$?`, history, and
their trust in what the prompt just did. Everything grades through
surfaces that already exist.

`verifierKinds` is a table a `verify:` line resolves against, so adding a
kind is one map entry plus a type and never an engine change. Phase 1
ships the two the demo needs:

| kind | note |
|---|---|
| `any-input` | demo/observe steps, and the ones the plan keeps ungraded on purpose (Ctrl+Z / `fg`) |
| `output-regexp` | matches the **trimmed** output, no implicit flags — `^hello$` works for `echo hello` without every content file spelling the newline |

`output-regexp` fails when the eval errored, even if the text matched.
The whole point of `ls missing && echo ok` is that the `&&`
short-circuits.

## The engine

`internal/tutor/tutor.go` is the state machine. Panel once per step (not
per prompt — a bare Enter or a `^C` must not re-post it), a tick on pass,
and escalation on misses: silence on the first, then hints one at a time,
then the answer once the hints run out. Two misses is where a stuck
student is still trying but no longer learning from the silence.

A failed attempt is not an error state. The student's command really ran,
its output and exit status are real, and the shell is exactly where they
left it. The only thing a miss costs is the step not ticking over — which
is the argument for grading in a live session rather than a sandboxed
imitation of one.

One wart worth naming: `BeforePrompt` is handed the loop's writer and
ignores it, because `AfterEval` has no writer parameter. Everything the
engine prints goes to one sink (`engine.out`); feedback interleaved
across two sinks would reorder unpredictably when one is a buffer (tests)
and the other a terminal.

## Session construction, and why the tutor owns it

`tutor.Run` builds its own session rather than taking one from `main`,
because it needs the writers teed. It also sets:

- **`NoRC`** — someone else's alias for `ls` breaking step 1 is a bad
  first impression of the language.
- **`Ephemeral`** — a lesson must not pollute `~/.grsh_units`. The
  in-memory store still works, so `session save` stays available for the
  capstone chapter.

`grsh tutor [chapter]` dispatches in `main.go` beside `init`, before any
session is built. It validates the chapter number *first*, then requires
a terminal — a typo deserves to be reported as a typo even when the tutor
could not have run anyway.

## Verification

The pty test is the phase's actual deliverable:

```
TestTutorEndToEnd (internal/repl/e2e_pty_test.go)
  builds the real binary, attaches a pty, drives the demo lesson
  - a wrong answer PRINTS ITS OUTPUT and is graded a miss (no advance)
  - three correct answers, each advancing to the next panel
  - "Lesson complete." and a clean exit with NO ^D from the test
```

Alongside it, `TestContentSolutionsPass` runs every shipped step's
canonical solution through a live session against that step's own
verifier. It is trivial today with three steps; it is the test that makes
content contributions safe in Phase 3, and it exists now so the content
never lands ahead of its guard.

Seam tests pin the risk the plan called out — that the interceptor might
disturb continuation or Ctrl-C:

- a 4-line block is **one** panel and **one** grade, not four of each
- `^C` mid-continuation drops the unit with no phantom grade, and the
  next unit gets a fresh panel
- eval errors reach `AfterEval` carrying the error
- `Done` ends the loop with its own code while lines are still queued

Plus engine tests for hint escalation, panel idempotence, the verifier
table, `ParseVerifier` rejections, the capture window, chapter parsing,
and a check that no ANSI escape leaks when color is off.

`go build ./... && go vet ./... && go test ./...` — all green.

## Files

```
internal/repl/repl.go            Interceptor, Options, RunOptions, loop hooks
internal/repl/repl_test.go       runIntercepted harness + 5 seam tests
internal/repl/e2e_pty_test.go    TestTutorEndToEnd
internal/tutor/tutor.go          engine (repl.Interceptor), Run, style
internal/tutor/lesson.go         Lesson/Step types, demo lesson
internal/tutor/verify.go         Attempt, Verifier, kind table
internal/tutor/capture.go        goroutine-safe windowed output tee
internal/tutor/tutor_test.go     content self-check + engine/verifier tests
cmd/grsh/main.go                 `grsh tutor [chapter]` dispatch
```

## Open

- **`Done()` always returns exit code 0.** Once `:quit` exists (Phase 2),
  an abandoned lesson may want a nonzero code. The signature already
  carries it; nothing decides it yet.
- The demo's bridge step grades on output alone. Forcing the intended
  mechanism wants `used-construct` (must contain `$(`), which lands with
  the rest of the verifier suite.
- The plan's progress-persistence question (`bytdb` vs. a plain
  `~/.grsh_tutor.json`) is untouched and still open for Phase 2.
