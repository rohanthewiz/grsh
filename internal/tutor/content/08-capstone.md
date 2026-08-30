# Capstone: session to script

The thesis of the language, in five steps: what you just typed at the
prompt **is** a script, so a working session can be saved and re-run
without translating anything.

Build a small report, save the session, and then run the saved script
back through this very shell.

## step: count-errors
Start the report. Count the failing requests — capture the matching
lines and count them in Go this time, so the number is an `int` from
the start.
---
verify: used-construct \$\(
verify: var errs type=^int$ value=^17$
hint: `lines(...)` splits captured output; `len` counts.
hint: `errs := len(lines($(grep 500 access.log)))`
solution: errs := len(lines($(grep 500 access.log)))

## step: count-total
Now the denominator: every request in the log.
---
verify: var total type=^int$ value=^120$
hint: Read the file and count its lines, all in Go.
hint: `total := len(lines(readFile("access.log")))`
solution: total := len(lines(readFile("access.log")))

## step: report
Print the report line, exactly:

  17 of 120 requests failed
---
verify: used-construct errs
verify: used-construct total
verify: output-exact 17 of 120 requests failed
hint: `fmt.Printf` takes Go's own verbs; remember the `\n`.
hint: `fmt.Printf("%d of %d requests failed\n", errs, total)`
solution: fmt.Printf("%d of %d requests failed\n", errs, total)

## step: save
`session save [N] file.grsh` writes your input units — whole units, so
a multi-line `for` block is one entry, not five — as a runnable script,
shebang included. With an `N` it takes only the last N.

The last three units are exactly the three lines of the report. Save
them as `report.grsh`.
---
verify: used-construct ^session save
verify: file report.grsh contains=(?m)^#!/usr/bin/env grsh$
verify: file report.grsh contains=requests failed
hint: `session save N file` — you want the last 3.
hint: `session save 3 report.grsh`
solution: session save 3 report.grsh

## step: source
`source file.grsh` (or `. file.grsh`) runs a script **in this session**:
its variables, functions and exports persist afterwards.

So the round trip closes here. Run the script you just saved, and watch
your session play itself back.
---
verify: used-construct report\.grsh
verify: output-regexp (?m)^17 of 120 requests failed$
hint: `source report.grsh`
solution: source report.grsh
