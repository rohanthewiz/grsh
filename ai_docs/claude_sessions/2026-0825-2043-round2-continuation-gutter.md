# Session: Round 2 — Continuation Gutter Fix

Session: 9637d0c0-0024-4484-b870-420f009250db
Date: 2026-08-25

## Goal

Close the "Found, not fixed" item carried over from the hint-line session:
the `"  ... "` secondary prompt was **never painted on continuation rows**
under the reeflective editor. Auto-indent spaces showed up; the gutter did
not. Landed; full suite green, all four pty e2e tests pass.

## Root cause

Not our bug — a library default that never lands.

`reeflective/readline@v1.3.0` paints continuation markers in
`internal/display/refresh.go:renderMultilineIndicators`, which opens with:

```go
columns := e.opts.GetBool("multiline-column") ||
    e.opts.GetBool("multiline-column-numbered") ||
    e.opts.GetString("multiline-column-custom") != ""
promptEmpty := e.prompt.LastUsed() == 0

if !columns && !promptEmpty {
    return          // <-- grsh landed here on every refresh
}
```

**The secondary prompt is not independent of the multiline column — it
rides along with it.** `Prompt.Secondary()` can be set all day; with no
column enabled and a non-empty primary prompt the entire indicator pass
returns before `SecondaryPrint()` is ever reached.

Why the option was off, despite the library intending otherwise:

- `internal/keymap/config.go` builtin option table says
  `"multiline-column": true`.
- `loadBuiltinOptions()` only applies a builtin **to keys that are still
  unset**: `if val := m.config.Get(name); val == nil`.
- `inputrc/bind.go`'s default map already defines
  `"multiline-column": false`.

So the key is never nil, the intended `true` never lands, and every
consumer with a non-empty prompt gets bare continuation rows. Setting it
explicitly is the fix.

## Second problem, found on the way

With the column enabled the engine's pass paints a **two-glyph tree**, not
a uniform gutter:

```go
for i := 1; i <= e.line.Lines(); i++ {
    term.WriteString("\n")
    switch {
    case numbered:            // 2, 3, 4...
    case i == e.line.Lines(): e.prompt.SecondaryPrint()   // LAST row only
    default:                  term.WriteString(pipe)      // "│ " on the rest
    }
}
```

Every row but the last gets `ui.DefaultMultilineColumn` (`│ `); only the
final row gets the secondary prompt. The library's own default secondary
is `└ `, so the design is a tree — but grsh (and the legacy editor's
`promptFor`) wants one uniform `... ` on every continuation row, and the
engine exposes no hook for the other rows.

Also noted while reading: `Prompt.MultilineColumnPrint()` — which *does*
honor `multiline-column-custom` — is **dead code in v1.3.0**. Nothing
calls it. `multiline-column-custom` therefore enables the pass but is
ignored by it; only the hardcoded `pipe` is ever drawn.

## What landed

### `internal/repl/editor_reef.go`

**1. Enable the column** (with the whole story above in the comment):

```go
_ = r.rl.Config.Set("multiline-column", true)
```

**2. `secondary()` — repaint the rows the engine won't.** The callback
fires once per refresh, positioned at column 0 of the LAST continuation
row. It climbs, paints, and comes back:

```go
rows := strings.Count(string(*r.rl.Line()), "\n")  // == engine's line.Lines()
if rows < 2 || r.wrapped() { return mark }

fmt.Fprintf(&b, "\x1b[%dA", rows-1)
for range rows - 1 {
    b.WriteString(mark)
    b.WriteString("\r\x1b[1B")
}
b.WriteString(mark)
```

Why reaching up the screen from a prompt callback is safe here:

- **Only the ROW must be preserved.** The engine CRs and re-forwards to
  `lineCol` once the pass finishes, so the column we leave is discarded.
  Up N-1, paint down N-1, net zero.
- **Nothing can accumulate.** `displayLine` emits `ClearLineBefore` at the
  start of every continuation row, so the gutter area is wiped and
  repainted from scratch each keystroke — a mispaint dies on the next one.
- **The whole frame is buffered** (`term.BeginBuffer`/`EndBuffer`), so
  overwriting the engine's `│ ` costs no flicker; the pipe never reaches
  the screen.

**`\r\x1b[1B`, not `\r\n`** — learned from the pty: the tty is in raw mode
but `MakeRaw` here **does not clear OPOST**, so ONLCR is still on and our
LF came back as `\r\r\n`. (The engine's own bare `"\n"` between indicators
silently depends on that.) A CR plus CUD is exact either way, and cannot
scroll: every row stepped down to is one just climbed up from.

**3. `gutter()` — right-aligned into the prompt width.** The engine
indents continuation rows to the prompt's start column, so a mark parked
at column 0 sits stranded under a wide prompt. Right-aligned, it ends
exactly where the code begins:

```
grsh ~/projs/go/grsh> func hi() string {
                 ...    return "x"
                 ...  }
```

(The legacy editor prints the same mark at column 0 only because there the
gutter IS the prompt.) Dim (`\x1b[2m`) when color is on, padding outside
the SGR run so printed width is unchanged. A prompt too narrow to hold
`"... "` truncates it rather than running into the buffer.

**4. `wrapped()` — the honest limitation.** The engine's pass counts
LOGICAL lines while it travels by VISUAL rows (`MoveCursorUp(e.lineRows)`
then `Lines()` indicators), so wrapped input already misplaces its own
indicators. When any buffer row would wrap, the gutter stays on the single
row the engine positioned us on instead of smearing the mark across rows
it cannot place.

**5. `promptCols()`** — printed width of the prompt's last line, skipping
CSI escapes. This is what the gutter is sized against, and it matches the
engine's `startCols` in every real path (DSR probe answers the true
column; probe-off falls back to `prompt.LastUsed()`, the same measure).

**6. `reefReader.color`** — `colorEnabled()` cached at construction. The
gutter runs per refresh and the check stats the terminal; a keystroke
already pays for a classifier pass and a history scan.

### `internal/repl/e2e_pty_test.go` — harness honesty

`startShell` now sets `$INPUTRC` to a temp file containing
`set cursor-position-probe off`.

The harness answers every `ESC[6n` with a fixed **"row 1, column 1"**, so
the engine believed input started at column 0 no matter how wide the
prompt actually was — `startCols` came out 2 (the minimum
`ensureIndicatorSpace` reserves) against a real prompt of ~44. Any
column-sensitive assertion was being measured against a fiction. With the
probe off the engine derives the start column from the printed prompt
width, which is what a real terminal reports back anyway.

Bonus: the library resolves `~/.inputrc` from the **user database**, not
`$HOME`, so the existing temp-HOME isolation never covered it — the tests
were reading the developer's personal inputrc. `$INPUTRC` closes that too.

New assertions in `TestReefHighlightAndIndentEndToEnd`:

```go
p.send("func hi() string {\r")
p.waitFor("\x1b[2m... \x1b[0m")   // gutter BEFORE the breadcrumb
p.waitFor("… func hi")
```

**Ordering matters and is not stylistic**: both live in the same frame,
gutter first (`renderInputArea` before `renderHelpers`). `waitFor`'s
offset only moves forward, so waiting for the breadcrumb consumes past the
gutter — and no further frame arrives until the next keystroke, so the
reversed order hangs for the full 10s timeout.

Three-row case asserts the repaint shape itself:
`"\x1b[2m... \x1b[0m\r\x1b[1B"`.

### `internal/repl/editor_reef_test.go`

- `TestReefConfigGuards` — `multiline-column` must be on, with a note that
  the whole indicator pass (secondary prompt included) dies without it.
- `TestPromptCols` — plain / colored / multi-line / unicode / empty.
- `TestGutter` — right-alignment, narrow-prompt truncation, dim form's
  printed width unchanged.
- `TestSecondaryGutter` — one row returns the bare mark, three rows return
  `\x1b[2A` + mark + step + mark + step + mark, wrapped buffer falls back.
  Deterministic off a terminal: `term.GetSize` fails under `go test`, so
  `wrapped()` takes the 80-column fallback.

## Verification

`go build ./... && go vet ./... && go test ./...` — all green, including
all four pty e2e tests (`TestReefEditorEndToEnd`,
`TestReefHighlightAndIndentEndToEnd`, `TestReefGhostTextEndToEnd`,
`TestReefHintLineEndToEnd`).

Method worth reusing: a throwaway pty probe test that types a multiline
unit and `t.Logf`s the raw byte stream with `\x1b` → `<ESC>`. It showed
the bare `\x1b[2C\x1b[1K  ` continuation rows before the fix, the ONLCR
`\r\r\n` doubling mid-fix, and the final `\x1b[1A` + mark + step + mark
sequence after. Deleted once the real assertions landed.

## Gotchas for next time

- **`Prompt.Secondary` is gated by `multiline-column`.** Setting the
  callback is not enough; without a column enabled the engine returns
  before calling it. (Corrects the note in the editor-swap session doc:
  the secondary renders on ONE row — the last — not on all of them.)
- **Raw mode here keeps OPOST/ONLCR.** `internal/term/raw_unix.go` clears
  IFLAG/LFLAG/CFLAG bits but never touches `Oflag`, unlike `cfmakeraw(3)`.
  Anything writing `\n` to the tty gets CR-LF; anything writing `\r\n`
  gets CR-CR-LF. Use CUD for row motion.
- **The pty harness lies about the cursor position** unless the probe is
  off. Any new column/alignment assertion must keep `$INPUTRC` in place.
- **`multiline-column-custom` does nothing** in v1.3.0 — the only code
  that honors it (`MultilineColumnPrint`) is never called.
- The gutter runs on every refresh: `promptCols` + a `term.GetSize` ioctl
  per keystroke, on top of the per-keystroke costs already listed in the
  hint-line session doc.

## Still open in Round 2

The manual smoke pass from the previous session, minus the gutter item:
highlighting feel, indent/dedent feel, ghost-text feel, hint-line
legibility (`▏` separator, signature flicker on fast typing), ^Z/fg on a
live job, paste, recall. `GRSH_EDITOR=legacy` if anything's off.
