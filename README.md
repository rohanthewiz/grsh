# grsh

A Go-powered shell scripting language — bash-style commands and real Go
logic in the same script, run by a single native binary.

```
#!/usr/bin/env grsh

# commands work like bash
ls -la ~/projs
cat access.log | grep 500 > errs.txt

# but logic is Go
for _, f := range glob("*.go") {
    out := $(wc -l {f})            // $(...) captures output as a string
    if len(fields(out)) > 0 {
        fmt.Println(f, fields(out)[0], "lines")
    }
}
```

## Status: alpha — v1 feature-complete, v2 underway

**grsh is alpha software** — the language surface is still settling and
may change without notice. Not yet recommended as your login shell or
for production automation.

All five v1 milestones are complete. Verified by 40 end-to-end golden
scripts, a ~160-line real-world classification corpus, per-package unit
tables, and CLI tests covering exit codes, error positions, and shebang
execution. v2 has begun with the interactive REPL.

- **Interactive REPL** (new in v2) — run `grsh` with no arguments:
  full session state across inputs, multi-line continuation for Go
  blocks and shell pipes, history (`~/.grsh_history`), tab completion
  (PATH commands, your identifiers, file paths), cwd-and-status prompt.
  Piped stdin runs as a script: `echo 'ls | wc -l' | grsh`.
- **Background jobs & job control** (new in v2) — `make -j8 &`, `jobs`,
  `wait [%N]`, `fg`, `bg`, `kill %N`, and **Ctrl+Z** suspends the
  foreground command into the job table (full terminal handoff on
  `fg`). Jobs run in their own process group (Ctrl+C safe) with stdin
  from `/dev/null`; the prompt announces completions. Expansion is
  eager at launch and builtins can't background — see
  [docs/LANGUAGE.md](docs/LANGUAGE.md#background-jobs-) for semantics.
  `pipefail(true)` = `set -o pipefail`.

- **Shell core** — pipes, redirections (`>`, `>>`, `<`, `2>`, `2>&1`, `&>`),
  `&&`/`||`/`;`, quoting, `$VAR`/`${VAR}`, tilde and glob expansion,
  command substitution, line continuations, builtins (`cd`, `export`,
  `unset`, `exit`, `alias`, `source`, `command`), shebang scripts, `-c`.
- **Go engine** — a custom tree-walking interpreter over `go/parser`:
  `:=`/`=`, all `for` forms, `if`/`else`, `switch`, `range`, closures
  (recursion, forward references, variadics, multi-returns), slices, maps,
  struct types, `defer`, type assertions, comma-ok.
- **The bridge** — shell lines inside Go blocks; `$(cmd)` capture with
  one- or two-value assignment (`out, err := $(...)`); `{expr}` Go
  interpolation inside commands (a `[]string` splices into argv);
  `status()`, `ok()`, `errexit(true)` (= `set -e`), `pipefail(true)`
  (= `set -o pipefail`).
- **Curated stdlib** — `fmt`, `strings`, `strconv`, `os`, `filepath`,
  `time`, `regexp`, `json` (script-friendly `Parse`/`Marshal`), `sort`,
  `math`, `errors`, plus [serr](https://github.com/rohanthewiz/serr) and
  [logger](https://github.com/rohanthewiz/logger). Helper builtins:
  `glob`, `lines`, `fields`, `trim`, `readFile`, `writeFile`, `exists`,
  `env`, `setenv`, `cd`, `pwd`, `args`, `sh`, `capture`.

Errors report real `script.grsh:line` positions (a `//line` directive maps
the transformed program back to your source).

**Full language reference: [docs/LANGUAGE.md](docs/LANGUAGE.md)** —
classification rules, shell features, the Go subset, bridge semantics,
builtins, and every deliberate difference from bash.

## How a line is classified

Deterministic rules, in order — see `internal/classify`:

1. Blank, `#`, or `//` line → comment.
2. `sh ` prefix → forced shell. Leading `(` → forced Go expression.
3. Leading `{`/`}` or a Go keyword (`if`, `for`, `func`, `var`, ...) → Go.
   **`go` is not in the list** — `go build` is a command.
4. `:=` outside quotes → Go.
5. A *declared* identifier followed by `=`, `(`, `[`, `.field`, `++`, ... →
   Go; a registered package name followed by `.` → Go (`fmt.Println(...)`).
6. Everything else → shell (`dd if=/dev/zero`, `awk '{print $1}'`,
   `time ls`, and `cd ..` all stay shell).

Run any script with `--explain` to see each line's decision and rule.

## Deliberate differences from bash

- `$VAR` never word-splits (zsh-like); `$(cmd)` output does (unquoted).
- `{expr}` interpolation produces exactly one word for a string; use a
  `[]string` to splice multiple argv words.
- `$?` is spelled `status()`; `set -e` is `errexit(true)`.
- Background jobs (`&`), heredocs, and process substitution are not in v1.

## Build & test

```
go build -o bin/grsh ./cmd/grsh
go test ./...

./bin/grsh script.grsh [args...]
./bin/grsh -c "ls | wc -l"
```

## Architecture

```
.grsh source
  → classify    per logical line: SHELL or GO (scope-tracking)
  → shellparse  shell fragments → AST side table
  → transform   line-preserving rewrite → one valid Go file (//line mapped)
  → go/parser   → interp (tree-walker) → shellexec (os/exec)
```

Inspired by [goshell](https://github.com/ahmedakef/goshell); design notes
live in the milestone plan. v2 targets the interactive REPL (the
`runner.Session.Eval` seam is already in place — classifier scope,
interpreter globals, and the shell side table all persist across Eval
calls), background jobs and job control, heredocs, struct methods, and a
wider registry surface.
