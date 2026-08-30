# Where bash habits break

Five places grsh deliberately refuses to do what bash does. Each refusal
is loud, and each one is the point of this chapter — you are asked to
**trigger** two of these errors and read what they say.

## step: spacey
The playground has a directory whose name contains a space:
`old logs`. In bash this is where the quoting dance starts.

Bind that name to `d`.
---
verify: var d type=^string$ value=^old\slogs$
hint: An ordinary Go string: `d := "old logs"`
solution: d := "old logs"

## step: interp-space
Now splice it into a command. A `string` in `{...}` is **one argument**
however many spaces it holds — grsh never word-splits an interpolation,
and neither does it split `$VAR`. There is no `"$d"` to remember, and no
bug waiting for the first path with a space in it.

List what is inside that directory.
---
verify: used-construct \{d\}
verify: output-regexp legacy\.log
hint: `ls {d}` — no quotes needed.
solution: ls {d}

## step: bare-assign
grsh has exactly two variable models: Go (`x := ...`) and the process
environment (`export K=V`). A third, shell-local namespace would blur
the line this language exists to draw, so a **bare** `FOO=bar` is an
error — with a hint naming both alternatives.

(`FOO=bar cmd args` still works: that sets `FOO` for that one command,
so `GOOS=linux go build` is fine.)

Trigger it: type a bare assignment, and read the hint.
---
verify: used-construct ^[A-Za-z_][A-Za-z0-9_]*=
verify: status nonzero
hint: Anything of the form `NAME=value`, alone on the line.
hint: `FOO=bar`
solution: FOO=bar

## step: param-expand
`${VAR}` accepts a plain name and nothing else. Parameter expansion
operators — `${VAR:-default}`, `${VAR%.txt}`, `${#VAR}` — are rejected
**at parse time**, because bash would compute them and silently
expanding to empty is the worst of the three outcomes.

Trigger that one too.
---
verify: used-construct \$\{[A-Za-z_]+:-
verify: status nonzero
hint: Try a default-value expansion on any variable.
hint: `echo ${HOME:-/tmp}`
solution: echo ${HOME:-/tmp}

## step: iff-default
The rejection tells you the replacement, and it is the `iff` from
chapter 3: a real conditional, evaluated lazily, in a language that
has one.

`NOPE` is not set. Print `fallback` for it — using `env` and `iff`.
---
verify: used-construct iff\(
verify: used-construct env\(
verify: output-exact fallback
hint: `env("NOPE")` is the empty string when unset.
hint: `fmt.Println(iff(env("NOPE") == "", "fallback", env("NOPE")))`
solution: fmt.Println(iff(env("NOPE") == "", "fallback", env("NOPE")))
