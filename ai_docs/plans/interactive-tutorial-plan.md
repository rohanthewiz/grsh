# Plan: `grsh tutor` — an interactive, in-REPL tutorial

*Drafted 2026-08-30.*

## Concept

A vimtutor/Tour-of-Go-style tutorial that runs **inside the real REPL**, not a
simulation of it. The user types into the actual shell with all v2
conveniences live (highlighting, ghost text, breadcrumb, hint line), while a
lesson engine sits around the loop: it prints a lesson panel before the
prompt, watches what the user evaluates, grades the result, and advances.

A web-based tour behind rweb was deferred to an optional later phase on the
theory that the embedding API would make it nearly free. Phase 5 built it,
and the theory was half right: the transport was cheap, but the engine had
to grow a host-facing driver (`tutor.Driver`) and a data view of its state
(`tutor.View`) first, and it went through `runner.Session` rather than the
public embedding API — the verifiers grade through read-only surfaces the
public API does not expose.

Why in-REPL: the product's whole pitch is "interactive work and scripts are
the same language." Teaching in the real REPL means every convenience (the
`--explain` hint, `?name` inspection, `session save`) becomes part of the
curriculum instead of something to describe.

## Architecture

```
grsh tutor [chapter]                       (cmd/grsh — same dispatch style as `grsh init`)
   │
   ▼
internal/tutor/
  tutor.go      engine: step state machine, wraps the REPL loop
  drive.go      Driver: the same engine for a host with no line editor
  view.go       View: the engine's state as data, for a non-text surface
  lesson.go     Lesson / Step types + loader
  verify.go     verifier implementations (output, status, var, file, classify)
  sandbox.go    playground dir with fixture files; cd in, clean up on exit
  progress.go   resume support
  content/      go:embed'ed lesson files (01-shell.md … 08-scripts.md)
  content_test.go  every step's canned solution must pass its own verifier

internal/tour/   (Phase 5)
  tour.go       rweb server: routes, visitor sessions, the loopback guard
  visitor.go    one student: a Driver, a lock, an output stream
  sink.go       the transcript: io.Writer in, SSE + bounded replay out
  assets/       index.html / app.css / app.js — the page draws both surfaces
cmd/grsh-tour/  its own binary, so the shell links no web framework
```

Four integration seams, all small:

1. **REPL loop hook** — `internal/repl.loop` (repl.go:126) is the one place
   input units complete. Refactor it to accept an optional interceptor with
   two methods: `BeforePrompt(w io.Writer)` (print the lesson panel /
   progress bar) and `AfterEval(src string, err error)` (grade, advance,
   print "✓ nice" or a hint). `repl.Run` grows a variant that takes this
   interceptor; the normal REPL passes nil. This keeps the tutor from forking
   the loop's continuation/Ctrl-C/EOF logic, which is subtle and already
   correct.

2. **Output tee** — verifiers need to see what the user's command printed
   without hiding it. Route the session's stdout/stderr through
   `io.MultiWriter(os.Stdout, rollingBuffer)` (the embedding API already
   accepts writers per `grsh.Options`; `internal/runner.Options` needs the
   same fields exposed if they aren't yet — check
   `internal/runner/session.go`). The buffer is cleared before each eval so
   a verifier only sees the current attempt.

3. **Session introspection for grading** — no hidden `Eval` calls in the
   user's session (that would pollute status, history, and trust). Grade
   only through existing read-only surfaces: `sess.Inspect(name)` for
   variable checks, `sess.LastStatus()`, `sess.Idents()`, the output buffer,
   and direct filesystem checks in the sandbox.

4. **Tutor meta-commands** — extend the `replCommand` pattern
   (internal/repl/commands.go:21) inside the tutor loop with a colon prefix
   that can't collide with shell or Go: `:hint`, `:skip`, `:sol` (show the
   canonical solution), `:back`, `:menu` (chapter list + jump), `:progress`,
   `:quit`. The engine handles these before classification ever sees them.

## Lesson format

Lessons as embedded data, not Go structs — content iteration must not
require touching engine code. One markdown file per chapter with a light
front-matter-per-step convention:

```markdown
## step: pipe-count
Count the Go files in this directory using a pipe.
Try: `ls *.go | wc -l`
---
verify: output-regexp ^\s*3\s*$
hint: `ls *.go` lists them; pipe into `wc -l` like in bash.
hint: The playground has exactly 3 .go files.
solution: ls *.go | wc -l
```

Steps have: prose (rendered with the REPL's existing ANSI styling), a task,
an ordered hint list (`:hint` reveals the next one; auto-offer after 2
failed attempts), a solution, and one verifier line.

### Verifier kinds (verify.go, table-driven)

| kind | grades by | teaches |
|---|---|---|
| `output-regexp` / `output-exact` | tee buffer | shell + fmt lessons |
| `status` | `LastStatus()` | exit codes, `status()`, `errexit` |
| `var` | `Inspect(name)` + predicate (type, value regexp) | Go lessons (`n := 42`) |
| `file` | sandbox fs check (exists, content regexp) | redirection, `writeFile` |
| `classified-as` | run `classify` on the input | the classification chapter |
| `any-input` | just advance | demo/observe steps |
| `used-construct` | input matches a regexp (e.g. must contain `$(`) | forcing the intended mechanism |

`classified-as` is the special one worth building: the classification
chapter can say "make this line run as *shell*" and grade the user's
understanding of rules 1–6 directly — no other shell tutorial can do that.

## Sandbox

`sandbox.go` creates `$TMPDIR/grsh-tutor-*` and seeds fixtures so every
exercise is deterministic: a fake `access.log` (with some 500 lines, for the
README's own grep example), three small `.go` files, a `data.json`, a
`notes/` subtree for glob/filepath work. The tutor session starts `cd`'d
there; `:quit` and normal exit clean it up (keep it on crash for debugging).

Phase 3 added an `old logs/` directory whose name contains a space, for
chapter 6's word-splitting steps, and settled the capstone's output: it
writes `report.grsh` **inside the playground** and sources it back, rather
than leaving `~/.grsh_tutor_graduate.grsh` in the student's home. Nothing
in the curriculum touches the user's real files.

## Curriculum (8 chapters, mapping to docs/LANGUAGE.md)

1. **It's just a shell** — pipes, redirection, `&&`/`||`, quoting. Builds
   trust: everything you know works. (5–6 steps)
2. **Two languages, one prompt** — the classification rules, `--explain`
   hint turned on automatically for this chapter, escape hatches (`sh `,
   leading `(`). Uses `classified-as` verifiers. (6 steps)
3. **Go at the prompt** — `:=`, `if`/`for`, `fmt`, multi-line continuation
   (teaches the breadcrumb and auto-indent by making the user write a real
   loop), `?name` inspection. (6 steps)
4. **The bridge** — `$(cmd)` capture (one- and two-value), `{expr}`
   interpolation, `[]string` splicing, `status()`/`ok()`/`errexit(true)`.
   The heart of the product; most steps here. (7–8 steps)
5. **Helpers & registry** — `glob`, `lines`, `fields`,
   `readFile`/`writeFile`, `json.Parse`, `strings.*` with the signature hint
   line pointed out. (6 steps)
6. **Where bash habits break** — `$VAR` never word-splits, bare `FOO=bar`
   rejected (user is *asked to trigger the error* and read the hint),
   `${VAR:-default}` rejection, `$?` → `status()`. Verifying that errors
   teach is the point of this chapter. (5 steps)
7. **Jobs** — `&`, `jobs`, `wait`, `kill %N`. Ctrl+Z/`fg` are demo-style
   steps (`any-input`) since grading a terminal handoff is fragile.
   (4–5 steps)
8. **Capstone: session → script** — user builds a small log-report pipeline
   across several steps, then `session save report.grsh`, then the tutor
   *runs the saved script* and grades its output. Closes the loop on the
   core thesis. (4 steps)

~45 steps total, 30–40 minutes.

## Progress & resume

Persist chapter/step + per-step attempt counts so `grsh tutor` resumes where
you left off and `grsh tutor 4` jumps to a chapter.

**Decided (Phase 2): `bytdb`**, at `~/.grsh_tutor.db`, per the standing
preference. It costs three module lines (bytdb, btypedb, tidwall/btype) for
one row, and the alternative — a plain JSON file matching the
`~/.grsh_history` / `~/.grsh_units` conventions — remains a one-file swap
because `progress.go` keeps the choice behind a `Store` interface. The
engine API (`Open`/`CreateTable`/`Insert`/`Update`/`Get`) is used rather
than the SQL front door: one row with a known primary key buys nothing from
a parser and planner. Every failure path degrades to a `nopStore` — losing
progress must never cost a student their lesson.

## Testing

- **Content self-check** (`content_test.go`): load every lesson, run each
  step's `solution` through a headless session + its own verifier — a step
  whose solution doesn't pass fails CI. This is the single highest-value
  test; it makes content contributions safe.
- **Engine unit tests**: verifier table tests; state machine transitions
  (`:skip`, `:back`, wrong-then-right, hint escalation).
- **E2E**: scripted-stdin run of chapter 1 end-to-end (precedent:
  `cmd/grsh/main_test.go` and `internal/repl/e2e_pty_test.go`), asserting
  the completion banner and exit code 0.
- **Golden**: one golden transcript of a short chapter to catch accidental
  prose/panel regressions.

## Phases

1. ~~**Engine skeleton**~~ — **done (2026-08-30).** Loop interceptor seam
   (`repl.Interceptor` / `repl.RunOptions`, repl.go), output tee
   (`tutor/capture.go`), `Step`/`Lesson` types, `any-input` +
   `output-regexp` verifiers behind a kind table, hardcoded 3-step demo
   lesson, `grsh tutor [chapter]` dispatch in main.go. Proven end-to-end by
   `TestTutorEndToEnd` (real binary on a real pty) plus the content
   self-check `TestContentSolutionsPass`. Notes for Phase 2:
   `runner.Options` already exposed Stdout/Stderr, so no runner change was
   needed; the tutor session runs with `NoRC` and in-memory history so a
   user's `~/.grshrc` can't break a lesson.
2. ~~**Verifier suite + sandbox + meta-commands**~~ — **done
   (2026-08-30).** All eight verifier kinds behind the table
   (`output-exact`, `status`, `var`, `file`, `classified-as`,
   `used-construct` joined the two Phase-1 kinds), plus an `All`
   conjunction so a step can demand the mechanism AND the result — the
   demo's bridge step now uses it, closing Phase 1's open item. The
   `var` kind grades through a new read-only `Session.VarInfo` /
   `Interp.InspectParts` (type and value as separate raw strings) rather
   than parsing `?name`'s rendered line, so lesson regexps never depend
   on the inspector's cosmetics. `sandbox.go` builds a deterministic
   playground (`$TMPDIR/grsh-tutor-*`: a 120-line `access.log` with 17
   500s, three `.go` files, `data.json`, a `notes/` subtree) and chdir's
   the process into it; teardown is explicit, not deferred, so a panic
   leaves it for debugging. Meta-commands (`:hint :sol :skip :back
   :menu :progress :help :quit`) ride a fourth interceptor hook,
   `Command(src) bool`, placed ahead of `replCommand`, `Eval` AND
   `hist.Append` — a `:hint` must never land in the unit history the
   capstone turns into a script. Progress persists to `~/.grsh_tutor.db`
   via bytdb's engine API (decision below resolved in favor of the
   standing preference), keyed by lesson and storing a step *ID* so
   editing content can't teleport a returning student. Notes for Phase
   3: `Attempt` now carries `Dir` (the sandbox root, for `file`); the
   content self-check runs each solution in its own fresh playground.
3. ~~**Content**~~ — **done (2026-08-30).** The 8 chapters, 47 steps, as
   `go:embed`'ed markdown under `internal/tutor/content/` with a loader in
   `lesson.go`, so writing a lesson never means touching engine code.
   Format is one `## step: id` heading per step, prose to a `---`, then
   repeatable directives (`verify:` lines conjoin via `All`, `hint:` lines
   are the ordered list, an empty value opens an indented block for a
   multi-line answer); `content/FORMAT.md` is the authoring guide,
   including the `var`-anchoring note Phase 2 left open. Panel prose
   renders markdown's two inline marks — backticks in the code colour
   (kept literal under `NO_COLOR`, the only emphasis a plain terminal
   has) and `**stars**` in bold (dropped under `NO_COLOR`, since
   emphasis has no plain-text convention). `TestContentSolutionsPass` now
   runs a chapter AS a chapter — one playground, one session, solutions in
   order — because the curriculum composes on purpose (chapter 4 captures
   into a variable it splices three steps later; the capstone saves the
   session and sources it back), and per-step isolation would forbid
   exactly what the language is for. Units go through a new
   `repl.UnitLog`, so `?count` and `session save` are checked on the same
   dispatch a student's keystrokes take rather than a reimplementation.
   One deviation from the sketch above: the capstone writes `report.grsh`
   inside the playground and closes the loop with `source report.grsh`
   rather than writing into the student's home — a tutorial leaving files
   in `~` is a surprise, and sourcing proves the round trip in the same
   session that made it.
4. ~~**Polish**~~ — **done (2026-08-30).** The engine stopped being a
   one-chapter program. `tutor.Run` is now a chapter LOOP: each chapter
   gets a fresh sandbox, a fresh `runner.Session` and a fresh engine, and
   a chapter change is a teardown and a rebuild rather than a lesson
   swapped under a live session (which would leave a student in chapter 5
   holding chapter 2's variables, files and cwd). That one restructure
   closed four Phase-3 open items at once: `:next` and `:menu N` jump for
   real (the engine records the target in `jump` and ends the loop
   through `Done`); chapter 2 runs itself with `Explain: io.Discard`, so
   the classification rules fire in the prompt's hint lane as the student
   types — the writer is discarded because only that half of `--explain`
   belongs in a lesson; and a finished chapter no longer ends the
   session while it still has something to offer. `Done` now says so
   directly: quit or jump end it, and a finished chapter ends it only
   when there is no next chapter AND no keepsake file pending — an outro
   that offers an action must leave the prompt alive to take it.
   Resume grew from a step to a chapter: bare `grsh tutor` calls
   `resumeChapter`, which scans the records backwards and picks the
   FURTHEST chapter touched (a student who ran `grsh tutor 6` wants
   chapter 6 back, not chapter 1 because they skipped the basics),
   carrying on to the next chapter when the furthest one is finished and
   starting over only after the last. The lesson format grew two
   chapter-level directives in the front matter — `explain: on` and
   `keep: report.grsh` — claimed by key so the rest of that region stays
   the editor's commentary. `:keep` closes Phase 3's last open item: the
   capstone's script is OFFERED at the outro and copied out on request
   (home by default, `~` expanded, a directory argument taking the
   file's name, never overwriting), instead of the plan's original
   silent write into `~`. Tests: `testdata/chapter01.golden` is the
   golden transcript (intro, panels, a miss, the earned hint, the ticks,
   the outro; `-update` regenerates), `chapters_test.go` covers
   navigation, the resume policy and `:keep`, and two pty runs prove the
   seam end to end — `TestTutorEndToEnd` now finishes chapter 1 and
   `:next`s into a live chapter 2, and `TestTutorResumesEndToEnd` runs
   two processes over one `$HOME` to prove the record survives the
   first. README has the section.
5. ~~**Web tour**~~ — **done (2026-08-30).** The same eight chapters in a
   browser, with no second copy of anything: the lesson engine was never
   really coupled to the REPL, it was coupled to four hooks around an
   input unit, so a second host only had to call them. `tutor.Driver` is
   that host-facing driver — it reproduces repl.loop's call sites
   (notifications, `Done`, `BeforePrompt`, the continuation buffer,
   `Command` ahead of everything, `repl.UnitLog.Submit`, `AfterEval`)
   for a host that has no line editor, and `tutor.View` hands out the
   engine's state as data so a sidebar can render the lesson instead of
   scraping it back out of the transcript. `internal/tour` is rweb over
   that: `POST /input` takes a line, an SSE stream carries `out` (the
   shell's bytes, escape codes intact, rendered to spans in the page)
   and `state` (the View as JSON). The step panel is routed to
   io.Discard because the sidebar already says it; the ticks, hints and
   outro stay inline where they belong.
   Deviations from the sketch: it is a SEPARATE BINARY (`cmd/grsh-tour`)
   rather than a `grsh tour` subcommand — reaching rweb from grsh's main
   would link a web framework into every copy of the shell — and it uses
   `runner.Session` directly rather than the public `grsh.NewSession`,
   because the verifiers grade through read-only surfaces
   (`VarInfo`, `LastStatus`, `Preview`) that the embedding API does not
   expose. Chapter 2 keeps its lesson: `--explain`'s hint lane has no
   prompt to live in, so `Driver.Classify` hands the verdict to the page
   and it appears under the input as you type.
   The hard constraint is documented rather than papered over: a grsh
   session's cwd IS the process's cwd, so drivers take turns under one
   eval gate that re-enters each student's playground. That serves a few
   tabs on one machine, and one student's `sleep 30` delays another's
   next command. It binds to loopback and refuses more without
   `-allow-remote`, since it runs shell commands as the user.
   Three bugs found by running it that no Go test could have caught
   alone, all now pinned: rweb labelled SSE responses `Content-Encoding:
   text/plain` (a media type where a content coding belongs), which Go's
   http client ignores and every browser treats as an undecodable body —
   headers arrived, events never did (fixed upstream in rweb v0.1.28,
   which sends no such header; the tour's local override is gone and only
   the assertion remains); rweb's `Run` installs its own
   SIGTERM handler and returns, so a second handler in main raced it and
   lost, leaking a playground per Ctrl+C; and the table of contents
   ticked off every chapter *before* the current one, congratulating a
   student who had jumped for work they never did.

## Risks / open questions

- ~~**Loop refactor**~~: settled. The interceptor does not disturb
  continuation (`Pending`) or Ctrl+C semantics; the repl seam tests plus two
  pty end-to-end runs (`TestTutorEndToEnd`, `TestTutorMetaCommandsEndToEnd`)
  guard it.
- ~~**Output-based grading brittleness**~~: handled in content rather than
  in the engine — `FORMAT.md` says to prefer `var`/`file`/`status` where a
  step allows the choice, and the chapters do. The remaining output checks
  run against the deterministic playground and are regexps, except three
  `output-exact` steps whose bytes are the lesson.
- ~~**`runner.Options` writers**~~: it already exposed them; the tee was
  pure addition.
- ~~**Jobs chapter timing**~~: chapter 7 backgrounds `sleep 30`, lists it,
  kills it with `%%` (no job number to guess), lists again, and ends on a
  `wait` with nothing left to collect. Nothing in it waits on a timer, and
  the last two steps are `any-input` demos as planned. Ctrl+Z / `fg` / `bg`
  are described for after the tutor rather than graded — a terminal
  handoff is not something a scripted check can honestly assert.
