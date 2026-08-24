# Session: Round 1 — Critical Bug Fixes + Interactive UX Phase A

Session: https://claude.ai/code/session_01K3196tdPNVAVuoAajgNWr7
Date: 2026-08-24

## Goal

Make grsh a daily-driver terminal. Full improvement audit (3 exploration
agents + 2 design agents), then Round 1 execution: fix the 5 critical
bugs, land the correctness papercuts, and ship the on-chzyer interactive
UX wins. The complete multi-round roadmap lives in
`ai_docs/plans/grsh-improvement-plan.md`.

## Round 1 — what landed

### Critical bug fixes (each with regression tests)

1. **Capture/job-control race** — `interactiveTTY` (shellexec/tty.go)
   gated only on stdin being a tty, so REPL `x := $(ls)` (buffer stdout)
   entered `runForegroundJobControl`, which reaps via `Wait4` and never
   calls `c.Wait()` → race with os/exec copier goroutine (truncated
   captures) + parent pipe fd leak. Fix: all three stdio legs must be
   `*os.File` (`isOSFile`), plus a loud internal-error guardrail inside
   `runForegroundJobControl`. Tests: darwin pty-pair helper
   (`tty_test.go`, /dev/ptmx + TIOCPTYGRANT/UNLK/GNAME via raw ioctl),
   byte-exact 20× capture stress under `-race`.
2. **Interpreter panics killed the shell** — recover backstop moved into
   `runner.Session.RunSource` (covers REPL/-c/script/source/embedding);
   root-cause fixes: nil-map write in `setIndexed` + `delete` no-op on
   nil map, `make` validates negative len / len>cap, `copy` pre-checks
   element assignability. (interp/expr.go, interp/call.go)
3. **Wedged classifier depth** — `RunSource` now classifies on
   `cls.Clone()` and commits `s.cls = cls` only after go/parser
   succeeds; runtime errors still commit (side effects already ran).
   No more phantom-`}` prompts after a failed parse.
4. **`x <<= 2` hang** — `startsGoOp` was missing `&= |= ^= <<= >>= &^=`
   → classified shell → `<<` read as heredoc → REPL waited forever.
   Added the ops there AND to `interp.assignOp`; also unary `^x`,
   binary `&^`, and negative-shift errors. Golden-style test
   `TestCompoundBitOps` (11, -1).
5. **`{expr}` wrong error positions + reparse-per-iteration** — new
   `Interp.parseFragment`: fragments parse into the interpreter's OWN
   fileset via `ParseExprFrom` with `AddLineInfo` remapping to the
   enclosing shell statement's script line (//line-style), cached per
   (src, line), cache reset per Run (fset changes). `wordEval` now
   carries the `__shell/__capture` call node.

### Correctness papercuts

- **Prefix env** `FOO=bar cmd` implemented in all three exec paths
  (runSimple, runPipes, launchJob → `preparedCmd.env`);
  `splitAssignPrefix`/`isAssignWord` in exec.go. Bare `FOO=bar` →
  positioned error with hint (variables are Go / export). Decision:
  no shell-local variable namespace, ever.
- **`${VAR:-default}` rejected at parse time** (`validParamName` /
  `paramBase` in shellparse/parse.go) with an `iff(env(...))` hint —
  previously silently expanded via `os.Getenv("VAR:-default")` = "".
- **`errexit(true)` interactive** no longer exits the shell
  (`!in.sh.Interactive` guard in runShellStmt), matching bash.
- **Scripts exit with last command's status** (bash semantics) —
  `grsh -c false` was exiting 0! main.go now `os.Exit(sess.LastStatus())`
  on success. CLI regression test added.
- Cleanups: "grsh v1" error strings → "yet"; dead `transform.HeaderLines`
  removed; `eofReader` unified as exported `shellexec.EOFStdin`;
  `go mod tidy` fixed `// indirect` mislabels; README/LANGUAGE.md
  stale-v1 claims rewritten.

### Interactive UX Phase A (all on chzyer/readline)

- **Construct breadcrumbs + auto-indent** (flagship): new
  `classify.Pending(src) PendingInfo{NeedsMore, Depth, Constructs}` —
  clone-based, non-mutating, subsumes NeedsMore (one classify pass per
  REPL line instead of two). `Classifier.blocks` label stack maintained
  in `trackGoLine` (first LBRACE of a line gets `constructLabel`:
  "func greet"/"for"/"else"/closure name; later braces "{"). Prompt:
  `  ... func greet ▸ for ▸ ` + 2 spaces per depth (indent lives in the
  prompt string — history and eval source stay clean). Tests:
  `pending_test.go` table + `TestLoopBreadcrumbPrompt` via the
  prompt-recording fakeReader (loop signature now takes `hist`).
- **`~/.grshrc`** — `loadRC` in repl.Run (before SetInteractive), `$GRSH_RC`
  override, `-norc` flag, `exit` honored, errors printed and continue.
- **`grsh init`** — new `internal/zshimport`: conservative .zshrc →
  .grshrc translator. Active only when certain (safe aliases, exports,
  PATH/path+= edits, plain commands); zsh-isms (setopt/bindkey/…),
  functions, if/case blocks, source/eval lines preserved as comments
  with `# [zsh ...]` notes and porting TODOs. Reads
  .zshenv/.zprofile/.zshrc, never overwrites (writes ~/.grshrc.new).
  Subcommand: `flag.Arg(0) == "init"` in main.go.
- **`?name` inspector** — `Interp.Inspect` (interp/inspect.go): aligned
  tables for maps/slices (cap 20 items), struct fields, closure
  signatures from AST; surfaced as `Session.Inspect`, intercepted in
  the loop by `replCommand` (repl/commands.go).
- **`session save [N] file.grsh`** — writes session units (or last N) as
  a shebang script; refuses to overwrite. Round-trip verified by test
  (saved script replays through a fresh session).
- **Unit history store** — `~/.grsh_units`, fish-style escaped
  one-unit-per-line (repl/history.go). chzyer's per-line history kept
  as-is for recall (no regression); the unit store backs session save
  and the Phase B editor swap.
- **Error carets** — `userMsg`/`caretBlock`: eval errors with line:col
  echo the source line + `^` (tabs flattened).
- **Completion** — `stdlibreg.Members(pkg)` + `selectorShaped` branch:
  `fmt.Pr<TAB>` → Print/Printf/Println…; `shellexec.BuiltinNames()`
  replaces the drifted hardcoded list.
- **Prompt templates** — `$GRSH_PROMPT` (repl/prompt.go): `%d %s %g %t
  %j %%`, `{red}…{reset}` tags, `colorEnabled()` (NO_COLOR/TERM=dumb/
  non-tty), `gitBranch` reads .git/HEAD walking up (no fork),
  `Session.JobCount()` added.

## Key design decisions

- Editor strategy (user-approved): two-phase — Phase A quick wins on
  chzyer; Phase B swap to **reeflective/readline** behind the 2-method
  `lineReader` seam (chzyer's RuneBuffer cursor math makes ANSI in the
  buffer impossible → highlighting/ghost text need the swap). Half-day
  spike first; fallback hand-rolled x/term editor.
- `{expr}` fragments share the interpreter fileset (not a swapped
  private one) so nested closure calls keep correct positions; cache
  key includes line so distinct call sites map to their own lines.
- Classifier commit point = after go/parser success, before interp.Run
  (runtime errors still record declared names — REPL units are
  depth-balanced by construction).
- zshimport emits ACTIVE lines only when semantics are certain;
  everything else is preserved as comments — nothing silently changes
  meaning.

## Verification

- `go vet ./...` clean; `gofmt` clean; **full suite green under
  `-race`** (all packages, -count=1).
- New tests: robustness_test.go (panic vectors, wedge recovery, prefix
  env, ${} rejection, errexit-interactive, {expr} position),
  tty_test.go (darwin pty gate + capture stress), pending_test.go,
  repl tests (breadcrumbs, caret, inspector, session save, history
  round-trip, rc load, prompt render, git branch, pkg.Member
  completion), zshimport_test.go, CLI last-status test.
- Smoke-tested binary: `grsh init` against a fake HOME, bit ops,
  prefix env, rejection messages, exit statuses.

## Next rounds (see ai_docs/plans/grsh-improvement-plan.md)

2. **Editor swap**: reeflective/readline spike → syntax highlighting
   (classify.Preview per-line lexers, red/green command validity),
   fish ghost-text from unit history, hint line with
   stdlibreg.Signature reflection, vi mode, real auto-indent.
3. **Perf**: benchmarks FIRST, then lazy env maps + evalBlockIn (2 map
   allocs/loop-iteration today), incremental consumeGo lexing, reuse
   NeedsMore clone in RunSource.
4. **Safety net**: transform + interp unit tests, fuzz (shellparse,
   differential heredoc scanners), pty e2e helper (pace writes —
   readline eats pre-buffered input).
5. **Conveniences**: echo/true/false/test/read/time builtins (needs
   builtins-in-pipelines), xtrace, **Ctrl+C interrupts pure-Go eval**
   (atomic flag in evalStmt), net/http + bufio registry, `$!`,
   bash-porting hints, `each {}` typed pipeline stages.

## Gotchas for future sessions

- serr `GetAttribute` JOINS duplicate keys ("a - b") — never wrap the
  same key twice on one error path (why EvalGoExpr doesn't add "in";
  expandWord already does).
- zsh eats `=word` (equals expansion): don't use `echo ====` as a
  separator in Bash tool calls on this machine.
- The darwin pty test helper uses deprecated `unix.Syscall(SYS_IOCTL)`
  — fine for tests; revisit if x/sys removes it.
- `exprCache` must be reset whenever `Interp.fset` is replaced (Run
  does this); cross-Eval closures already resolve against the newest
  fset (pre-existing, positions slightly off — acceptable).
