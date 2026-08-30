# The grsh Language

grsh scripts (`.grsh` files) interleave two worlds in one file:

- **Shell lines** — run as processes, exactly as you'd type them in bash.
- **Go lines** — a practical subset of Go for all logic.

Every line is classified as one or the other by deterministic rules
(below). Run any script with `--explain` to see each line's decision and
the rule that made it.

```
#!/usr/bin/env grsh

# commands work like bash
ls -la ~/projs
cat access.log | grep 500 > errs.txt

# logic is Go
for _, f := range glob("*.go") {
    out := $(wc -l {f})
    if len(fields(out)) > 0 {
        fmt.Println(f, fields(out)[0], "lines")
    }
}
```

---

## 1. Line classification

Rules are applied top-to-bottom per logical line; the first match wins.

| # | Rule | Result | Examples |
|---|------|--------|----------|
| 1 | Blank, or starts with `#` or `//` | comment (skipped) | `# note`, `// note` |
| 2 | First word is `sh` | **shell** (prefix stripped) | `sh time ls` |
| 3 | First char is `(` | **Go** (bare expression) | `(x + 1)` |
| 4 | First char is `{` or `}` | **Go** | `}`, `} else {` |
| 5 | First word is a Go keyword¹ | **Go** | `for`, `if x > 1 {`, `var n int`, `return` |
| 6 | Contains `:=` outside quotes/parens | **Go** | `x := 5`, `out := $(date)` |
| 7 | First word is a **declared** identifier followed by `=` `,` `(` `[` `++` `--` `+=` `-=` `*=` `/=` `%=` or a selector-shaped `.`² | **Go** | `x = 1`, `x++`, `myfn(3)`, `r.Stars = 9` |
| 8 | First word is a registered package immediately followed by `.` | **Go** | `fmt.Println("hi")` |
| 9 | Everything else | **shell** | `git status`, `dd if=/dev/zero`, `time ls` |

¹ The keyword set is `if for func var const type return switch select
defer break continue fallthrough case default else struct interface map
chan range`. **`go` is deliberately excluded** — `go build ./...` is a
command. Goroutines are planned for v2 with a narrower rule.

² "Selector-shaped" means `.` followed by a letter or `_`. So `x.field`
is Go, but `cd ..`, `cd ./dir`, and `cd .` stay shell even though `cd` is
a declared builtin.

### Escape hatches

| You want | Write |
|----------|-------|
| Force a line to be shell (e.g. a var shadows a command name) | `sh time ls` |
| Run the real `sh` binary | `command sh -c '...'` or `/bin/sh -c '...'` |
| Evaluate a bare Go expression | wrap it: `(x + 1)` |
| A literal `{` in a command | `\{`, or put it in single quotes |

### Declared identifiers

The classifier tracks declarations as it reads: `x := ...`, `var`/`const`/
`type` names, `func` names and parameters, and range variables. All
top-level `func`/`var`/`const`/`type` names are pre-collected, so forward
references classify correctly. The interpreter's builtins (`glob`,
`status`, `errexit`, ...) are pre-declared.

### Logical lines and continuations

**Shell lines** continue onto the next physical line after a trailing
`\`, `|`, `&&`, or `||`:

```
cat report.txt |
  grep -v noise |
  sort
```

**Go lines** follow Go's own semicolon-insertion rule: a line continues
when it can't end where it is — after a trailing operator or comma, or
with unbalanced `(`/`[`:

```
total := base +
    bonus
fmt.Println(total,
    "points")
```

A trailing `{` behaves two ways, matching Go intuition:

- After a control header or closure header (`for ... {`, `if ... {`,
  `func(...) {`) it **opens a block** — the lines inside are classified
  individually, so shell commands work inside loops.
- After anything else (`m := map[string]int{`, `type T struct {`) it's a
  **multi-line literal/declaration** — lines join until braces balance,
  and nothing inside is treated as shell.

---

## 2. Shell features

### Words and quoting

| Form | Behavior |
|------|----------|
| `'single'` | Literal. No expansion of any kind (`awk '{print $1}'` works unmodified). |
| `"double"` | `$VAR`, `${VAR}`, `$(cmd)`, and `{expr}` expand; glob and tilde do not. |
| `\x` | Escapes any single character (`\*`, `\{`, `\ `). |
| `~`, `~/path` | Home directory (word start, unquoted only). |
| `*` `?` `[...]` | Glob per word. **An unmatched pattern passes through literally** (like bash without nullglob). Quoted glob characters never match. |

### Expansions

| Form | Behavior |
|------|----------|
| `$VAR`, `${VAR}` | Environment variable. **Never field-split** (zsh-like): a path with spaces stays one argument. Empty/unset expansions drop the word unless quoted. |
| `$0` | Script name. |
| `$1`..`$9`, `$#` | Script arguments and count. |
| `$@` | All script arguments, **always one field per argument** (bash `"$@"` semantics, quoted or not). |
| `$(cmd)` in a shell word | Command substitution. Unquoted: trimmed output **is** field-split (`kill $(pgrep myapp)` works). Quoted: one word, whitespace preserved. |
| `{expr}` | Go interpolation — see §4. |

Notes:

- `${VAR}` accepts only a plain name (or `@`, `#`, digits). Parameter
  expansion operators — `${VAR:-default}`, `${VAR%.txt}`, `${#VAR}` —
  are **rejected at parse time** with a hint (bash would compute them;
  silently expanding empty would be worse). For defaults use
  `iff(env("VAR") == "", "fallback", env("VAR"))`.
- `$10` parses as `$1` followed by `0`, exactly as in bash; use `args()`
  on the Go side for arbitrary indexing.

### Per-command environment

`FOO=bar cmd args...` runs `cmd` with `FOO` set in its environment only
(`GOOS=linux go build` works). Multiple prefix assignments stack. A
**bare** `FOO=bar` is an error, with a hint: grsh has one variable
model — Go (`FOO := "bar"`) — plus the environment (`export FOO=bar`);
a third, shell-local namespace would blur it. Prefix assignments before
a shell builtin are accepted but have no effect. If `FOO` is a declared
Go identifier, `FOO=bar cmd` classifies as Go (rule 6a) — rename the
variable or use `env` explicitly.

### Operators and redirection

Pipes `|`, sequencing `;`, short-circuit `&&` / `||`, background `&`, and:

| Redirection | Meaning |
|------------|---------|
| `> f`, `>> f` | stdout truncate / append |
| `< f` | stdin from file |
| `2> f`, `2>> f` | stderr truncate / append |
| `2>&1`, `1>&2` | duplicate one fd onto another |
| `&> f`, `&>> f` | stdout **and** stderr |
| `<<EOF`, `<<-EOF`, `<<'EOF'` | heredoc — see below |

Only fds 0–2 are supported. A `#` at the start of a word begins a
comment; mid-word `#` is literal (`file#1` is one word).

### Heredocs

```
cat <<EOF > cfg.json
{"user": "$USER", "host": "$(hostname)"}
EOF
```

The body is the following lines up to a line that is exactly the
delimiter. Inside it, `$VAR`, `${VAR}` and `$(...)` expand (`\$` and
`\\` escape); **`{expr}` Go interpolation is deliberately NOT live in
heredoc bodies**, so JSON braces pass through untouched — interpolate
via `$(...)` or a variable if you need a computed value. Quote the
delimiter (`<<'EOF'`) for a fully literal body. `<<-EOF` strips leading
tabs from body and delimiter lines. Heredocs feed pipelines
(`cat <<EOF | jq .`), combine with other redirects, and work on
background jobs (the body expands eagerly at launch, like every other
redirect). Two on one line read their bodies in operator order; the
last one owns stdin. Heredocs inside `$(...)` command substitution are
not supported.

In the REPL, an open heredoc keeps the continuation prompt until you
type the delimiter line.

### Builtins

| Builtin | Notes |
|---------|-------|
| `cd [dir]`, `cd -` | Changes the real working directory; sets `PWD`/`OLDPWD`. No arg → home. |
| `export K=V ...` | Sets environment variables (always exported — grsh uses the real process environment). |
| `unset K ...` | Removes variables. |
| `exit [n]` | Ends the script with status n. Works from sourced files too. |
| `alias k='v'`, `alias`, `unalias k` | Command-position substitution. v1 limitation: alias values are split on whitespace (no nested quoting). |
| `source f.grsh`, `. f.grsh` | Runs another script **in the current session** — its variables, functions, aliases, and exports persist. |
| `command cmd ...` | Bypasses aliases and builtins. |
| `jobs` | Lists background jobs; finished jobs are reported once and removed. |
| `wait [%N ...]` | No args: collect every job (status 0). With specs: block on those jobs; the status is the last job's. Collected jobs leave the table. |
| `fg [%N]` | Brings a job to the foreground (newest by default): a stopped job gets the terminal back, SIGCONT, and a suspendable wait; a running `&` job is simply waited for (its stdin is `/dev/null`). Echoes the command line, takes the job's status. |
| `bg [%N]` | Resumes a stopped job in the background; it is announced at the prompt when it finishes. |
| `kill [-SIG] %N ...` | Signals the job's whole process group (default TERM; names like `-KILL`/`-9` accepted). `kill` with plain pids stays the external command. |

### Background jobs (`&`)

A trailing `&` runs the whole and-or chain as a job: `make -j8 &`,
`build && notify done &`. `&` also separates, so `sleep 9 & echo hi`
prints immediately. Job specs are `%N`, `%%`, or `%+` (newest).

```
grsh ~/proj> make -j8 > build.log 2>&1 &
grsh ~/proj> jobs
[1]  Running    make -j8 &
grsh ~/proj> wait %1        # block; status() reports make's exit
```

Deliberate v1 semantics (each differs from bash — see §8):

- **Expansion is eager.** `$VAR`, `{expr}`, aliases, and redirect targets
  are expanded when the job *launches*, not lazily in a subshell. The
  async part never touches interpreter state.
- **Builtins cannot be backgrounded** (`cd /tmp &` is an error) — there
  is no subshell to run them in.
- Jobs run in their **own process group**: Ctrl+C at the prompt never
  kills them. Stdin is `/dev/null`, so a background job cannot steal
  interactive input.
- Job stdout/stderr go to the terminal (or wherever redirected). Inside
  a `$(...)` capture, background output is **discarded** — redirect to a
  file to keep it.
- The interactive prompt announces finished jobs (`[1]  Done    cmd &`);
  scripts exit without waiting for jobs (use `wait`). There is no `$!`;
  use `wait %N` + `status()`.

### Suspending with Ctrl+Z (interactive only)

At the REPL, foreground commands run in their own process group with the
terminal, so **Ctrl+Z suspends them** into the job table:

```
grsh ~/proj> make -j8
^Z
[1]  Stopped    make -j8
grsh ~/proj [146]> bg %1     # resume in the background…
grsh ~/proj> fg %1           # …or bring it back (Ctrl+Z again works)
```

- `$?` after a suspension is 128+SIGTSTP (like bash).
- `kill %N` on a stopped job also sends SIGCONT so the outcome is
  collected — a terminating signal never sits pending on a frozen job.
- **`wait` skips stopped jobs** with a warning instead of blocking:
  nothing could ever resume them while the shell is stuck waiting. `fg`
  or `bg` the job first. (Deviation from bash, which blocks.)
- Only external commands suspend; builtins and Go code run inside the
  shell itself (bash builtins are not suspendable either). Script mode
  has no job control: Ctrl+Z suspends the whole script, as with bash.

### Failure behavior

A failing command prints its own stderr and sets the status; the script
**continues** (like bash without `set -e`):

- command not found → status 127
- permission denied → status 126
- nonzero exit → that status

`errexit(true)` enables `set -e` behavior: any failing statement-position
command silently ends the script with that command's status. Read the
last status from Go with `status()` (int) or `ok()` (bool). There is no
`$?`.

A pipeline's status is its **last** command's status (bash default).
`pipefail(true)` switches to the rightmost *nonzero* status, like
`set -o pipefail`; it applies to foreground pipelines and is captured at
launch for background jobs. Combine with `errexit(true)` for strict
mode.

Statement-position commands inherit the terminal (stdin/stdout/stderr),
so interactive tools — `less`, `vim`, password prompts — work. Only
captures buffer output.

---

## 3. The Go subset

### Supported

- `var`, `const`, `:=`, `=`, multi-assignment, `+=` and friends, `++`/`--`
- Types: `bool`, `int`, `int64`, `float64`, `string`, `byte`, `rune`,
  `any`, `error`; slices `[]T`; maps `map[K]V`; struct **types**
  (declaration, literals, field get/set — no embedding yet)
- A declared struct type works **in type position** like any other:
  `[]Item`, `map[string]Item`, `[][]Item`, `var it Item`,
  `make([]Item, 3)` (elements are zero structs, not nils), a
  struct-typed **field** (`type Order struct { Head Item }`, whose zero
  is a real `Item`), and `v.(Item)`. Nested literals elide their type
  the same way Go's do: `[]Item{{"nut", 2}}`, `Order{Head: {"crate", 1}}`.
  A struct works as a **map key** too (`map[Coord]string`): keys compare
  and hash by their FIELDS, `range` yields the struct back, and grsh
  sorts struct keys so ranging is deterministic. `m[missing]` on a
  struct-valued map yields the **zero struct**, as in Go.
- **A container knows which struct it holds**, at both leaves. `[]Item`
  and `[]Order` are different types to the interpreter, and so are
  `map[Coord]int` and `map[Point]int`, so `v.([]Item)` and
  `v.(map[Coord]int)` are exact even when the container is EMPTY — and so
  is `v.([]map[Coord]int)`, where the map is nested. Storing the wrong
  struct — by `append`, an index assignment, a literal, or as a map key —
  is an error naming both types rather than a silent success or a lookup
  that quietly misses.
- **Struct equality**: `p == q` compares **field-wise**, as Go does, so
  a copy equals its original and two separately built literals with the
  same fields are equal. It recurses into struct-typed fields, works in
  `switch`, and answers `false` across two different struct types (Go
  rejects that at compile time; grsh has no compile time). A struct with
  a slice, map or func field is refused with the offending field named,
  the way Go refuses it — comparing one is an error, not a `false`.
  The same verdict decides map keys: `map[P]V` is refused for exactly
  the P that `==` refuses.
- **Struct methods**: top-level `func (p Point) Norm() float64 {...}`
  and `func (p *Point) Scale(f float64) {...}`. Go semantics: a value
  receiver sees a copy, a pointer receiver mutates the instance.
  Methods hoist like functions (declaration order doesn't matter) and
  work in `{expr}` interpolation. Method *values* (`f := p.Norm`) are
  not supported — call them.
- `if`/`else` (with init), all `for` forms, `range` (slices, strings,
  maps, integers), expression `switch` (with init and `default`),
  `break`/`continue` (unlabeled), `defer` (LIFO, args evaluated at defer
  time), `return`
- Functions: `func name(...)` at top level (hoisted — forward references
  and recursion work), closures via `f := func(...)`, variadic parameters,
  multiple returns
- Builtin functions: `len`, `cap`, `append`, `make`, `delete`, `copy`
- `iff(cond, a, b)` — the missing ternary. Lazy like a real `?:`: only
  the taken branch evaluates, so `iff(len(xs) > 0, xs[0], "none")` is
  safe on an empty slice. Nest it for chains.
- Conversions: `int(x)`, `float64(x)`, `string(r)` (from rune/byte/[]byte),
  `rune(x)`, `byte(x)`, `int64(x)`. **`string(65)` of an int is refused**
  (a classic Go footgun) — use `strconv.Itoa`.
- Type assertions `v.(T)` incl. comma-ok; map comma-ok `v, ok := m[k]`
- Indexing, slicing `s[i:j]`, string concatenation and comparison
- Methods on **registry values** via reflection (e.g.
  `regexp.MustCompile(p).FindString(s)`, `time.Now().Year()`)

### Not supported yet

Goroutines/channels/`select`, struct embedding, method values, interfaces
beyond `any`/`error`, generics, labels, type switches, `fallthrough`,
spread calls (`xs...`), fixed-size arrays, pointers (beyond method
receivers). Unsupported constructs fail with a positioned error naming
the construct.

All compound assignments work, including the bitwise set
(`&= |= ^= <<= >>= &^=`), as do unary `^x` complement and `&^`.

### Semantics notes

- Values are native Go values; ints are `int`, floats `float64`. Mixed
  int/float arithmetic promotes to `float64`.
- **Ranging a map iterates in `fmt`'s order** — the same order
  `fmt.Println(m)` prints, so a range and a print of one map agree, and
  a script sees the same order on every run. That covers ints, floats,
  strings, bools, struct keys (field by field), and a `map[any]V` whose
  keys share one type. Scripts are deterministic by default.

  Three key types are the exception, and cannot be otherwise: a map keyed
  by a **pointer, a channel, or an `unsafe.Pointer`** ranges in Go's own
  randomised order, because `fmt` orders those by a machine address that
  changes between runs. A key mixing two dynamic types — `map[any]V`
  holding both an `int` and a `string` — falls back to the order the keys
  RENDER in, which is deterministic but is not `fmt`'s.
- `type` declarations create dynamic struct types; `fmt.Println(v)`
  prints them as `Name{Field: val, ...}`.
- **`== nil` unwraps reference values.** A nil map, slice, func, chan or
  pointer compares equal to `nil` (`var m map[string]int; m == nil` is
  true; `re, err := regexp.Compile("(")` leaves `re == nil` true), and so
  does the zero a missing key hands back for a slice-typed element. An
  empty-but-built `[]int{}` or `map[string]int{}` is NOT nil, as in Go.
  Scalars keep their own zeros: `0 == nil` and `"" == nil` are false.
  One exception, which is Go's own rule: a pointer whose type implements
  `error` stays **non-nil** even when the pointer is nil — that value is
  a live failure to the error-return convention below, and calling it
  nil would let `if err != nil` step past an error the one-value form of
  the same call aborts on.
- **`%T` prints storage, not script types.** A script struct has no Go
  type of its own, so `fmt.Printf("%T", it)` reports grsh's internal
  representation. Every message grsh itself produces names the script's
  own type (`cannot use Order{...} (Order) as Item`); only a raw `%T`
  handed straight to Go's `fmt` shows through.
- Two identical `type` declarations of the same name — the same fields
  with the same types, declared twice, e.g. inside a loop — share one
  container-storage type, so a `[]P` built under one accepts a `P` built
  under the other. `p.(P)` and `p == q` still tell them apart. A struct
  MAP KEY splits the same way and lands on the `==` side: a `map[P]int`
  accepts a `P` from either declaration, but the keys already in it were
  stored under the declaration that built them, so a lookup made after
  re-declaring `P` misses.
- Top-level `return` ends the script (status 0).
- `import "strings"` lines are accepted and validated but optional — all
  registry packages are pre-loaded.

### Error-return convention

Calls returning `(T, error)` follow Go-with-teeth rules:

```
data := os.ReadFile("cfg.json")     // error non-nil → script aborts (with position)
data, err := os.ReadFile("cfg.json") // you own the error
```

The exception is `$(...)` capture — see next section.

---

## 4. The bridge between worlds

### `$(cmd)` in Go context — capture

```
out := $(git branch --show-current)     // trimmed stdout; never aborts
out, err := $(git branch --show-current) // err non-nil on nonzero exit
```

- stdout is buffered and trailing newlines trimmed (bash semantics);
  stderr passes through to the terminal.
- Single-value form never aborts — check `status()`/`ok()` if you care.
- Nonzero exit yields a serr-wrapped error with the status and position.

### `{expr}` in shell context — interpolation

Any Go expression can be spliced into a command word:

```
name := "access.log"
grep 500 {name}                     // one argument, even with spaces
files := []string{"a.txt", "b c.txt"}
wc -l {files}                       // splices: three arguments total
echo "built at {time.Now().Year()}" // other types go through fmt.Sprint
```

- A `string` is **exactly one word** — no word-splitting, ever. Safer
  than bash; no quoting dance needed for paths with spaces.
- A `[]string` splices into one word per element.
- `{}` (empty) is literal, so `find . -exec wc {} \;` works.
- Inert inside single quotes; active in double quotes and bare words.

### Dynamic command lines

When you need to *build* a command string at runtime:

```
err := sh("tar -czf backup.tgz " + dir)       // run it
out, err := capture("git log --oneline -" + n) // capture it
```

(`sh()`/`capture()` strings are parsed as shell but do not support
`{expr}` — you're already in Go; concatenate instead.)

---

## 5. Helper builtins

Pre-declared in every script:

| Function | Description |
|----------|-------------|
| `glob(pat) []string` | Filename expansion, empty slice when no match |
| `lines(s) []string` | Split on newlines, trailing newline ignored |
| `fields(s) []string` | Split on any whitespace |
| `trim(s) string` | `strings.TrimSpace` |
| `readFile(p) (string, error)` | Whole file as a string |
| `writeFile(p, s) error` / `appendFile(p, s) error` | Write/append a string |
| `exists(p) bool` | Path exists |
| `env(k) string` / `setenv(k, v)` | Environment access |
| `cd(dir) error` / `pwd() string` | Directory control from Go |
| `args() []string` | Script arguments |
| `status() int` / `ok() bool` | Last pipeline status |
| `errexit(on bool)` | Toggle abort-on-failure (`set -e`) |
| `pipefail(on bool)` | Pipeline status = rightmost nonzero (`set -o pipefail`) |
| `sh(cmdline) error` / `capture(cmdline) (string, error)` | Dynamic commands |

## 6. Registry packages

Scripts can call a curated surface of these packages directly:

`fmt`¹ · `strings` · `strconv` · `os`² · `filepath` · `time` · `regexp` ·
`json`³ · `sort` · `math` · `errors` · `serr` · `logger`

¹ `fmt.Println`/`Print`/`Printf` write to the session's stdout, so their
output is capturable and redirectable like any command output.

² `os.ReadFile` returns a `string`; `os.WriteFile`/`MkdirAll` default the
permission bits — script-friendly adaptations.

³ Adapted for scripting (no pointers in scripts): `json.Parse(s) (any,
error)` replaces `Unmarshal`; `json.Marshal`/`MarshalIndent` return
`string`.

Unknown symbols fail with a positioned error. The surface is deliberately
curated; ask for what you're missing.

## 7. Running scripts & exit codes

```
grsh script.grsh [args...]      # run a script
grsh -c "ls | wc -l"            # run a one-liner
./script.grsh                   # via shebang: #!/usr/bin/env grsh
grsh                            # interactive REPL (stdin is a terminal)
echo 'ls | wc -l' | grsh        # piped stdin runs as a script
```

### Interactive mode

Running `grsh` with no arguments at a terminal starts the REPL. The same
classifier and interpreter run behind the prompt, and state persists
across inputs: variables, functions, structs, aliases, the working
directory, and exported environment all carry forward, exactly as if the
lines were a script evaluated incrementally.

```
grsh ~/projs> x := 40
grsh ~/projs> if x > 1 {
  ... fmt.Println("x is", x+2)
  ... }
x is 42
grsh ~/projs> echo shell sees {x}
shell sees 40
```

- **Startup file** — `~/.grshrc` (or `$GRSH_RC`) is sourced before the
  first prompt; it runs through the same classifier, so it can mix
  shell (aliases, `export`s) and Go (helper funcs). `-norc` skips it.
  `grsh init` generates a starter `~/.grshrc` from your zsh startup
  files: safe lines (aliases, exports, PATH edits, plain commands)
  come across active, zsh-specific lines are preserved as comments,
  and functions get porting TODOs. An existing `~/.grshrc` is never
  overwritten (`~/.grshrc.new` is written instead).
- **Prompt** — `grsh <cwd>> `; after a failing command it shows the
  status: `grsh <cwd> [1]> `. Set `$GRSH_PROMPT` to customize: `%d`
  cwd, `%s` status, `%g` git branch (read from `.git/HEAD`, no fork),
  `%t` time, `%j` job count, `%%` literal, plus `{red}`/`{cyan}`/
  `{bold}`/`{reset}`-style color tags (dropped under `NO_COLOR`,
  `TERM=dumb`, or a non-terminal).
- **Continuation** — the `... ` prompt appears while the input unit is
  incomplete: an open `{` block or composite literal, a Go line ending
  mid-expression, a pending heredoc body, or a shell line ending in
  `\`, `|`, `&&`, `||`. Open Go blocks show a **construct breadcrumb**
  (`… func greet ▸ for`) below the input, and **auto-indent** seeds
  each new line with two real spaces per open block; typing `}` as the
  first character of such a line dedents it one level, gofmt-style.
  Heredoc bodies are never auto-indented — their lines are literal.
- **Syntax highlighting** — live, classifier-aware: Go lines color
  keywords, strings, numbers, and comments; shell lines color the
  command word green if it resolves (builtin, `$PATH`, alias, explicit
  path) and red if it doesn't, plus flags, `$variables`, quoted
  strings, and comments. `{go interpolations}` inside shell lines stay
  plain. Disabled under `NO_COLOR`, `TERM=dumb`, or a non-terminal.
- **Ghost text** — fish-style inline autosuggestion: the most recent
  unit in `~/.grsh_units` that starts with what you've typed is shown
  dimmed after the cursor. `→` or `Ctrl+F` (at the end of the line)
  accepts the whole suggestion; a forward-word key (`Alt+F`, `Alt+→`,
  `Ctrl+→`) accepts the next word. Nothing is inserted unless you
  accept it. Multi-line units are never suggested (ghost text has to
  fit on the current row), and the feature follows the same color gate
  as highlighting — the suggestion is only distinguishable from typed
  input by being dim.
- **Hint line** — the dim line under the input, composed left to
  right from whatever applies:
  - **Signature help** for registry symbols: with the cursor on
    `strings.Split`, or anywhere inside its still-open call, the line
    shows `strings.Split(string, string) []string`. Reflected from the
    registry itself, so every symbol has one and nothing can go stale
    — at the cost of parameter *names*, which reflection cannot
    recover (`strings.Split(s, sep string)` reads back as
    `(string, string)`). Non-function symbols show type and value:
    `math.Pi float64 = 3.141592653589793`.
  - **Alias expansion**: typing a command that is an alias shows
    `ll → ls -la`, and it stays up while the arguments are typed. The
    literal definition is shown, not the recursive resolution the
    executor performs.
  - The **construct breadcrumb** described under Continuation.
  - **Classification**, under `--explain` only, last: how the
    classifier is reading the line the cursor is on, in the same
    Kind/rule terms the batch output uses — `shell · rule=default`,
    `go · rule=declared-ident`. A chunk covering more than one
    physical line also shows its span (`shell 1-2 · rule=default`),
    which is how you see that a backslash continuation or an open
    composite literal is being read as ONE line. Start an interactive
    session with `grsh --explain` to turn it on; the batch output
    still prints after each unit runs.

  Everything on this line is display-only — nothing is inserted into
  the buffer, and Enter runs exactly what you typed.
- **History** — per-line recall in `~/.grsh_history` (arrow keys,
  Ctrl+R); complete input units (a whole `func` block is one unit) are
  additionally persisted to `~/.grsh_units`, which backs
  `session save`.
- **`?name`** — inspects a live Go variable: type plus pretty-printed
  value (slices and maps as aligned tables, structs by field, closures
  with their parameter list).
- **`session save [N] file.grsh`** — writes this session's input units
  (or the last N units) as a runnable script, shebang included.
  Interactive work and scripts are the same language, so the
  round-trip is exact.
- **Completion** — Tab completes command names from `$PATH`, shell
  builtins (from the live builtin table), declared identifiers,
  registry package names **and their members** (`fmt.Pr<TAB>` →
  `fmt.Println`), Go keywords, and file paths (path-shaped words
  always complete as files).
- **Ctrl+C** — at the prompt, discards the current line (and any pending
  continuation). While a command runs, interrupts the command; the shell
  survives.
- **Ctrl+D** — on an empty line, exits with the last status.
  Mid-continuation, abandons the open block.
- **`exit [n]`** — exits the shell. `errexit(true)` at the prompt sets
  the status on failures but never exits the interactive shell (bash
  behaves the same interactively).
- Single-line eval errors print without a location; multi-line inputs
  keep their line number, and when a column is known the offending
  line is echoed with a caret:

  ```
  grsh: line 2: undefined: nope
      y := x + nope
               ^
  ```

| Exit code | Meaning |
|-----------|---------|
| `n` | The script called `exit n` (or errexit tripped on status n) |
| last status | A script that runs to the end exits with its **last command's status**, like bash (`grsh -c 'false'` exits 1) |
| 1 | Syntax error (shell or Go) |
| 2 | Runtime error |

Errors print as `script.grsh:12: message`. `--debug` (or `GRSH_DEBUG=1`)
prints the full structured error chain, including the script-level
function call trail. `--explain` prints every line's classification; at an interactive
prompt it also puts that verdict in the hint line for the line you are
typing, before you press Enter.

## 8. Deliberate differences from bash — summary

| bash | grsh | Why |
|------|------|-----|
| `$VAR` word-splits | never splits | eliminates the #1 quoting bug class |
| `"$v"` needed everywhere | quoting rarely needed | strings are values |
| `$?` | `status()` / `ok()` | Go-side, explicit |
| `set -e` | `errexit(true)` | explicit, greppable |
| `set -o pipefail` | `pipefail(true)` | same |
| `` `cmd` `` backticks | not supported | `$(...)` only |
| `$((math))` | Go expressions | a real language is right there |
| brace expansion `{a,b}` | not supported | `{...}` is Go interpolation |
| `FOO=bar` shell variable | rejected with hint | one variable model: Go (`:=`) + environment (`export`) |
| `${VAR:-default}` etc. | rejected with hint | silent empty expansion is the worst outcome; `iff(...)` covers it |
| `cmd &` subshell (lazy expansion, builtins ok) | eager expansion at launch; external commands only | single-threaded interpreter; no fork |
| `$!` | `wait %N` + `status()` | explicit, like `$?` → `status()` |
| bg job shares tty stdin | stdin is `/dev/null` | jobs can't steal interactive input |
| `wait` blocks on stopped jobs | warns and skips them | a blocked shell could never resume them |
| heredoc bodies expand `` ` `` and `{`...`}` freely | `$VAR`/`$(...)` only; `{expr}` stays literal | JSON/config bodies survive; quote the delimiter for fully raw |
| heredocs inside `$(...)` | not supported | rarely needed; keeps one-line substitution |
| `<(...)` process substitution | not supported | considered for v2 |
| — | `iff(cond, a, b)` lazy ternary | Go has no `?:`; scripts want one |
