# Session: Phase 2 — Verifiers, Sandbox, Meta-commands, Progress

Session: 5eaeb4f8-94c1-40d9-8de1-373e5218806e
Date: 2026-08-30

Phase 2 of `ai_docs/plans/interactive-tutorial-plan.md`. Phase 1 proved
the architecture with two verifier kinds and a hardcoded demo. This phase
built the machinery the curriculum will actually be written against: the
full verifier table, a deterministic playground, the tutor's own
vocabulary, and a place to save.

## A fourth hook, and where it sits

Meta-commands can't be graded — they have to be *claimed* before anything
else touches the line. So `repl.Interceptor` grew a fourth method:

```go
Command(src string) bool   // true = handled; skip the rest of the unit
```

Its position in `loop` is the whole design:

```
unit completes
   │
   ├─ ic.Command(src)   ← here.  ahead of replCommand, Eval, AND hist.Append
   ├─ replCommand       (?name, session save)
   ├─ hist.Append
   └─ sess.Eval → ic.AfterEval
```

Ahead of `hist.Append` is the non-obvious one. The capstone chapter turns
unit history into a runnable script via `session save`; `:hint` is not a
line anyone wants replayed. `TestInterceptorCommandClaimsUnit` asserts
exactly that — the claimed unit reaches neither the shell nor the history.

The colon prefix is free because nothing else can start with it: no shell
word, no Go statement. `TestColonIsACompleteUnit` pins the other half of
that claim — the classifier reports every `:cmd` as a *complete* unit, so
the student is never left at a `...` continuation prompt.

## The verifier table, completed

Six kinds joined the two from Phase 1. Each is a single-purpose predicate
with its own constructor error path:

| kind | grades | note |
|---|---|---|
| `output-exact` | trimmed output == literal | for steps where the spacing *is* the lesson |
| `status` | `LastStatus()` | `0`, `N`, or `nonzero` |
| `var` | `Session.VarInfo` | `var n type=^int$ value=^42$` |
| `file` | the sandbox on disk | `file errs.txt contains=500` |
| `classified-as` | `Session.Preview` | shell vs. go — rules 1–6, graded directly |
| `used-construct` | the student's *input* | forces the mechanism |

Two decisions inside that table are worth naming.

**`status` deliberately does not reject `a.Err`.** Every output kind
does — `ls missing && echo ok` must fail even though the text matched.
But chapter 6 asks the student to *make something fail* and then read
`status()`, so an eval error there is the expected result, not a
disqualification. `TestStatusVerifierAcceptsAFailedEval` locks the
difference in.

**`var` reads new plumbing rather than scraping `?name`.** `Inspect`
renders for a human: it quotes strings, prints `(len 3)`, and elides past
60 runes. A verifier grading that string would make every lesson's regexp
depend on the inspector's cosmetics — and worse, would pass any answer
whose first 60 runes were right. So `Interp.InspectParts` and
`Session.VarInfo` return type and value as separate *raw* strings. A
string's value is its contents, not `"contents"`, so content writes
`value=^ada$`. `TestInspectPartsIsUntruncated` and
`TestVarVerifierIsUntruncated` guard the truncation trap from both sides.

`classified-as` is the one no other shell tutorial can have: chapter 2
says "make this line run as shell" and grades the student's grasp of
classification by asking the classifier the same question the REPL asked
a moment ago. `Preview` runs on a clone, so it stays genuinely read-only
— no scope declared, no state moved.

### Conjunction instead of a richer grammar

The plan said one `verify:` line per step. The interesting steps want
two: `$(...)` capture is *the mechanism* AND *the result*, and grading
the result alone ticks over for a student who typed `echo bridge` — the
one thing that step is not about. `All(...)` conjoins verifiers, and the
demo's bridge step now uses it, closing a Phase-1 open item:

```go
Verify: MustAll(`used-construct \$\(`, "output-regexp (?m)^bridge$"),
```

Conjunction rather than a richer spec syntax keeps each kind
independently testable, and a step's repeated `verify:` lines map onto it
in Phase 3 with no new syntax to invent.

## The playground

`sandbox.go` builds `$TMPDIR/grsh-tutor-*` and **chdir's the process**
into it — not a session field, the real working directory, because the
student's `ls`, `cat` and `glob("*.go")` all resolve through it and a
lesson that lied about where it was would teach the wrong thing at the
first unresolved path.

Fixtures are generated in Go rather than embedded, because a tutorial's
fixtures are part of its logic: `accessLogErrors = 17` is a constant a
step grades against, and keeping it next to the generator means a content
change and its fixture change land in one file.

```
.
├── access.log      120 lines, 17 of them 500s   (the README's own example)
├── data.json       one small object
├── greet.go        three .go files, so `ls *.go | wc -l` is 3
├── main.go
├── util.go
└── notes/          monday.md, tuesday.md, archive/old.md
```

Deterministic by construction — no `time.Now`, no map iteration in the
log builder — because output-based grading is only as reliable as the
bytes underneath it.

Two smaller things the tests pin:

- The path is `EvalSymlinks`'d. macOS hands back `/var/...`, a symlink to
  `/private/var`; a tutor reporting one form while `pwd` printed the
  other would look like it was lying about where the student is.
- **Teardown is explicit, not deferred.** A panic unwinds defers, which
  would delete the one thing a crash report needs: the fixtures plus
  whatever the student's last command wrote.

## Meta-commands

`:hint :sol :skip :back :menu :progress :help :quit`, dispatched from a
table that `:help` renders itself. (That self-reference is why
`metaCommands` is a function, not a var — Go rejects the initialization
cycle, and populating in `init` would hide the table from the file that
documents it.)

The behaviors chosen on purpose:

- **`:sol` shows the answer but does not advance.** Reading a command and
  typing it are different acts, and only the second builds the muscle
  memory a tutorial exists for.
- **`:back` resets the step's hint state.** A student returning to
  re-read deserves the same silence they had the first time. It also
  clears `finished` — stepping out of the outro must un-finish the
  lesson, or `Done` would end the session at the very next prompt.
- **An unknown `:hnt` is still claimed.** Handing it to the shell would
  have the thing that invented colon commands reply "command not found".
- **`:quit` exits 0.** This resolves Phase 1's open question. Walking out
  of a tutorial is a choice, not a failure; a nonzero code would break
  the entirely reasonable `grsh tutor && next-thing` and teach nobody
  anything. The signature keeps the code for a future mode with an actual
  verdict.

## Progress

`bytdb` at `~/.grsh_tutor.db` — the standing preference, chosen
deliberately over a plain JSON file after raising the dependency cost
(three module lines for one row). `progress.go` keeps it behind a `Store`
interface, so the alternative stays a one-file swap.

The engine API (`Open`/`CreateTable`/`Insert`/`Update`/`Get`) rather than
the SQL front door: one row with a known primary key buys nothing from a
parser and planner, and it keeps the dependency to bytdb itself. `Save`
is an `Update` that falls back to `Insert`, since the engine separates
them.

Two properties matter more than the storage choice:

- **The record stores a step *ID*, not an index.** Editing the curriculum
  must never teleport a returning student into the middle of a step they
  have never seen. A stale ID and an empty one (lesson finished) both
  mean the same safe thing: start at the top. `TestResumeAt` covers all
  six cases.
- **Every failure degrades to a `nopStore`.** A locked database, a
  read-only home, a second tutor running — none of it may cost the
  student their lesson. "Your progress won't be saved" beats "the tutor
  won't start."

Attempts and revealed hints resume too: a student who quit stuck on step
4 comes back to the hint they had already earned, not to silence.

## Verification

```
go build ./... && go vet ./... && go test ./... && go test -race ...
```
all green.

New end-to-end proof, real binary on a real pty:

```
TestTutorMetaCommandsEndToEnd (internal/repl/e2e_pty_test.go)
  - the intro names the playground
  - `ls *.go` finds the fixtures — the prompt really is inside it
  - `:hint` is claimed by the tutor, never reaching the shell
  - `:skip` advances to step 2's panel
  - `:quit` ends the loop itself, exit 0
```

Alongside it, four new unit-test files mirroring the sources:
`verify_test.go` (every kind, both directions, plus the parse
rejections), `sandbox_test.go` (fixture counts the curriculum grades
against, determinism, cwd move and restore), `progress_test.go`
(insert/update round trip, survives reopen, degrades to nop, `resumeAt`),
`commands_test.go` (claim boundary, hint escalation, `:sol` not
advancing, `:back` un-finishing, `:quit` at 0, no ANSI leak with color
off).

`TestContentSolutionsPass` now runs each solution in its own fresh
playground, so a Phase-3 step that writes a file is graded against
fixtures in the state the student meets them. `TestContentStepsAreWellFormed`
joins it, guarding what the engine assumes but cannot enforce: unique
non-empty step IDs (a duplicate would resume the wrong step) and a
non-nil verifier.

## Files

```
internal/repl/repl.go            Interceptor.Command + its hook site
internal/repl/repl_test.go       stub Command + TestInterceptorCommandClaimsUnit
internal/repl/e2e_pty_test.go    TestTutorMetaCommandsEndToEnd
internal/interp/inspect.go       InspectParts (type/value, raw, uncapped)
internal/interp/inspect_test.go  InspectParts coverage
internal/runner/session.go       Session.VarInfo
internal/tutor/verify.go         8 kinds + All/MustAll; Attempt.Dir
internal/tutor/sandbox.go        deterministic playground + fixtures
internal/tutor/progress.go       Store, bytdb engine-API impl, nopStore
internal/tutor/commands.go       the colon vocabulary
internal/tutor/tutor.go          advance/save split, resumeAt, Run wiring
internal/tutor/lesson.go         bridge step now conjoins mechanism + result
internal/tutor/*_test.go         four new test files
ai_docs/plans/…-plan.md          Phase 2 marked done; storage question resolved
go.mod / go.sum                  + bytdb (btypedb, tidwall/btype indirect)
```

## Open

- **`:menu` lists chapters but cannot jump.** Switching chapters in place
  would need a fresh sandbox and a fresh session; relaunching as
  `grsh tutor N` is the honest way to get both. Revisit in Phase 4 if the
  restart reads as a rough edge.
- **Progress is per lesson, and there is one lesson.** With eight
  chapters (Phase 3), `grsh tutor` with no argument should probably
  resume the furthest chapter rather than always chapter 1 — the records
  are already keyed to support it, nothing reads them that way yet.
- **`var` predicates are unanchored regexps.** `type=int` also matches
  `interface{}`. That belongs in the Phase-3 content guide rather than
  being guessed at in the engine, since `type=\[\]string` needs the same
  care.
- The README section and the golden-transcript test remain Phase 4.
