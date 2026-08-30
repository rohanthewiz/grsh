# Plan: `grsh tutor` — an interactive, in-REPL tutorial

*Drafted 2026-08-30.*

## Concept

A vimtutor/Tour-of-Go-style tutorial that runs **inside the real REPL**, not a
simulation of it. The user types into the actual shell with all v2
conveniences live (highlighting, ghost text, breadcrumb, hint line), while a
lesson engine sits around the loop: it prints a lesson panel before the
prompt, watches what the user evaluates, grades the result, and advances.

The alternative — a web-based tour embedding `grsh.NewSession` behind rweb —
is deferred to an optional later phase, since the embedding API makes it
nearly free once the lesson engine exists.

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
  lesson.go     Lesson / Step types + loader
  verify.go     verifier implementations (output, status, var, file, classify)
  sandbox.go    playground dir with fixture files; cd in, clean up on exit
  progress.go   resume support
  content/      go:embed'ed lesson files (01-shell.md … 08-scripts.md)
  content_test.go  every step's canned solution must pass its own verifier
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
Exercises never touch the user's real home except the final chapter, which
deliberately writes `~/.grsh_tutor_graduate.grsh` via `session save` —
that's the capstone.

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
you left off and `grsh tutor 4` jumps to a chapter. Default per standing
preference is `bytdb`; caveat to decide consciously: it adds a dependency to
a binary that currently has six, for what is one tiny record. A plain JSON
file at `~/.grsh_tutor.json` (matching the `~/.grsh_history` /
`~/.grsh_units` conventions already in the codebase) may fit grsh's texture
better. The plan works either way — `progress.go` isolates the choice behind
an interface.

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
2. **Verifier suite + sandbox + meta-commands** — remaining verifiers,
   fixtures, `:hint/:sol/:skip/:menu`, progress persistence, content
   self-check test.
3. **Content** — the 8 chapters, iterating with the self-check test; ANSI
   panel styling consistent with the existing prompt colors, `NO_COLOR`
   respected.
4. **Polish** — resume UX, completion banner, README section, e2e/golden
   tests.
5. *(Optional, later)* **Web tour** — rweb server + `grsh.NewSession` per
   visitor with the same lesson files; the embedding API's
   `Interrupt`/`Kill` and writer-based IO were built for exactly this.

## Risks / open questions

- **Loop refactor**: the interceptor must not disturb continuation
  (`Pending`) or Ctrl+C semantics — Phase 1's demo lesson plus the existing
  repl tests guard this.
- **Output-based grading brittleness**: environment leaks into output (`ls`
  ordering, locale). Mitigation: fixtures + regexp verifiers + prefer
  `var`/`file`/`status` checks where possible.
- **`runner.Options` writers**: if the internal runner doesn't yet expose
  Stdout/Stderr the way root `grsh.Options` does, that's a small Phase-1
  addition.
- **Jobs chapter timing**: background-job steps need generous, non-flaky
  waits; keep them demo-flavored.
