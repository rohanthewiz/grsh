# Session: Round 2 — reeflective/readline Spike + Editor Swap

Session: https://claude.ai/code/session_01PYbPTTVgVMLhQDC1L6HofU
Date: 2026-08-24

## Goal

Round 2 of the improvement plan (`ai_docs/plans/grsh-improvement-plan.md`):
spike reeflective/readline on macOS, and if it holds up, swap it in behind
the `lineReader` seam. **Verdict: GO** — the swap landed as the default
editor, chzyer kept behind `GRSH_EDITOR=legacy` (or `chzyer`).

## What landed

### The adapter (`internal/repl/editor_reef.go`)

- `reefReader` implements the 2-method `lineReader` seam
  (Readline/SetPrompt). reeflective v1.3.0; only extra transitive dep is
  rivo/uniseg.
- **Native multiline** (the marquee win): `rl.AcceptMultiline` consults
  `sess.Pending(line).NeedsMore`, so an unfinished func/if/heredoc stays
  in ONE editable buffer (arrow keys cross lines) and Readline returns
  the whole unit. The loop's buf-accumulation machinery is untouched but
  dormant with reef — Pending on a returned unit is complete by
  construction. Legacy chzyer still uses it.
- **Breadcrumbs relocated to the hint lane**: `rl.Hint.SetProvider`
  recomputes `… func hi ▸ for` (dimmed via `colorEnabled()`) from the
  live buffer every refresh — the display engine calls UpdateProvided
  per repaint. Secondary prompt is a plain `  ... ` gutter (per-line
  breadcrumbs aren't possible there: one string serves all lines, and
  the code is visible in-buffer anyway).
- **Prompt**: reeflective is callback-based — `Prompt.Primary` closes
  over `r.prompt`, which SetPrompt writes; loop unchanged.
- **History**: `unitSource` adapts the Round 1 unit store
  (`~/.grsh_units`) to reeflective's `history.Source`. Up-arrow restores
  a multi-line func as one editable buffer. `Write` is a deliberate
  no-op — the loop persists via `hist.Append` after `replCommand`
  dispatch, so `?x`/`session save` lines stay out of saved scripts;
  index 0 = oldest matches the store's ordering.
- **Completions**: `completer.matches(before) (word, []string)` extracted
  as the shared candidate engine; chzyer's `Do` re-shapes to suffixes
  (+trailing space), reef's `completeReef` returns whole-word values
  `.Prefix(word).NoSpace('/')` (engine replaces the PREFIX span;
  directories chain).
- `inputrc.WithApp("grsh")` → users can scope `~/.inputrc` with
  `$if grsh`; vi mode comes free via `set editing-mode vi`.

### Spike landmines (each cost a debug cycle; all pinned by tests)

1. **`\C-c` defaults to self-insert** — reeflective's ErrInterrupt only
   comes from its ^G/^] "terminator" abort path. Registered a custom
   `grsh-interrupt` command (`Display.AcceptLine()` +
   `History.Accept(false, false, ErrInterrupt)` — the exact tail of the
   library's abort) and bound `\x03` in emacs/vi-insert/vi-command.
2. **`\C-z` also self-inserts** (literal SUB byte into eval source).
   First tried binding "abort" — WRONG: abort probes
   `InputIsTerminator()`, which dispatches against pending keys and EATS
   the next typed-ahead keystroke. Bound a registered `grsh-noop`
   instead. No SIGTSTP can reach the parent at the prompt (raw mode
   clears ISIG — the chzyer footgun is gone by construction); ^Z during
   a foreground command still suspends the job (cooked mode, ISIG on).
3. **UTF-8 corruption, two separate byte-era defaults**:
   `convert-meta` (default true) rewrites high-bit INPUT bytes as
   ESC+char; `output-meta` (default false) routes self-insert of any
   rune ≥ U+0080 through `strutil.Quote`, so é landed in the LINE BUFFER
   as a literal `ESC i` chord (U+00E9 | stripped-high-bit = 'i'), while
   3-byte CJK runes survived — maddening to bisect. GNU readline flips
   both automatically in UTF-8 locales; grsh now sets
   `convert-meta=false`, `output-meta=true` at construction.
4. `enable-bracketed-paste` defaults OFF — enabled: multiline pastes
   arrive as one buffer edit (no per-line eval, no tab-triggered
   completion mid-paste).
5. **The editor blocks on `ESC[6n`** (DSR cursor probe) at every
   refresh — any pty harness must play terminal and answer `ESC[r;cR`,
   handling requests split across read boundaries.

### Loop/sentinel changes (`repl.go`)

- Editor-neutral `errInterrupt` sentinel; `chzyerReader` wraps the
  legacy instance and translates chzyer's ErrInterrupt; reef translates
  its own. Loop checks only the neutral sentinel.
- `Run` builds `hist` + `comp` up front, picks the editor via
  `legacyEditor()` (`GRSH_EDITOR=legacy|chzyer`). chzyer's per-line
  `~/.grsh_history` file is legacy-only now.
- reef ^D semantics improve on legacy: ^D with content = delete-char
  (bash), so there's no ^D-mid-continuation case at all.

### Bonus fix (found by the e2e): bare `exit` status

`exit` with no args exited 0 regardless of `$?`. Bash defines `exit` as
`exit $?` — now `code := st.LastStatus` in the exit builtin
(`internal/shellexec/builtins.go`). `false` then `exit` → shell exits 1.

### Tests

- `internal/repl/e2e_pty_test.go` (darwin): builds the real grsh binary,
  attaches it to a BSD-/dev/ptmx pty pair as controlling terminal
  (Setsid+Setctty, TIOCSWINSZ 40x120 — a 0x0 winsize breaks wrap math),
  and drives the reef editor end to end: Go eval, classifier-driven
  multiline accept + breadcrumb hint, shell leg, ^C recovery (dropped
  text must not run), ^Z inertness, é/—/CJK runes, ^D exit, and a
  second test for nonzero exit status propagation.
  - **Marker discipline**: every waitFor targets output only EVAL can
    produce, built by concatenation (`"yo-" + "ho"` → wait "yo-ho") —
    keystroke echo and full-buffer repaints replay typed source many
    times, so literal markers false-match.
  - Harness details: DSR responder with 3-byte tail carry for
    split-across-reads requests; `writeMu` serializes test keystrokes
    with responder replies (an unserialized reply can land BETWEEN the
    two bytes of one UTF-8 rune — this masqueraded as a library bug).
- `editor_reef_test.go`: `TestReefConfigGuards` (convert-meta off,
  output-meta on, bracketed paste on, ^C/^Z binds per keymap),
  `TestUnitSource` (order, bounds, Write-is-noop), and
  `TestReefAcceptMultiline`.
- `go vet` clean, `gofmt` clean, **full suite green under `-race`**.

## Key design decisions

- Skipped a throwaway spike binary: the real adapter behind the seam +
  pty e2e + legacy escape hatch WAS the spike vehicle.
- Loop stays editor-agnostic; no reef types leak past editor_reef.go.
- REPL commands (`?x`, `session save`) intentionally not recallable in
  reef mode (they bypass the unit store so saved scripts stay runnable).
- Classifier semantics note (not a bug): bare `x + 1` is SHELL by rule 6
  — Go usage needs assign/call/selector shape; `fmt.Println(x + 1)` or
  `y := x + 1` are the REPL idioms. The e2e initially assumed wrong.

## Still open in Round 2 (next session)

In order, all on hooks that are now one line away:
1. **Syntax highlighting** — `rl.SyntaxHighlighter func([]rune) string`;
   plan: `classify.Preview(src) []Chunk` per-line lexers (go/scanner for
   Go lines; shell first-word green-if-known/red-if-not, flags, $vars,
   strings).
2. **Fish ghost text** — `rl.SetInlineSuggestion` /
   `GetInlineSuggestion` from unit-history prefix match.
3. **Hint-line signatures** — extend the existing Hint provider:
   `stdlibreg.Signature(pkg, sym)` reflection help + alias expansion
   (breadcrumb already lives there; compose, don't clobber).
4. Real auto-indent (seed spaces on newline — needs a custom Enter
   binding or upstream hook; AcceptMultiline only inserts bare \n).
5. User manual smoke still pending: ^Z/fg on a live job, paste feel,
   recall feel. `GRSH_EDITOR=legacy` if anything's off.

## Gotchas for future sessions

- reeflective quirks are all in the "landmines" list above — reread it
  before touching editor_reef.go. Extra: `user.Current()` (not $HOME)
  resolves its inputrc, so test isolation via HOME doesn't isolate
  inputrc; this machine has none, but CI images might.
- `Prompt.Secondary` renders ONE string for all continuation lines —
  per-line breadcrumbs can't live there.
- Hint provider runs `sess.Pending` per keystroke (clone-based classify)
  — fine interactively; Round 3 perf if it ever shows up.
- pty e2e builds grsh via `go build ../../cmd/grsh` per test run (~1s,
  skipped under `-short`).
- The darwin pty ioctls (`unix.Syscall(SYS_IOCTL)`) are deprecated in
  x/sys — same known status as shellexec/tty_test.go.
