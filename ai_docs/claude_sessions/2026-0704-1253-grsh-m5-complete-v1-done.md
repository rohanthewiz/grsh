# grsh v1 COMPLETE — M5 finished (docs, corpus, goldens, CLI tests)

Session ID: `fedbfbb0-5706-45b8-85ac-0749bb4a29f3`
Date: 2026-07-04
Repo: `~/projs/go/grsh` → https://github.com/rohanthewiz/grsh (public, alpha)
Previous doc: `2026-0704-1230-grsh-v1-milestones-1-4.md` (architecture + package map — read that first)

## State: all five v1 milestones complete

`go test ./...` green, gofmt/vet clean. Repo pushed through this session's
final commit. Task list (harness): tasks #1–#5 all completed.

## What M5 added (this session's second half)

1. **docs/LANGUAGE.md** — the full language reference: 9-rule
   classification table + escape hatches, continuations (incl. the
   block-open vs composite-literal `{` distinction), shell feature
   reference, supported/unsupported Go subset, bridge semantics
   (`$()` capture 1/2-value, `{expr}` splicing, `sh()`/`capture()`),
   helper builtin + registry tables, exit codes, bash-differences table.
2. **Classifier corpus → ~160 lines** with documented tie-breaks:
   httpie `a:=1` → Go unless quoted; `make build` shell vs `make(...)` Go;
   `sh` prefix; `[ -f x ]` shell; `time ls` shell.
3. **40 golden scripts** (target met). New batch 2: defer_order, env_vars,
   pipes_go (map counting from shell output), json_report, text_processing,
   escape_hatches, import_stmt, recursion_mutual, compare_ops, var_decls.
4. **cmd/grsh/main_test.go — CLI e2e** (builds the binary in TempDir):
   `-c`, `-version`, usage exit 2, shell parse error → exit 1 +
   `file:line` on stderr, Go parse error → exit 1, runtime error → exit 2
   + `undefined: x`, `exit 5` → 5, script args, **shebang execution**,
   `--debug` stack-trail (serr `in_func` chain shows inner→outer).
5. **min/max** interpreter builtins; Go builtin names predeclared in the
   classifier (call-position `delete(m,k)` is Go; `make build` stays shell).
6. **Fixes found by verifying generated goldens** (never trust -update
   blindly — this caught three):
   - `_ = expr` classified shell → ran a command named `_`. Fix: `_` is
     always "declared" in rule 6a.
   - `import` was missing from the classifier keyword set → `import
     "strings"` would have run a command named `import`.
   - `splitQuoted` ate backslashes in double quotes → alias
     `printf "%s!\n"` lost its newline. Now only `\\` and `\"` escape.
   - (Earlier in session: `sh -c ...` trips the sh escape prefix — by
     design; use `/bin/sh -c`. Documented in LANGUAGE.md §escape hatches.)
7. Removed orphaned `internal/scan`; alias values split quote-aware;
   README updated to "alpha — v1 feature-complete".

## Where things live (delta from previous doc)

- `docs/LANGUAGE.md` — user-facing spec (keep in sync with code!)
- `cmd/grsh/main_test.go` — CLI/exit-code/stderr contract
- Corpus + tie-breaks: `internal/classify/classify_test.go` (the
  `scoped` map declares idents some entries rely on)
- Goldens: `testdata/scripts/` — 40 × `.grsh`/`.want`, optional `.exit`
  (expected exit code) and `.args` (script args, whitespace-split).
  Regenerate: `go test ./internal/runner -run TestGolden -update`
  **then eyeball every changed .want** — that review caught real bugs.

## Known v1 limitations (documented, intentional)

No `&`/job control, heredocs, process substitution, brace expansion,
goroutines/channels, methods on script types, generics, labels, type
switches, `fallthrough`, spread calls, fixed arrays, pointers.
Func-type assertions (`x.(func(int) int)`) unsupported (typeOf has no
FuncType). Typed-nil comparisons (`m == nil` for a nil map) differ from
Go. `$?` is `status()`.

## v2 roadmap (in priority order discussed)

1. **Interactive REPL** — Session.Eval seam is ready; needs line editor
   (chzyer/readline or bubbline), prompt, completion, history; classifier
   already incremental (scope/depth persist).
2. Background jobs `&` + job control; pipefail toggle.
3. Heredocs; struct methods; wider registry (net/http? bufio scanning).
4. `--explain` as full trace mode; nicer bash-porting errors (e.g. hint
   when `if [ ... ]; then` fails to parse).
