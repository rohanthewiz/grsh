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

### Operators and redirection

Pipes `|`, sequencing `;`, short-circuit `&&` / `||`, background `&`, and:

| Redirection | Meaning |
|------------|---------|
| `> f`, `>> f` | stdout truncate / append |
| `< f` | stdin from file |
| `2> f`, `2>> f` | stderr truncate / append |
| `2>&1`, `1>&2` | duplicate one fd onto another |
| `&> f`, `&>> f` | stdout **and** stderr |

Only fds 0–2 are supported. A `#` at the start of a word begins a
comment; mid-word `#` is literal (`file#1` is one word).

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
| `fg [%N]` | Waits for the job (newest by default), echoes its command line, takes its status. v1: no terminal handoff — fg means "block until done". |
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
- Not yet: Ctrl+Z suspend, `bg`, stopped jobs (planned with terminal
  handoff).

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
  (declaration, literals, field get/set — no methods or embedding yet)
- `if`/`else` (with init), all `for` forms, `range` (slices, strings,
  maps, integers), expression `switch` (with init and `default`),
  `break`/`continue` (unlabeled), `defer` (LIFO, args evaluated at defer
  time), `return`
- Functions: `func name(...)` at top level (hoisted — forward references
  and recursion work), closures via `f := func(...)`, variadic parameters,
  multiple returns
- Builtin functions: `len`, `cap`, `append`, `make`, `delete`, `copy`
- Conversions: `int(x)`, `float64(x)`, `string(r)` (from rune/byte/[]byte),
  `rune(x)`, `byte(x)`, `int64(x)`. **`string(65)` of an int is refused**
  (a classic Go footgun) — use `strconv.Itoa`.
- Type assertions `v.(T)` incl. comma-ok; map comma-ok `v, ok := m[k]`
- Indexing, slicing `s[i:j]`, string concatenation and comparison
- Methods on **registry values** via reflection (e.g.
  `regexp.MustCompile(p).FindString(s)`, `time.Now().Year()`)

### Not in v1

Goroutines/channels/`select`, methods on script types, interfaces beyond
`any`/`error`, generics, labels, type switches, `fallthrough`, spread
calls (`xs...`), fixed-size arrays, pointers. Unsupported constructs fail
with a positioned error naming the construct.

### Semantics notes

- Values are native Go values; ints are `int`, floats `float64`. Mixed
  int/float arithmetic promotes to `float64`.
- Ranging over a **string-keyed map iterates in sorted key order** —
  scripts are deterministic by default.
- `type` declarations create dynamic struct types; `fmt.Println(v)`
  prints them as `Name{Field: val, ...}`.
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

- **Prompt** — `grsh <cwd>> `; after a failing command it shows the
  status: `grsh <cwd> [1]> `.
- **Continuation** — the `... ` prompt appears while the input unit is
  incomplete: an open `{` block or composite literal, a Go line ending
  mid-expression, or a shell line ending in `\`, `|`, `&&`, `||`.
- **History** — persisted to `~/.grsh_history`; arrow keys and Ctrl+R
  search work as usual.
- **Completion** — Tab completes command names from `$PATH`, declared
  identifiers and builtins, registry package names, Go keywords, and file
  paths (path-shaped words always complete as files).
- **Ctrl+C** — at the prompt, discards the current line (and any pending
  continuation). While a command runs, interrupts the command; the shell
  survives.
- **Ctrl+D** — on an empty line, exits with the last status.
  Mid-continuation, abandons the open block.
- **`exit [n]`** — exits the shell. An `errexit(true)` failure also exits
  (same `set -e` semantics as scripts).
- Single-line eval errors print without a location; multi-line inputs
  keep their line number (`grsh: line 2: undefined: x`).

| Exit code | Meaning |
|-----------|---------|
| `n` | The script called `exit n` (or errexit tripped on status n) |
| 1 | Syntax error (shell or Go) |
| 2 | Runtime error |

Errors print as `script.grsh:12: message`. `--debug` (or `GRSH_DEBUG=1`)
prints the full structured error chain, including the script-level
function call trail. `--explain` prints every line's classification.

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
| brace expansion `{a,b}` | not in v1 | `{...}` is Go interpolation |
| `cmd &` subshell (lazy expansion, builtins ok) | eager expansion at launch; external commands only | single-threaded interpreter; no fork |
| `$!` | `wait %N` + `status()` | explicit, like `$?` → `status()` |
| bg job shares tty stdin | stdin is `/dev/null` | jobs can't steal interactive input |
| Ctrl+Z / `bg` / stopped jobs | not yet | planned with terminal handoff |
| heredocs, `<(...)` | not in v1 | planned/considered for v2 |
