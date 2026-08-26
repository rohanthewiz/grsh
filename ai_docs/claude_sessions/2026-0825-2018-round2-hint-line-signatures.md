# Session: Round 2 — Hint-Line Signatures

Session: a665757a-9751-4cf5-953d-ba7f7115c15d
Date: 2026-08-25

## Goal

Item 1 of the remaining Round 2 backlog: extend the hint provider with
`stdlibreg.Signature(pkg, sym)` reflection help and alias expansion,
composing with the breadcrumb rather than clobbering it. Landed; full
suite green under `-race`, including two pty e2e paths.

## What landed

### `internal/stdlibreg/signature.go` (new)

`Signature(pkg, sym) (string, bool)` — the registry stores symbols as
plain Go values, so the only runtime description available is the
reflected TYPE:

```
strings.Split(string, string) []string
strings.Cut(string, string) (string, string, bool)
fmt.Printf(string, ...any) (int, error)
math.Pi float64 = 3.141592653589793
```

Deliberate trade, written into the code and LANGUAGE.md: **no parameter
names** (reflection cannot recover them — `Split(s, sep string)` reads
back as `(string, string)`), in exchange for zero generated tables to
keep in sync. Every symbol added to a package file gets a hint for free.

- **Bound symbols** (the stdio-dependent `fmt.Println`/`Printf`/`Print`)
  are resolved by calling their binder with `io.Discard`: only the type
  is inspected, and that does not vary with the streams. This also makes
  `Signature` a better existence check than `Lookup`, which is blind to
  the Bound map.
- Variadics are spelled from the slice's element type; a single result is
  bare, multiple results parenthesized — as in Go source.
- `interface {}` → `any`: scripts write `any`, and the reader should not
  need two vocabularies for one thing.
- Non-func entries render `pkg.Sym T = value`, value flattened to one
  line and capped at `maxLiteralRunes` (48).
- `TestSignatureCoversRegistry` walks `Names()` × `Members()` and fails
  if any registered symbol renders emptily — the guard against a future
  entry shape that reflection chokes on.

### `internal/runner/session.go`

`Alias(name) (string, bool)` — the literal right-hand side as defined,
NOT `shellexec.expandAlias`'s recursive resolution: the hint shows what
the user wrote, which is what they are confirming while typing. `IsAlias`
now delegates to it.

### `internal/repl/hint.go` (new)

`hinter` owns the whole lane. The breadcrumb moved here from
`hintProvider` unchanged; two cursor-local lanes joined it. Composed left
to right, cursor-local segment first, `hintSep = "  ▏ "`:

```
strings.Split(string, string) []string  ▏ … func hi
ll → ls -la --color=auto
```

- **Signature lane.** The completed `pkg.Sym` ending at the cursor wins
  (`fmt.Sprintf(strings.Split‸` → Split: it is what is being typed right
  now); failing that, `callee()` — a bracket matcher, not a parser —
  finds the name in front of the innermost still-open `(`. It skips
  interpreted string/char literals **with Go escape rules** (so `'\''`
  is one literal — distinct from the highlighter's `closeQuote`, which
  follows SHELL rules where single quotes have no escapes), raw strings,
  `//` and `/* */`. A stray paren in text therefore cannot wedge the
  stack, and stray closers cannot underflow it.
- **A resolving `pkg.Sym` is itself the evidence that this is Go** — no
  classifier call in this lane at all.
- **Alias lane follows the COMMAND word of the cursor's segment**, not
  the word under the cursor. First cut was `currentWord` +
  `commandPosition`, which died the moment a space was typed; the
  segment's first word keeps the expansion up while arguments are typed,
  the same way signature help stays up inside a call. `commandWord`
  scans for the last `|`/`&`/`;`/`(` outside quotes, then takes
  `Fields[0]`. A half-typed name simply is not in the alias table, so no
  hint appears until the name is complete.
- **The Preview guard is paid only after a name matches the alias
  table.** `ll := 3` is Go and must not hint, so the lane confirms the
  cursor's physical line is a `classify.Shell` chunk — but a classifier
  clone per keystroke would be unacceptable, and after the map lookup it
  is effectively never run.
- **`oneLine` sanitizes the expansion**: alias values are arbitrary user
  text; a newline would cost a screen row and an ESC would corrupt the
  dim run the display engine measures around. Control chars become
  spaces, length capped at 64 runes.
- **Memo is keyed on (buffer, cursor)** — unlike the highlighter's,
  because both cursor-local lanes move with the cursor — and is dropped
  in `Readline`, so an alias defined by the command just run hints at
  the very next prompt.
- `goSignature` checks `trailingSelector` (walks back a few bytes)
  before `callee` (scans the whole prefix); the two were originally in
  one slice literal, which evaluated both eagerly on every keystroke.

### Wiring (`editor_reef.go`)

`reefReader` gained `hints *hinter`, built unconditionally — the lane is
not color-gated (only its dimming is). `hintProvider` is now
ghost-update + `r.hints.hint(line, pos)`, returning nil when empty so the
lane collapses instead of reserving a row. `Readline` calls
`r.hints.reset()` next to the `ghostHold` reset.

### Tests

- `stdlibreg/signature_test.go`: every rendering shape spelled out in
  full, misses, bound-only coverage, the whole-registry sweep,
  literal flattening/capping.
- `repl/hint_test.go`: a `‸` (U+2038) cursor marker — `|` collided with
  the pipeline test buffers, which is how the first run failed. Covers
  the signature lane (nested calls, closed calls, literals, comments,
  multi-line calls, half-typed symbols, shell lines), the alias lane
  (pipelines, `&&`, argument position, Go lines, quoted separators),
  sanitization, composition with the breadcrumb, the memo, and `callee`
  / `trailingSelector` directly.
- `repl/editor_reef_test.go`: `TestReefHintProvider` — the seam. One
  callback feeds both the lane and the ghost; the lane collapses to nil;
  the memo does not outlive a prompt.
- `repl/e2e_pty_test.go`: `TestReefHintLineEndToEnd` — types into a real
  pty, waits for `\x1b[2mstrings.ToUpper(string) string` mid-call and
  `\x1b[2mgsay → printf`, then confirms the buffer was untouched by
  running both commands. Going through a pty is what proves the row math:
  the lane prints BELOW the input, so a bad measurement corrupts the
  prompt rather than merely looking wrong.

### Docs

README "Interactive conveniences" bullet; a new LANGUAGE.md "Hint line"
bullet (sub-bullets for signature help, alias expansion, and the
breadcrumb) placed after Ghost text.

## Gotchas for future sessions

- **`Signature` covers Bound symbols; `Lookup` does not.** Use Signature
  as the existence check for anything user-facing.
- Reflection gives no parameter names. If named parameters ever matter,
  that needs a generated table beside the registry — a real cost the
  current design deliberately avoids.
- **Two quote scanners now exist and must not be confused**:
  `highlight.go:closeQuote` (shell rules, no escapes inside `'`) and
  `hint.go:skipGoQuote` (Go rules, escapes in both `'` and `"`).
  `commandWord` uses the shell one on purpose.
- The hint memo is keyed on the buffer, so it MUST be reset per prompt —
  session state it reads (aliases, idents) changes between prompts.
- Per keystroke the editor now runs: 2 `Pending` (accept-line override +
  breadcrumb), 1 `Preview` (highlighter), 1 history scan (suggester,
  memoized), 1 selector walk + 1 prefix scan (hint), and a `Preview`
  ONLY when a command word matched the alias table. Still the first
  place to look if typing ever feels heavy.

## Found, not fixed

The pty dump shows the `"  ... "` **secondary prompt is not painted on
continuation rows** — the auto-indent spaces appear, the gutter does not.
Pre-existing (predates this session's changes), out of scope here, worth
resolving during the manual smoke pass.

## Still open in Round 2 (next session)

1. User manual smoke: highlighting feel, indent/dedent feel, ghost-text
   feel, the new hint line (is the `▏` separator legible? does the
   signature flicker on fast typing?), the missing `... ` gutter above,
   ^Z/fg on a live job, paste, recall. `GRSH_EDITOR=legacy` if anything's
   off.
