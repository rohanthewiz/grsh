# Session: Phase 3 — The Curriculum

Session: 35d5ec70-dc3b-42a4-a312-43a24a296059
Date: 2026-08-30

Phase 3 of `ai_docs/plans/interactive-tutorial-plan.md`. Phases 1 and 2
built an engine with nothing to teach. This phase wrote the eight
chapters — 47 steps — and, to do it, turned lessons from Go source into
embedded data and taught the content self-check to run a chapter the way
a student does.

## Chapters are data now

`demoLesson()` is gone. `internal/tutor/content/NN-slug.md` is
`go:embed`'ed and parsed by `lesson.go`, because the plan's reason for
that was the right one: writing a lesson must not mean touching engine
code, and a chapter that reads as prose in an editor is a chapter people
will actually edit.

The format is one structural rule and four directives:

```markdown
# It's just a shell

## step: pipe-count
Pipes work exactly as they do in bash.

Count the Go files here **with a pipe**.
---
verify: used-construct \|
verify: output-regexp (?m)^\s*3\s*$
hint: `ls *.go` lists them; `wc -l` counts lines.
solution: ls *.go | wc -l
```

Three decisions inside it are worth naming.

**Directives repeat instead of nesting.** Several `hint:` lines are the
ordered hint list; several `verify:` lines are conjoined through Phase
2's `All`. That was the point of building conjunction before there was
content to use it: "capture it with `$(...)` and print 17" is two
clauses, and grading only the second ticks over for a student who typed
`echo 17`. Fourteen steps use two or three clauses; none needed new
syntax.

**An empty value opens an indented block.** Chapter 3's answer is a real
three-line `for`, and a format that could not hold one would have quietly
banned the multi-line lesson the REPL's breadcrumb and auto-indent exist
for:

```markdown
solution:
  for _, f := range files {
      fmt.Println(f)
  }
```

The block is dedented by its own first line, so the file stays readable
while the student sees the answer at column zero. Blank edges are
trimmed — the blank line before the next `## step:` is layout, not part
of the answer.

**The lesson ID is the filename.** `03-go.md` → `03-go`, which is what
progress records key on. `fs.Glob` sorts and the numbers are padded, so
chapter order needs no index kept in sync. Renaming a file resets that
chapter's saved place, which is the honest outcome: its steps moved too.

`lessons()` is a `sync.OnceValue` that **panics** on a malformed
chapter. The content is compiled into the binary and `TestContentParses`
guards it, so the panic is unreachable in a shipped build — and a
half-loaded curriculum that silently skipped chapter 5 would be far worse
than a loud failure.

`content/FORMAT.md` is the authoring guide, and it carries the
`var`-anchoring warning Phase 2 left open (`type=int` also matches
`interface{}`; write `type=^int$`) plus the one grammar wart found while
writing chapter 6: `var` predicates are whitespace-separated, so a value
with a space in it writes `value=^old\slogs$`.

## The self-check now runs a chapter as a chapter

This is the real change of the phase, and it reverses a Phase-2 decision
deliberately.

Phase 2 ran each step's solution in its own fresh session and playground,
with a comment saying that steps whose solutions compose are "a content
design question, not something to paper over." Writing the content
answered that question the other way. Chapter 4 captures a count into
`errs` and splices it back three steps later. Chapter 2 declares `name`
in step 3 so that step 4 can prove rule 7 sees a *declared* identifier.
The capstone builds a report, saves the session as a script, and sources
it again. Per-step isolation would forbid exactly the composition the
language exists for.

So `TestContentSolutionsPass` now runs one chapter per subtest: one fresh
playground, one session, solutions submitted in order, capture reset
between steps exactly as `BeforePrompt` does it. That is the only thing a
student can do, and it is now the only thing CI does.

### `repl.UnitLog`

Two steps are prompt affordances, not language — `?count` in chapter 3
and `session save 3 report.grsh` in the capstone — and neither reaches
`Eval`. A test that called `sess.Eval` on them would grade
"command not found."

So `internal/repl/host.go` exports the loop's post-completion dispatch,
minus the terminal:

```go
func (u *UnitLog) Submit(src string, sess *runner.Session, outW, errW io.Writer) error {
    if replCommand(src, sess, u.h, outW, errW) { return nil }  // ?name, session save
    u.h.Append(src)
    return sess.Eval(src)
}
```

Three lines, and they are the same three lines `loop` runs. The
alternative — reimplementing the dispatch in the test — could pass while
the real prompt diverged, and the capstone is precisely where that would
go unnoticed, because its evidence is a file written by the branch under
test. It is deliberately not a headless REPL: no editor, no continuation
state, no interceptor. Deciding when a unit is *complete* stays in `loop`.

## The curriculum

| # | chapter | steps | the load-bearing verifier |
|---|---|---|---|
| 1 | It's just a shell | 6 | output — grep, pipe, `>`, `&&`, awk in single quotes |
| 2 | Two languages, one prompt | 6 | `classified-as`, one step per rule (9, 5, 6, 7, 8, 2) |
| 3 | Go at the prompt | 6 | `var`, plus `?name` and a real multi-line `for` |
| 4 | The bridge | 8 | `used-construct` + result, both directions |
| 5 | Helpers and the registry | 6 | mixed; `file` for `writeFile` |
| 6 | Where bash habits break | 5 | `status nonzero` on two *deliberately triggered* errors |
| 7 | Jobs | 5 | `used-construct` and two `any-input` demos |
| 8 | Capstone: session → script | 5 | `file` on the saved script, then its own output |

Chapter 2 is the one no other shell tutorial can have: it walks the
classification rules in order and grades each with `classified-as`, which
asks the classifier the same question the REPL asked a moment earlier.
Step 4 is the one that pays for the sequencing — `name = "grace"` is Go
*only because* step 3 declared `name`, and a student who skipped step 3
sees the rule fail to apply.

Chapter 6 asks the student to break things on purpose: a bare `FOO=bar`
and a `${HOME:-/tmp}`. Both are graded `used-construct` (they typed it)
AND `status nonzero` (it really was refused), and the step's whole
content is the hint the shell prints back. It closes on the replacement
grsh offers, `iff(env("NOPE") == "", "fallback", env("NOPE"))`.

Chapter 8 closes the thesis in five steps: two captures into ints, a
`fmt.Printf` report line, `session save 3 report.grsh`, then
`source report.grsh` — graded on the report line appearing a second time.
`session save 3` rather than a bare save is the difference between a
tight script and one that replays every miss; the `[N]` form was already
there for exactly this.

Two content-shaped facts made steps possible that otherwise weren't:
re-declaring with `:=` is accepted (so sourcing the script back into the
live session works), and a Go line does not disturb `LastStatus` (so
"fail, then read `status()`" is two steps rather than a race).

## Panel rendering

Lesson prose is markdown, so the panel renders markdown's two inline
marks. The interesting half is what happens with color off:

- **Backticks stay.** They are the only emphasis a plain terminal has,
  and a `NO_COLOR` reader who lost them would read `ls *.go | wc -l` as
  ordinary prose.
- **Stars go.** Emphasis has no plain-text convention worth preserving;
  literal asterisks are just noise around the word they meant to lift.

Spans don't nest and an unclosed mark is left exactly as written rather
than swallowing the rest of the line. A `try:` may now be a block, with
the label on the first line and the rest aligned under it.

## Fixtures

One addition: `old logs/legacy.log`. Chapter 6's first two steps are
`d := "old logs"` then `ls {d}`, and the point — a string is one
argument, no `"$d"` to remember — needs a real directory with a real
space in its name to be anything but a claim.

## Verification

```
go build ./... && go vet ./... && go test ./... && go test -race ...
```
all green.

- `TestContentSolutionsPass` — 8 chapters, 47 solutions, in order, in
  a live session each.
- `TestContentParses` — 8 chapters embedded, each with a title and a
  plausible step count; turns `lessons()`'s panic into a failing build.
- `TestParseLessonShape` / `ConjoinsVerifies` / `Rejects` — the format's
  whole surface, including six malformed chapters that must not parse.
- `TestInlineCodeSpans` — both marks, both color states, unpaired
  backtick.
- `TestTutorEndToEnd` (pty, real binary) now walks **all of chapter 1**:
  a miss that must not advance, then six passes, then the banner and a
  clean exit. The last step is graded `output-exact`, which is what makes
  the deterministic playground load-bearing rather than decorative.

## Files

```
internal/repl/host.go            UnitLog: the loop's unit dispatch, off-terminal
internal/repl/e2e_pty_test.go    both tutor pty tests walk real chapter 1
internal/tutor/lesson.go         go:embed + parseLesson (replaces demoLesson)
internal/tutor/lesson_test.go    format coverage + inline rendering
internal/tutor/content/*.md      8 chapters, 47 steps
internal/tutor/content/FORMAT.md the authoring guide
internal/tutor/tutor.go          inline() markdown spans; multi-line `try:`
internal/tutor/tutor_test.go     per-chapter cumulative self-check; TestContentParses
internal/tutor/sandbox.go        `old logs/legacy.log` fixture
internal/tutor/progress_test.go  resumeAt uses a fixture, not shipped content
ai_docs/plans/…-plan.md          Phase 3 done; two risks closed
```

## Open

- **The graduate keeps nothing.** `report.grsh` is written inside the
  playground, which is deleted on exit. The plan had the capstone write
  `~/.grsh_tutor_graduate.grsh`; leaving files in someone's home at the
  end of a tutorial is a surprise, and `source` closes the loop without
  it. If the script is worth keeping, offer it at the outro rather than
  writing it silently.
- **`grsh tutor` with no argument always starts chapter 1.** With eight
  chapters this is now the rough edge Phase 2 predicted: the records are
  keyed to support resuming the furthest chapter, nothing reads them that
  way. Phase 4.
- **`:menu` still cannot jump** — same reason as before (a chapter switch
  needs a fresh sandbox and session), and with eight chapters the
  `grsh tutor N` restart is worth reconsidering in Phase 4.
- **Chapter 2 never turns `--explain` on**, though the plan wanted it
  automatic for that chapter. The prose points at it instead. Turning it
  on per chapter means a knob the interceptor doesn't have yet.
- README section and the golden-transcript test remain Phase 4.
