# Session: Round 2 — Fish-Style Ghost Text

Session: https://claude.ai/code/session_01FDep78PQkXsdsFeWkaGXwY
Date: 2026-08-24

## Goal

Item 1 of the remaining Round 2 backlog: fish-style inline
autosuggestion ("ghost text") from the unit-history store, via
`rl.SetInlineSuggestion` / `GetInlineSuggestion`. Landed; full suite
green under `-race` including the pty e2e.

## What landed

### `internal/repl/suggest.go` (new)

`suggester` — prefix-matches `historyStore.Units()` **newest first**,
returns the whole suggested line (buffer prefix included, which is the
shape the display engine expects), `""` when nothing matches. Memoized
on the buffer string: the display engine refreshes on cursor-only
movement, so without it every arrow key would rescan history. Runs on
the editor's single read loop → no locking (same contract as the
highlighter's memo).

Refusals, all deliberate:

- **blank buffer** (`TrimSpace == ""`) — matches nearly everything, so
  the ghost would flicker on the first keystroke of every prompt.
- **multi-line buffer** — ghost text can't live on a wrapped/gutter'd
  buffer.
- **multi-line units** — grsh history is a UNIT store, so an entry can
  be a whole `func` block. `coordinatesLine` builds a `core.Line` from
  the suggestion ALONE, so a newline in it wrecks the row math. This
  filtering is the main reason not to just flip the library's
  `history-autosuggest` on (default false): that suggests from the
  merged sources with zero filtering, and also changes the semantics of
  forward-char/forward-word and the history match cursor.

### Wiring (`editor_reef.go`)

- **`reefReader` gained `sess`, `ghost *suggester`, `ghostHold bool`.**
  The hint closure became a method, `reefReader.hintProvider` — needed
  a callable seam for tests (the `provided` hint lane has no exported
  getter) and it reads better.
- **The hint provider is the per-refresh hook.** Ordering, verified in
  `internal/display/refresh.go` + `engine.go`:

  ```
  Refresh()
    prompt.LastPrint()               ← Primary callback (before e.line is resolved)
    computeCoordinates(true)
      e.line, e.cursor = completer.Line()
      hint.UpdateProvided(line, pos) ← OUR PROVIDER: ghost set here
      coordinatesLine(true)          ← consumes e.inline for row/col math
    renderInputArea()
      displayLine()                  ← SyntaxHighlighter, then paints e.inline
    renderHelpers()                  ← hint lane painted
  ```

  So the hint provider is the only app callback that runs after the
  effective buffer is resolved and before the suggestion is measured.
  Setting the ghost from `SyntaxHighlighter` would be one frame stale
  for the coordinate pass and mis-place the cursor when the ghost wraps.
- **`updateGhost(line, pos)`** sets the suggestion only when: not
  `ghostHold`, `Keymap.Local() == ""` (isearch minibuffer / virtually
  inserted completion candidate must not be extended), and
  `pos == len(line)`. Otherwise `SetInlineSuggestion("")` clears.
- **The accept-time hold — the subtle bug this design exists to avoid.**
  `Display.AcceptLine()` calls `computeCoordinates(false)`, and
  `coordinatesLine` checks `inlineSuggestionApplies` regardless of the
  `suggested` flag. With a ghost still set, the engine measures the
  SUGGESTED line, moves the cursor to the end of the ghost, then
  `ClearScreenBelow` + newline — leaving the ghost printed on the
  accepted line as if it had been typed. Clearing before delegating
  doesn't work: our provider re-runs inside that very
  `computeCoordinates`. Hence a hold flag, set in **`AcceptMultiline`**
  — the library's last call before `Display.AcceptLine` on *every*
  acceptance path through `acceptLineWith` (accept-line, accept-and-hold,
  accept-line-and-down-history, autosuggest-execute) — plus
  `grsh-interrupt`, which calls `AcceptLine()` directly. Lifted at the
  top of `reefReader.Readline`.
- **Whole-suggestion accept needed no wiring**: `forwardChar` /
  `viForwardChar` already fall back to `acceptInlineSuggestion()` when
  the cursor is at end-of-line, and emacs binds `\C-f` + `\M-[C`/`\M-OC`
  (→) to forward-char. Free.
- **Word accept overrides the `forward-word` COMMAND**, not keys — so
  every sequence that already resolves to it (`\M-f`, `\M-[1;3C` alt-→,
  `\M-[1;5C` ctrl-→, in whichever keymap the user has them) inherits the
  behavior. `inline-suggest-accept-word` is a no-op unless a suggestion
  applies, so "did the buffer grow?" is a sufficient, version-proof test
  for whether to fall through to the plain motion.
- **Gated on `colorEnabled()`**, same as the highlighter: the library
  paints the ghost with a hardcoded `Dim + 38;05;242`, so with color
  suppressed it would be indistinguishable from typed input.

### Tests

- `suggest_test.go`: `TestSuggesterMatch` (prefix, newest-wins, older
  still reachable, whole-line no-op, blank/empty, multi-line buffer,
  multi-line unit), `TestSuggesterMemo` (mutates the store behind the
  memo and expects the stale answer).
- `editor_reef_test.go`: `withGhost` helper wires a suggester by hand —
  `newReefReader` only builds one under `colorEnabled()`, false under
  `go test`. `TestReefGhostText` (end-of-line, cleared mid-buffer,
  cleared when the match is gone, held during accept + lifted after a
  continuation, off without color, composes with the breadcrumb and
  still refuses the multi-line unit) and
  `TestReefForwardWordAcceptsGhost` (word, then rest, then fall-through
  to the motion).
- `e2e_pty_test.go`: `TestReefGhostTextEndToEnd` — runs
  ``printf 'gho%s\n' st-seed``, waits for `ghost-seed`, retypes the
  prefix `printf 'gho`, waits for the literal
  `\x1b[38;05;242m` + remainder in the repaint stream, then `^F` + Enter
  and asserts the WHOLE command ran. The `%s` split keeps `ghost-seed`
  out of keystroke echo and repaints, so only evaluation can produce it.

### Docs

README "Interactive conveniences" bullet; new LANGUAGE.md "Ghost text"
bullet after Syntax highlighting (keys, the never-multi-line rule, the
color gate).

## Gotchas for future sessions

- **`Display.AcceptLine` measures the inline suggestion.** Anything that
  puts state into `e.inline` must take it down before acceptance; the
  reliable hook is `AcceptMultiline`, not the accept-line override
  (which the library's other accept commands bypass).
- The library's `history-autosuggest` inputrc var (default false) stays
  OFF. Turning it on would double-render (displayLine prefers it) and
  drag the unfiltered unit store — multi-line entries included — into
  the ghost lane.
- `reeflective`'s color constant is `Fg = "38;05;"` — note the leading
  zero. Ghost SGR is `\x1b[2m\x1b[38;05;242m`.
- `Hint` has no exported getter for the `provided` lane; test the
  provider by calling `reefReader.hintProvider` directly.
- `Keymap.Commands()` returns the live registry — capture stock funcs
  BEFORE registering an override (now done three times in this file:
  accept-line, forward-word, and the electric brace's neighbors).
- Per keystroke the editor now runs: 2 `Pending` (accept-line override +
  hint), 1 `Preview` (highlighter), 1 history scan (suggester, memoized).
  Still the first place to look if typing ever feels heavy.

## Still open in Round 2 (next session)

1. **Hint-line signatures** — extend `hintProvider`:
   `stdlibreg.Signature(pkg, sym)` reflection help + alias expansion.
   Compose with the breadcrumb and the ghost update, don't clobber.
2. User manual smoke: highlighting feel, indent/dedent feel, ghost-text
   feel (does it flicker? is → discoverable?), ^Z/fg on a live job,
   paste, recall. `GRSH_EDITOR=legacy` if anything's off.
