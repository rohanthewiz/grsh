# Session: Round 2 — Manual Smoke Pass + Go-Leg Exit Status

Session: dd6be4c2-5e12-4429-99d0-416707d25798
Date: 2026-08-25

## Goal

Close the last open item from Round 2: the manual smoke pass deferred
across the previous three sessions — highlighting feel, indent/dedent
feel, ghost-text feel, hint-line legibility, ^Z/fg on a live job, paste,
recall.

Outcome: six of seven areas clean. One real bug found and fixed (the
prompt's exit-status badge went stale after a Go eval). One reported bug
turned out to be a harness artifact and was retracted.

## The harness — why a VT emulator, not raw bytes

The existing `internal/repl/e2e_pty_test.go` asserts against the raw
escape stream. That is the right tool for "was this sequence emitted",
and the wrong tool for every question this pass actually asks —
"is the gutter aligned", "is the hint legible", "did that key do
anything visible". Those are questions about where glyphs land *after*
the escapes are interpreted.

So the driver (`scratchpad/smoke/`, not committed) pipes the pty master
into `github.com/hinshun/vt10x` and snapshots the rendered grid: every
non-blank row as text, plus an attribute map naming each colored/styled
run by column span, plus the cursor position.

Two things fall out of using a real emulator:

- **The cursor-position probe can stay ON.** vt10x is constructed with
  `WithWriter(master)`, so it answers `ESC[6n` from its own true cursor
  state. The test harness cannot do this — it replies with a fixed
  "row 1, column 1" and therefore has to disable the probe to stay
  honest (see the previous session doc). This driver needs no such
  crutch, and so exercises the same code path a real terminal does.
- **Colors survive into the dump.** vt10x implements no SGR 2 (dim),
  which grsh uses for the gutter, ghost text and hint line, so the
  driver rewrites `ESC[2m` → `ESC[2;3m` on the way in: the original
  code still flows through, and italic — which grsh never emits (checked
  against the SGR inventory in `internal/repl`) — becomes an
  unambiguous stand-in for dim.

Run in a SHORT cwd. The first run used the scratchpad path and the
prompt wrapped across 28 of the 30 rows, burying the buffer. `cmd.Dir =
home` with a `/tmp/grsh-smoke-*` home fixes it.

### Two harness bugs that produced false findings

Both are worth remembering because both *looked* exactly like product
bugs and one of them was reported as such before being caught.

**1. Settle-on-quiet races.** The first `settle(quiet, max)` waited for
the shell to be quiet for `quiet`. But `snap()` and the scenario's own
pauses can already have exceeded that window before the next key is even
sent — so the wait returns instantly on the PREVIOUS frame and the
snapshot reports stale screen state as "the key did nothing". This made
`^C` look like it failed to exit incremental search, and made two
history-recall steps look wrong. Fix: `sendSettle` anchors on a byte
count taken before the write, and waits for the stream to BOTH grow and
go quiet — returning a bool so "no output at all" is reportable as a
finding rather than silently indistinguishable from a timing artifact.

**2. A dump that ignored background color.** The `^R` match list
rendered as `DIM+black`, which on a dark terminal would be invisible —
a real legibility bug, except the driver was only reporting foreground.
With background added it reads `black on c255`: an ordinary selection
highlight. Nothing wrong at all.

Lesson for the next harness: any claim of the form "X is invisible" or
"key K did nothing" needs the harness to be able to distinguish that
from its own blind spots first.

## What passed

- **Highlighting** — matches `highlight.go`'s documented palette exactly:
  Go keywords magenta, strings yellow, numbers cyan, comments dim;
  shell command green when it resolves / red when it does not, flags
  cyan, `$vars` magenta. Pipes, redirects, globs and `$VAR`-inside-string
  are uncolored, which is what the file claims — not gaps.
- **Indent / dedent / gutter** — alignment is exact. Prompt 41 cols;
  `... ` occupies 37–40 so it ends precisely where the prompt ends, and
  code starts at 41. Nesting produced 2 then 4 spaces; electric `}`
  dedented 4 → 2 → 0. In the raw stream the engine's `│ ` tree is
  overpainted by the `secondary()` callback inside one buffered frame
  and never reaches the screen — the mechanism the last session built,
  now confirmed on a real emulator with the probe live.
- **Ghost text** — `^F` and `→` accept the whole suggestion; Alt-f
  accepts exactly one word (`echo al` → `echo alpha`, remainder still
  dim). Most-recent match wins on an ambiguous prefix.
- **Hint line** — signature, alias and breadcrumb all render and compose
  with the `▏` separator: `fmt.Println(...any) (int, error)  ▏ … func
  outer`. Final frame correct after fast typing.
- **^Z / fg** — suspend prints `[1]  Stopped`, prompt returns with
  `[146]`, `jobs` lists it, the editor keeps evaluating, `fg` resumes,
  `^C` kills it (`[130]`), editor healthy after.
- **Paste** — see the retraction below; correct on stock HEAD.
- **Recall** — up/down walk in order and land back on an empty line; a
  multiline unit comes back as ONE editable buffer with its gutter;
  `^R` narrows and exits cleanly on `^C`.

### Method note: isolate one key per shell

The ghost-acceptance keys were first tested chained in one session
(Alt-f, then `^F`, then `^E`) and the result was read exactly backwards
— `^E` appeared to be the key that accepted. Chaining cannot attribute
an effect: if a key mutates the buffer but does not repaint, the NEXT
key's repaint shows the change and gets the credit. Re-run one key per
fresh shell, snapshotting the SCREEN after the key and then pressing
Enter to read the BUFFER truth (which line actually ran), the real
behavior is unambiguous. Screen and buffer must be reported separately;
a mismatch between them is itself the interesting outcome.

## Retracted finding: bracketed paste

Reported as a bug, then withdrawn. Recording it because the failure mode
is reusable.

The paste scenario sent `func f() {\rreturn 5\r}\r` as one raw write.
Result: pasted indentation stacked with auto-indent (`    return 5`
arrived as `      return 5`), and a block ending in a newline executed on
arrival. Root cause looked identical to the previous session's
`multiline-column` trap — `inputrc/bind.go:20` defines
`enable-bracketed-paste: false`, builtin options only apply to unset
keys — so it was written up as the same class of bug.

It is not. **`enable-bracketed-paste` was already set** at
`editor_reef.go:145`, and `TestReefConfigGuards` already asserted it.
The harness was the problem: raw text with embedded `\r` is *fast
typing*, not a paste. A real terminal, having seen `ESC[?2004h`, wraps
pasted text in `ESC[200~` / `ESC[201~`.

Once the driver brackets pastes the way a terminal does — tracking
DECSET 2004 in the output stream and wrapping only when the app has
enabled it — stock HEAD is correct: 4 spaces stay 4, 8 stay 8, and a
trailing newline waits for a deliberate Enter. The duplicate config line
added mid-session was reverted.

**Rule:** when emulating a terminal, emulate the mode negotiation too.
Feeding an app input that no real terminal would produce tests a
configuration that does not exist.

## The real bug: LastStatus is shell-only

`false` → prompt shows `[1]`. Then `fmt.Println("go-ok")` prints `go-ok`
and the prompt STILL shows `[1]`. A shell command clears it; a Go
statement never does.

Cause: `LastStatus` was written in exactly two places, both in
`internal/shellexec/exec.go`. The Go leg has no notion of exit status,
so a unit with no shell command in it left whatever the previous shell
pipeline had set. The staleness reached the prompt badge
(`internal/repl/prompt.go:54`) and `status()` / `ok()` in
`internal/builtins/builtins.go:102-103`.

### Fix

**`internal/shellexec/state.go`** — added `StatusSeq uint64` and a
`SetStatus` helper. Every write to `LastStatus` goes through it, so the
counter cannot drift from the value; that invariant is the whole point
of the counter.

A counter rather than a bool because there is no natural place to clear
a flag: the shell leg has many entry points (pipelines, `$()` capture,
`&&`/`||` chaining) and no single "unit finished" hook.

**`internal/shellexec/exec.go`** — both assignment sites route through
`SetStatus`.

**`internal/runner/session.go`** — `RunSource` snapshots the counter and
reconciles in a deferred func:

```go
seq := s.st.StatusSeq
defer func() {
    if s.st.StatusSeq != seq { return } // shell spoke; its status stands
    if _, isExit := errors.AsType[shellexec.ExitErr](err); isExit { return }
    if err != nil { s.st.SetStatus(1); return }
    s.st.SetStatus(0)
}()
```

Three details that are load-bearing:

- **Registered FIRST so it runs LAST.** The existing `recover()` defer is
  registered after it and therefore executes before it, so a panic has
  already been folded into `err` by the time the status is decided.
- **Registered at the top of the function**, which also covers the early
  `ParseError` returns above the interpreter call.
- **Guarded by the seq, not by chunk kinds.** A Go statement can still
  reach the shell — a `{expr}` interpolation, a `$()` capture — and that
  pipeline's status must win. No inspection of `chunks[].Kind` would
  catch those; an untouched counter does.

`ExitErr` returns early so `exit N` keeps its own code.

### Tests — `internal/runner/status_test.go`

`TestGoUnitStatus` pins both directions in one table: four regression
cases (go call / go declaration after a failure → 0; go runtime error /
go parse error → 1) and four guard cases (shell failure reported, shell
success clears, `$(false)` inside a Go unit wins, trailing shell command
wins).

Verified non-vacuous: with the three source files stashed, exactly the
four regression cases fail (`LastStatus = 1, want 0` and the inverse)
while the four guards pass either way — which is what a guard should do.

`TestExitKeepsItsCode` guards the one path the reconciliation must not
touch.

### Not in scope, checked anyway

`-c` exit codes are byte-identical to the pre-fix baseline across
`true` / `false` / Go runtime error / `exit 7` / `false; fmt.Println(...)`.
Those come from the error TYPE in `cmd/grsh` (ParseError → 1, runtime →
2), not from `LastStatus`. Also noted while checking: `false;
fmt.Println("recovered")` on one line exits 127 "command not found" —
the classifier reads the `;`-separated tail as shell. Pre-existing,
confirmed against a stashed build, untouched here.

## Verification

`go build ./... && go vet ./... && go test ./...` — all green, including
the four pty e2e tests.

REPL confirmation of the fix: `false` → `[1]`, then
`fmt.Println("go-ok")` prints and the badge clears; `false` then
`x := 5` also clears.

## Open, not bugs

- **`^E` / End do not accept ghost text.** fish accepts on both; here
  only forward-char and forward-word are wired, which matches the
  comment at `editor_reef.go:251`. A deliberate gap, worth closing if
  fish muscle memory matters.
- **One blocking `ESC[6n` per keystroke.** Measured: ~140 B repainted
  per keystroke on a single row, ~430 B on a 3-row pending unit, and
  1.0 DSR probes per keystroke in every case. Each probe is a round trip
  to the terminal, so interactive latency over ssh has a floor of one
  RTT per character typed. `set cursor-position-probe off` in an inputrc
  avoids it at the cost of the accurate start-column the gutter's
  alignment depends on.

## Gotchas for next time

- **Emulate terminal mode negotiation, not just bytes.** The bracketed
  paste retraction above.
- **Never chain input keys when attributing an effect.** One key per
  fresh session; report screen and buffer separately.
- **A wait that can be satisfied by the previous frame is not a wait.**
  Anchor on a byte count taken before the write.
- **Report background color, or do not make legibility claims.**
- **`LastStatus` is a shell concept.** Anything new that reads it from
  the Go side should ask whether it wants the reconciled per-unit status
  or the last shell pipeline's status specifically.
