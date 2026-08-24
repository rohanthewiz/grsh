# grsh Improvement Plan — Daily-Driver Terminal

## Context

grsh is a Go-based shell mixing shell commands with Go scripting (pipeline: classify → shellparse + transform → go/parser → tree-walking interp → shellexec, ~9.6k LOC). The owner wants it as their go-to daily terminal, focused on intuitive interactive features (auto-indent, hints, current-construct indicator) plus robustness and performance. Exploration found a well-architected core with clean extension seams, but also 5 real bugs that undermine daily-driver trust, and an interactive layer missing most modern conveniences.

**User decisions:** two-phase editor strategy (quick wins on chzyer/readline now, swap to reeflective/readline later); Round 1 = critical bugs + UX Phase A; all four outside-the-box features approved for the roadmap plus a new one: **`.zshrc` import/translation**; shell assignment = implement `FOO=bar cmd` prefix env, reject bare `FOO=bar` with a hint.

---

## ROUND 1 (this implementation round)

### Part 1: Critical bug fixes (in order)

**A1. Interactive `$(...)` captures must never enter job control** — verified: `interactiveTTY` (`internal/shellexec/tty.go:15-24`) gates only on stdin being a tty, so REPL captures with `bytes.Buffer` stdout take `runForegroundJobControl`, which reaps via `Wait4` and never calls `c.Wait()` → race with os/exec copier goroutine (truncated capture) + fd leak.
Fix: extend the gate — all three stdio legs must be `*os.File` (same invariant `jobSafeWriter` in `jobs.go` encodes). Stateless, protects every non-file-stdio caller. Add a debug assertion in `runForegroundJobControl` that all stdio are `*os.File`.
Tests: unit test that `interactiveTTY` returns false with buffer stdout; capture-stress test (`seq 1 20000` × 50 under `-race`, byte-exact); fd-count check.

**A2. Recover interpreter panics + fix the three panic vectors** — only the embedding API recovers today; a panic kills the interactive shell.
Fix: move the recover into `runner.Session.RunSource` (`internal/runner/session.go`) around `s.in.Run(...)` (one choke point covers REPL, -c, scripts, source, embedding; keep the embedding recover as belt-and-braces). Root-cause the vectors: nil-map write in `setIndexed` + `delete` on nil map (`internal/interp/expr.go`, `call.go`) → positioned "assignment to entry in nil map" / no-op; `make` validates non-negative len, len ≤ cap (`call.go`); `copy` pre-checks element assignability (`call.go`).
Tests: each vector returns a positioned error and the same Session still evaluates afterward.

**A3. Classifier state commits only after successful parse** — `RunSource` mutates the live classifier before parse; a failed input that opened a brace wedges the REPL demanding phantom `}`.
Fix: `cls := s.cls.Clone()` → classify/transform/`go/parser` on the clone → commit `s.cls = cls` (and tab append) only after go/parser succeeds, before `in.Run` (runtime errors still commit — declared names/side effects already happened).
Tests: failed parse of a brace-opening input, then `echo hi` classifies shell and `NeedsMore("}")` is false; runtime-error case still commits declared names.

**A4. Compound-op gaps (`<<=` hangs the REPL)** — verified: `startsGoOp` (`internal/classify/classify.go:208`) lacks `&=, |=, ^=, <<=, >>=, &^=`; `x <<= 2` classifies shell → `<<` parsed as heredoc → classifier waits forever.
Fix: add the six ops (longest-first, `&^=` before `&=`) to `startsGoOp`; add the matching cases to `interp.assignOp` (`internal/interp/expr.go`) mapping to AND/OR/XOR/SHL/SHR/AND_NOT (verify `binaryOp` covers them; add unary `^`).
Tests: classify table tests + golden `bitops.grsh`.

**A5. `{expr}` interpolation error positions** — `wordEval.EvalGoExpr` (`internal/interp/call.go:80-97`) parses with a private fileset; errors resolve against `in.fset` → wrong line. Also re-parses per expansion (cache = Round 2 perf, but do the position fix now).
Fix: `wordEval` carries the shell-call `ast.Node`; parse with a dedicated fset, swap `in.fset` during eval (defer-restore), wrap errors with `"loc" = pos(node)` + the fragment text.
Test: golden error script — `echo {undefinedVar}` on line 40 reports `:40`.

### Part 2: Correctness papercuts

**B1. `FOO=bar cmd` prefix env; reject bare `FOO=bar`** — in `runSimple`/`runPipes` (`internal/shellexec/exec.go`): strip leading `IDENT=` argv words into `c.Env = append(os.Environ(), assigns...)`; if argv empties → positioned error `shell assignment is not supported; use FOO := "bar" or export FOO=bar`. Document the declared-Go-ident precedence quirk. Goldens: `FOO=bar sh -c 'echo $FOO'` prints bar, no persistence; `dd if=... of=...` unaffected.

**B2. Reject `${VAR:-default}`-style parameter expansion** — today it silently becomes `os.Getenv("VAR:-default")` = "". In `parseDollar` (`internal/shellparse/parse.go`): non-identifier `${...}` interior → positioned "parameter expansion not supported yet" with an `iff(...)` hint. (Implementing `:-`/`##` is a possible later follow-up.)

**B3. `errexit(true)` must not exit the interactive shell** — in `runShellStmt` (`internal/interp/call.go`), skip the ExitErr conversion when `in.sh.Interactive` (matches bash interactive `set -e`). Loop test proves the REPL survives with status set.

**B4. Cleanups** — replace stale "not supported in grsh v1" strings with "not supported yet" + hints; update README's stale v1 claims; `go mod tidy` (readline/x/term/x/sys are mislabeled `// indirect`); delete dead `transform.HeaderLines`; unify duplicated `eofReader`.

### Part 3: UX Phase A (on chzyer/readline, ordered)

**U1. Unified builtin list (S)** — add `shellexec.BuiltinNames()`; delete the drifting hardcoded `shellBuiltins` in `internal/repl/completer.go:18-21`.

**U2. `pkg.Member` completion (S)** — add `stdlibreg.Members(pkg) []string` (union of Symbols+Bound keys, sorted); new branch in `completer.Do` so `fmt.Pr<TAB>` → `Print/Printf/Println/…`. Pure completer test.

**U3. `~/.grshrc` (S)** — in `repl.Run` before the loop: `sess.RunFile($GRSH_RC or ~/.grshrc)` (RunFile exists); missing file silent, errors printed and continue; `-norc` flag in `cmd/grsh/main.go`. Rc mixes shell (aliases/exports) and Go (helper funcs) — document.

**U4. `.zshrc` import (M)** — *new, user-requested.* `grsh init` (or `--init-from-zsh`): read `~/.zshrc` (and `~/.zprofile`/`~/.zshenv` if present) and translate the most-used zsh constructs into a generated `~/.grshrc`:
- `alias x='...'` → same (grsh has aliases) — flag aliases containing redirections (unsupported) as comments
- `export K=V`, `PATH=...:$PATH` / `path+=(...)` → `export` lines
- `setopt`/`unsetopt`, `bindkey`, `zstyle`, completions, `autoload` → skipped with a one-line comment
- simple one-line functions → grsh Go funcs where trivially translatable, otherwise emitted as commented TODO blocks with the original source
- `source`/`.` of plugin managers (oh-my-zsh, zinit) → commented with a note
Never overwrite an existing `~/.grshrc` (write `~/.grshrc.new` and say so). New file `internal/zshimport/` with a small line-oriented translator + table tests over sample .zshrc fixtures.

**U5. Construct-aware continuation prompt + visual auto-indent (M)** — flagship. New `classify.Pending(src) PendingInfo{NeedsMore bool; Depth int; Constructs []string}` — non-mutating (Clone-based), subsumes `NeedsMore` so the REPL does one classify pass instead of two. Maintain a `blockStack` pushed/popped where `opensBlock`/brace tracking fires (`internal/classify/golines.go`), labels like `func greet`, `for`, `if`, `heredoc <<EOF`. Continuation prompt renders `… func greet ▸ for ▸ ` + two spaces per depth (indent lives in the prompt string — history stays clean). Surface via `runner.Session.Pending`. Tests via the existing prompt-recording fakeReader (`internal/repl/repl_test.go`) + classify table tests.

**U6. Error carets (S)** — *approved wild idea, fits here.* On eval error, `userMsg` already parses `file:line:col` — reprint the offending source line from the just-evaluated buffer with a `^` caret at the column (colorized when color is enabled). Small change in the REPL error path.

**U7. Unit-level history plumbing (M)** — `DisableAutoSaveHistory: true`; new `historyStore` (`internal/repl/history.go`, fish-style newline-escaped one-unit-per-line at `~/.grsh_history2`) appended when a buffered unit completes; feed chzyer a display form (first line + `…`) so up-arrow still works. Backend for Phase B recall/autosuggestions and session-save.

**U8. Session save (S)** — *approved wild idea.* `session save [N] file.grsh` builtin (or `grsh`-side command) writing the last N history units as a runnable script — interactive work → script in one command. Builds directly on U7's store.

**U9. Prompt customization + git branch + NO_COLOR (M)** — new `internal/repl/prompt.go`: `$GRSH_PROMPT` template (`%d` cwd ~-abbrev, `%s` status, `%g` git branch by reading `.git/HEAD` walking up — no exec, cached by dir+mtime, `%t` time, `%j` jobs, `{red}…{reset}` tags) stripped when `NO_COLOR`/`TERM=dumb`/non-tty. Default template reproduces today's prompt. Verify chzyer handles ANSI in the *prompt* acceptably; if misaligned, ship colorless until the editor swap.

**U10. Value inspector (M)** — *approved wild idea.* `?x` at the prompt (REPL intercepts a line matching `?ident`) pretty-prints the variable's type and value from the live interpreter env — slices/maps as aligned tables, structs with fields. New `runner.Session.Inspect(name) (string, bool)` reaching `internal/interp/env.go`. Unique to grsh: no other shell has typed Go values.

### Round 1 verification
- `go test -race ./...` green; full golden suite green (`internal/runner/golden_test.go`)
- New regression tests per bug (A1 stress, A3 wedge-recovery sequence, A4 bitops golden, A5 position golden)
- REPL features asserted via the fakeReader prompt-recording seam (no pty needed for U5/U6)
- Manual smoke: run `./grsh` interactively — multi-line func with breadcrumb prompt, `fmt.Pr<TAB>`, `?x` inspector, `~/.grshrc` aliases load, `grsh init` against the real `~/.zshrc`

---

## LATER ROUNDS (agreed roadmap, in order)

**Round 2 — Editor swap (Phase B):** half-day spike of `reeflective/readline` on macOS (job-control ^Z/fg, pty input pacing, wide runes) → swap behind the `lineReader` interface (`internal/repl/editor_reef.go`, map its interrupt/EOF onto existing sentinels, History backed by U7's store, `GRSH_EDITOR=legacy` escape hatch). Then: syntax highlighting (`classify.Preview(src) []Chunk`; Go lines via go/scanner, shell first-word green-if-known-command/red-if-not, flags/$vars/strings), fish-style ghost-text autosuggestions from unit history, hint line with `stdlibreg.Signature(pkg,sym)` reflection-derived signature help + alias expansion + mini `--explain`, vi mode via inputrc, real auto-indent (seed continuation lines with spaces, de-indent on `}`). Fallback if spike fails: hand-rolled x/term editor.

**Round 3 — Performance:** benchmarks FIRST (`BenchmarkForLoopInt`, `BenchmarkClassifyLargeGoBlock`, `BenchmarkREPLEvalTrivial`, `BenchmarkInterpolationLoop`), then: lazy env maps + `evalBlockIn` to stop double-wrapping blocks (2 map allocs/iteration today, `internal/interp/env.go`, `interp.go`); `{expr}` AST cache keyed by src (~1024 entries, composes with A5's fset design); incremental `consumeGo` lexing (O(n²) on pasted blocks); reuse the NeedsMore clone in RunSource (composes with A3); `strings.Join` in heredoc accumulation.

**Round 4 — Safety net:** transform unit tests (line-count preservation + `//line` correctness — backbone of all error messages); interp unit tests; fuzz targets (`FuzzShellparseParse`, `FuzzClassifyFile`, differential `FuzzHeredocAgreement` between the two heredoc scanners — the documented sync hazard, `FuzzSessionEvalGoOnly`); pty e2e helper with prompt-paced writes (readline eats pre-buffered input).

**Round 5 — Conveniences:** builtins `echo/true/false/test/[/read/time` (needs builtins-in-pipelines support in `runPipes` — in-process goroutine stages); `xtrace(true)`; **Ctrl+C interrupts pure-Go eval** (`Interp.interrupted atomic.Bool` checked in `evalStmt`/`evalFor` head; REPL SIGINT goroutine sets it; typed `ErrInterrupted`, status 130; flag cleared at RunSource entry; embedded `Interrupt()` wired); registry additions (`net/http` client subset, `bufio.Scanner`, `io.ReadAll`); `$!`; bash-porting hint errors (`local` → ":=", `[[` → "test"); **typed pipeline stages** `ls -l | each { line -> ... }` (approved wild idea — pipeline stage running a Go closure per line, bridging shellexec pipes into interp; the payoff feature of the hybrid design; largest single feature, design doc first).

## Key files (Round 1)
- `internal/shellexec/tty.go`, `exec.go` — A1, B1
- `internal/runner/session.go` — A2, A3, Pending/Inspect surfacing
- `internal/classify/classify.go`, `golines.go`, `repl.go` — A4, U5
- `internal/interp/call.go`, `expr.go`, `env.go` — A2, A4, A5, B3, U10
- `internal/shellparse/parse.go` — B2
- `internal/repl/repl.go`, `completer.go`, + new `prompt.go`, `history.go` — U1-U9
- `internal/stdlibreg/registry.go` — U2
- new `internal/zshimport/` — U4
