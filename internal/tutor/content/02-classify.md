# Two languages, one prompt

Every line is read as **shell** or as **Go**, by nine rules applied in
order — no mode switch, no subshell, no different language for scripts.
This chapter grades your grasp of those rules directly: the tutor asks
the classifier the same question the prompt just asked.

Run `grsh --explain` outside the tutor to see the verdict for every line
as you type it.

## step: shell-default
Rule 9 is the default: anything that isn't recognisably Go is a
command. That is why your muscle memory survives.

Count the lines in `access.log` — and note that nothing about this
line looks like Go.
---
verify: classified-as shell
verify: output-regexp 120
hint: `wc -l access.log`
solution: wc -l access.log

## step: go-keyword
Rule 5: a line whose first word is a Go keyword is Go. `var`, `if`,
`for`, `func`, `return`, `type` — the usual set.

Declare an `int` named `n` holding 7, using a `var` declaration.
---
verify: classified-as go
verify: var n type=^int$ value=^7$
hint: Go's own syntax, nothing added: `var NAME TYPE = VALUE`.
hint: `var n int = 7`
solution: var n int = 7

## step: walrus
Rule 6: a line containing `:=` outside quotes is Go, wherever the `:=`
sits. This is the rule you will use most.

Bind the string `ada` to `name`.
---
verify: classified-as go
verify: var name type=^string$ value=^ada$
hint: `name := "ada"`
solution: name := "ada"

## step: declared-ident
Rule 7: the classifier **remembers** declarations. `name` is now a
declared identifier, so a line starting with `name` followed by `=`,
`(`, `++`, `,` or a selector `.` is Go.

Assign `grace` to `name` — no `:=` this time, and watch it still be
read as Go.
---
verify: classified-as go
verify: var name value=^grace$
hint: Plain assignment: `name = "grace"`
solution: name = "grace"

## step: pkg-dot
Rule 8: a registered package name followed immediately by `.` is Go.
`fmt`, `strings`, `strconv`, `os`, `filepath`, `time`, `regexp`,
`json`, `sort`, `math` — all pre-loaded, no imports needed.

Print `SHELL` by upper-casing the word `shell` in Go.
---
verify: classified-as go
verify: output-regexp (?m)^SHELL$
hint: `strings.ToUpper` returns the upper-cased string; print it.
hint: `fmt.Println(strings.ToUpper("shell"))`
solution: fmt.Println(strings.ToUpper("shell"))

## step: escape-hatch
Rule 2 is the escape hatch, for the day a Go declaration shadows a
command you wanted: a line whose first word is `sh` is forced to
shell, and the `sh` is stripped before the command runs.

(It does not start `/bin/sh` — for that, `command sh -c '...'`.)

Force a line to shell and print `forced`.
---
verify: classified-as shell
verify: used-construct ^sh\s
verify: output-regexp (?m)^forced$
hint: Put `sh` in front of an ordinary command.
hint: `sh echo forced`
solution: sh echo forced
