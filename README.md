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
  (PATH commands, shell builtins, your identifiers, `pkg.Member`
  registry symbols, file paths), cwd-and-status prompt.
  Piped stdin runs as a script: `echo 'ls | wc -l' | grsh`.
- **`grsh tutor`** (new in v2) — an interactive tutorial that runs
  *inside* the real REPL: eight chapters, 47 graded steps, in a
  throwaway playground. `grsh-tour` serves the same curriculum in a
  browser. See [below](#learn-it-at-the-prompt-grsh-tutor).
- **Interactive conveniences** (new) —
  `~/.grshrc` startup file (mix shell and Go; `$GRSH_RC` overrides,
  `-norc` skips); `grsh init` translates your `~/.zshrc` into a
  starter `~/.grshrc`; a **construct breadcrumb** (`… func greet ▸ for`)
  below the input always shows what block you're in; **syntax
  highlighting** as you type (Go keywords/strings/numbers/comments;
  shell command names green when they resolve and red when they don't,
  fish-style, plus flags, `$vars`, and strings); **real auto-indent** —
  Enter inside an open block seeds the next line with its depth, and a
  `}` typed on that fresh line dedents itself one level; **ghost text**
  — the most recent history unit starting with what you've typed shows
  dimmed after the cursor, fish-style (→ or Ctrl+F accepts it, a
  forward-word key takes one word); a **hint line** under the input
  showing signature help for registry symbols
  (`strings.Split(string, string) []string`, reflected live), what
  an alias expands to (`ll → ls -la`), and — under `--explain` — how
  the classifier is reading the current line (`go · rule=define`); eval errors print the offending
  line with a **caret** under the column; `?name` **inspects any live
  Go variable** (type + pretty-printed value); `session save
  file.grsh` writes this session's inputs as a runnable script;
  `$GRSH_PROMPT` templates the prompt (`%d` cwd, `%s` status, `%g` git
  branch, `%t` time, `%j` jobs, `{cyan}...{reset}` colors, `NO_COLOR`
  respected); complete input units persist to `~/.grsh_units`.
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

## Learn it at the prompt: `grsh tutor`

```
grsh tutor        # start, or pick up where you left off
grsh tutor 4      # jump straight to a chapter
```

A tutorial that runs **inside the real REPL** rather than simulating
one. You type at an actual grsh prompt with every convenience live —
highlighting, ghost text, the breadcrumb, `?name` — while a lesson
engine prints a panel above the prompt, watches what you run, and grades
it. Grading looks at what a step is actually about: a live variable's
type and value, a file on disk, the exit status, or how the line was
*classified* — not just what it printed.

1. It's just a shell — 2. Two languages, one prompt — 3. Go at the
prompt — 4. The bridge — 5. Helpers and the registry — 6. Where bash
habits break — 7. Jobs — 8. Capstone: session to script

Each chapter runs in a throwaway playground seeded with fixtures (a
120-line `access.log`, three `.go` files, a directory with a space in
its name), deleted when you leave — experiment freely. Chapter 2 turns
`--explain` on for itself, so you watch each classification rule fire as
you type. The capstone builds a report, saves the session as a runnable
script with `session save`, and sources it back; `:keep` copies that
script out before the playground goes.

Your place is saved per chapter (`~/.grsh_tutor.db`), so bare
`grsh tutor` carries on where you stopped. Tutor commands take a colon
prefix — nothing in shell or Go starts with one, so they can never
shadow something you meant to run:

```
:hint   :sol   :skip   :back   :next   :menu (`:menu 4` jumps)
:keep   :progress   :help   :quit
```

### The same tutorial in a browser: `grsh-tour`

```
go build -o bin/grsh-tour ./cmd/grsh-tour
./bin/grsh-tour              # http://127.0.0.1:7654, opens a browser
./bin/grsh-tour -progress    # remember where you got to
```

The same eight chapters, the same verifiers, the same real shell — with
the terminal replaced by a page: a transcript on the left, the lesson on
the right. The commands are real, the playground is a real directory,
and every tutor command still works by typing it; the sidebar's buttons
are a convenience for the common ones. Chapter 2's classifier verdict,
which the terminal shows in the prompt's hint lane, appears under the
input as you type.

It is a **local tool**. The server runs shell commands as you, so it
binds to loopback and refuses anything else without `-allow-remote`, and
that is a reminder rather than a boundary — do not put it on a network.
Each tab gets its own playground; they take turns to evaluate, because a
grsh session's working directory is the process's, so this suits a few
tabs on one machine and nothing larger. Ctrl+C removes the playgrounds.

It is a separate binary on purpose: the tour needs an HTTP server and an
HTML page, the shell needs neither, and `grsh` stays free of a web
framework it would never load.

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
- `FOO=bar cmd` sets a per-command environment (like bash); bare
  `FOO=bar` is rejected — variables are Go (`FOO := "bar"`) or
  environment (`export FOO=bar`).
- `${VAR:-default}`-style parameter expansion is rejected with a hint
  rather than silently expanding empty; plain `${VAR}` works.
- Process substitution `<(...)` is not supported yet.

## Build & test

```
go build -o bin/grsh ./cmd/grsh
go build -o bin/grsh-tour ./cmd/grsh-tour   # the browser tutorial
go test ./...

./bin/grsh script.grsh [args...]
./bin/grsh -c "ls | wc -l"
```

`go test ./...` caches per package, and the pty and tour tests build or
serve things the change may be in — use `-count=1` when a change to
`internal/tutor` or `internal/tour` needs to reach them.

## Embedding

The root `grsh` package hosts a persistent session inside another Go
program — streaming output, `Interrupt`/`Kill` cancellation, no
terminal claims. See [docs/EMBEDDING.md](docs/EMBEDDING.md).

```go
sess := grsh.NewSession(grsh.Options{Stdout: out, Stderr: out})
err := sess.Eval(`ls | wc -l`)
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
