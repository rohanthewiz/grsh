# The bridge

Two constructs join the halves, and they are the reason the language
exists: `$(cmd)` hands a command's output to Go, and `{expr}` hands a Go
value to a command.

## step: capture
In Go context, `$(cmd)` is an expression: it runs the command, trims
the trailing newline from its stdout, and evaluates to a `string`.
stderr still goes to your terminal.

Capture the count of failing requests into `errs`.
---
verify: used-construct \$\(
verify: var errs type=^string$ value=^17$
hint: The command is the same `grep -c 500 access.log` from chapter 1.
hint: `errs := $(grep -c 500 access.log)`
solution: errs := $(grep -c 500 access.log)

## step: capture-int
Command output is text, so `errs` is a `string`. The `strconv` package
is pre-loaded.

Turn it into an `int` called `n`.
---
verify: var n type=^int$ value=^17$
hint: `strconv.Atoi` returns `(int, error)`. The one-value form aborts
hint: on an error rather than handing you a zero — take it.
hint: `n := strconv.Atoi(errs)`
solution: n := strconv.Atoi(errs)

## step: capture-two
The single-value form of `$(...)` **never** aborts, even when the
command exits nonzero — check `status()` if you care. The two-value
form hands you the error instead, Go style.

Capture the count of 404s into `out`, taking the error as well.
---
verify: used-construct ,\s*err\s*:=\s*\$\(
verify: var out type=^string$ value=^9$
hint: `out, err := $(...)` — same shape as any `(T, error)` call.
hint: `out, err := $(grep -c 404 access.log)`
solution: out, err := $(grep -c 404 access.log)

## step: interpolate
Going the other way: `{expr}` splices any Go expression into a command
word. A `string` becomes **exactly one argument** — it is never
word-split, however many spaces it holds.

Print `there were 17 errors` using a command, with `n` spliced in.
---
verify: used-construct \{n\}
verify: output-exact there were 17 errors
hint: `echo` is a command; `{n}` inside it is Go.
hint: `echo there were {n} errors`
solution: echo there were {n} errors

## step: splice
A `[]string` splices differently and deliberately: one argument per
element, so a slice of three paths becomes three arguments — never
one word with spaces in it.

Count the lines in every Go file, in one command, by splicing a glob.
---
verify: used-construct \{
verify: output-regexp (?s)greet\.go.*main\.go.*util\.go
hint: You can call a function inside the braces.
hint: `wc -l {glob("*.go")}`
solution: wc -l {glob("*.go")}

## step: fail
There is no `$?`. A failing command still prints its own stderr, sets
the status, and lets the session carry on — like bash without `set -e`.

Run something that fails: list a file that isn't there.
---
verify: status nonzero
hint: Any missing path will do.
hint: `ls nosuchfile`
solution: ls nosuchfile

## step: read-status
Read that status from the Go side. `status()` returns the `int`;
`ok()` returns the `bool`. Both are pre-declared, and both are
greppable in a way `$?` never was.

Print the status of the command you just ran.
---
verify: used-construct status\(\)
verify: output-regexp (?m)^[1-9][0-9]*$
hint: `fmt.Println(status())`
solution: fmt.Println(status())

## step: dynamic
When the command line itself has to be **built** at runtime, `sh(s)` runs
a string and `capture(s) (string, error)` captures one. They parse as
shell but do not expand `{expr}` — you are already in Go, so
concatenate.

Run a command built by concatenation that prints `built ok`.
---
verify: used-construct sh\(
verify: output-regexp (?m)^built ok$
hint: `sh("echo built " + "ok")` — contrived on purpose; the real case
hint: is a path or a flag computed a moment earlier.
solution: err := sh("echo built " + "ok")
