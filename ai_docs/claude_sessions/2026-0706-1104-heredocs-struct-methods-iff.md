# grsh v2 — heredocs, struct methods, iff lazy ternary

Session ID: `02e0ed21-04eb-47ca-9532-875cb3938809`
Date: 2026-07-05 → 2026-07-06
Repo: `~/projs/go/grsh` → https://github.com/rohanthewiz/grsh
Previous docs: `2026-0705-0725-v2-pipefail-and-full-job-control.md` (jobs),
`2026-0705-0117-v2-repl-complete.md` (REPL),
`2026-0704-1230-grsh-v1-milestones-1-4.md` (architecture/package map).

## Commits this doc covers

- `d24ae6c` v2: heredocs — <<, <<- and quoted delimiters end to end
- `2f0eb73` v2: struct methods + iff lazy ternary; LANGUAGE.md

State: `go test -race ./...` green, gofmt/vet clean. NOT pushed (2 ahead).
Remaining v2: wider registry (net/http?, bufio), --explain trace mode,
bash-porting error hints.

## Heredocs — design across the three layers

Layering rule of thumb: parser owns validity, classify owns line
gathering, shellexec owns delivery.

- **shellparse**: `\n` is no longer whitespace (skipSpaces = space/tab
  only). Newline acts like `;` at CmdList level via endLine(), which
  first drains `p.pending` heredocs — each `<<`/`<<-` in parseRedir →
  finishHeredoc records `Redir.Here *Heredoc{Delim, Quoted, StripTabs}`
  and parseCommand registers a pendingHeredoc{cmd, idx} (INDEX not
  pointer — Redirs slice reallocs). readHeredocBody consumes raw lines
  until the delimiter (`<<-` TrimLeft tabs on body AND delimiter;
  delimiter-at-EOF without trailing \n accepted). Unquoted bodies parse
  into Segs via parseHeredocSegs: only `$` is live (reuses parseDollar →
  EnvVar/CmdSub), `\$`/`\\` escape. Quoted delim → single Lit seg.
  parseCommand also breaks on `\n` now (was an infinite-loop hazard) and
  `#` comments eat to end-of-PHYSICAL-line only (`p.i = len(p.s)` would
  have swallowed heredoc bodies).
- **Deviations (documented §LANGUAGE.md)**: `{expr}` is NOT live in
  heredoc bodies (JSON braces survive — interpolate via $() or a var);
  heredocs inside `$(...)` unsupported (Parse of the interior errors
  "unterminated heredoc"); no `<<<` herestrings (delimiter scan stops,
  parser errors).
- **classify**: joinShell → (text, end, err); after continuation
  joining, scanHeredocs(text) finds operators (skipping quotes, balanced
  {expr} via skipGoBrace, word-start `#`), then raw body lines append
  with real `\n` (continuations join with spaces — bodies must not).
  Out of lines → error wrapping ErrIncomplete → REPL NeedsMore=true,
  script run hard-errors. Body lines are INSIDE the shell chunk, so
  `x := 1` in a body never classifies as Go (TestHeredocChunks).
  Corpus entry `cat <<gone.txt` updated — it's a real heredoc now.
- **shellexec**: resolveRedirs (the eager State/ev half — bg jobs expand
  at launch, test flips the env var after launch) → expandHeredoc
  renders Segs (EnvVar join-with-space, CmdSub via Capture) into
  resolvedRedir.hereBody. applyResolved: os.Pipe(), writer goroutine
  (WriteString + Close), read end into closers and fds[0]. Real fd ⇒
  jc-safe (no exec copy goroutines when stdio is all *os.File) and
  builtins/captures just read. No deadlock: unread big body blocks the
  writer only until closeAll closes the read end → EPIPE → goroutine
  exits (TestHeredocUnreadBigBody, 1 MiB).
- Two heredocs on a line: bodies read in operator order, last wins fd 0
  (applyResolved order). fd prefix `N<<` parses; fds 0–2 gate unchanged.

## Struct methods

- **transform**: methodRe at Depth 0 rewrites
  `func (p Point) Dist(` → `__m_Point_Dist := func(p Point, ` (receiver
  prepended to params; `, ` skipped when the param list is empty on the
  same line). Line-preserving; `MethodPrefix = "__m_"` const. Plain
  topFuncRe can't match `func (` so no interference.
- **interp**: hoisting is free — `__m_...` are ordinary top-level
  `name := func` assignments (topFuncAssign), so forward refs/mutual
  calls work. evalCall SelectorExpr: recv is *StructVal →
  callStructMethod: lookupMethod(globals `__m_Type_Name`) → prepend
  receiver → callClosure. Value receiver ⇒ sv.shallowCopy() (Go
  semantics: mutation doesn't leak; verified); pointer receiver (first
  param type is *ast.StarExpr — methodHasPtrRecv) shares the instance.
  Fallback to reflection (String on *StructVal) then a helpful error.
  structField unknown-field error hints when the name is a method
  ("call it: .M(...)") — method VALUES are unsupported.
- Methods work inside `{expr}` shell interpolation (wordEval → eval1 →
  evalCall). Bodies classify per-line like func bodies (shell allowed
  inside methods).

## iff — the ternary

`go/parser` can't lex `?:` and regex-rewriting it is fragile, so:
goBuiltin case "iff" in interp/call.go — evaluates cond (evalBool,
strict), then eval1 on ONLY the chosen branch. Lazy ⇒
`iff(len(xs) > 0, xs[0], "none")` safe; nests; shadowable (goBuiltin
already gated on `!shadowed`); predeclared in session.go's builtin list
so `iff(...)` classifies Go at statement start.

## Tests added

- shellparse: TestParseHeredocs (17 cases incl. two-heredoc, pipeline,
  bg, comment-on-line, fd prefix) + TestParseHeredocErrors (7).
- classify: heredoc NeedsMore cases (incl. `{x << 2}` non-heredoc,
  `# <<EOF` comment), TestHeredocChunks (body swallowed verbatim),
  TestHeredocUnterminated (errors.Is ErrIncomplete).
- shellexec/heredoc_test.go: expansion/quoted/tab-strip/pipeline/
  redirect/double/big-unread/bg-eager-expansion.
- runner/methods_test.go: methods (value-copy, compose+hoist, {expr}),
  method errors, iff (laziness via undefined boom() in dead arm,
  nesting, shell interpolation), iff errors, iff shadowing.
- Goldens: heredocs.grsh, struct_methods.grsh (regenerated with -update
  and EYEBALLED — both correct first try).
- Live pty verification: REPL `...` continuation through a heredoc body,
  $USER expanded; iff at the prompt. (Paced printf | script harness from
  the jobs session doc.)

## Gotchas / notes for future work

- pendingHeredoc must store {cmd, idx}, never &cmd.Redirs[i] — append
  reallocation invalidates interior pointers.
- The `#` comment fix matters beyond heredocs: any future multi-line
  shell construct would have been eaten by `p.i = len(p.s)`.
- scanHeredocs (classify) and finishHeredoc (parser) are parallel
  implementations of delimiter lexing — keep them in sync (classify is
  advisory: wrong count ⇒ parser error, never silent misparse).
- Value-receiver copy is SHALLOW: nested *StructVal fields still share,
  same as Go pointers-in-structs.
- `__m_` names are visible globals; user could collide/inspect. Fine
  for now, documented nowhere user-facing.
- struct_methods.grsh golden uses iff — goldens for the two features
  are coupled to commit 2f0eb73.
- Commit split verified independently green via
  `git stash push -u -- <paths>` → test → commit → pop → test.

## v2 roadmap remaining

1. Wider registry (net/http?, bufio scanning).
2. --explain trace mode; bash-porting error hints.
3. Maybe: struct embedding, method values, `<(...)`.
