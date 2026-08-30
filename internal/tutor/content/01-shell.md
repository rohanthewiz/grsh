# It's just a shell

Chapter 1 exists to build trust before it teaches anything: every habit
you already have works here, unchanged. Nothing in this chapter is
grsh-specific — that is the point.

## step: look-around
Welcome. You are at a real grsh prompt, in a throwaway playground
seeded with files to practise on.

Start by looking around.
---
try: ls
verify: output-regexp access\.log
hint: `ls` — the same one you already use.
solution: ls

## step: grep-count
`access.log` is a web server log. Some of its requests failed with a
500.

Count them.
---
verify: output-regexp (?m)^\s*17\s*$
hint: `grep` counts matching lines with `-c`.
hint: `grep -c 500 access.log`
solution: grep -c 500 access.log

## step: pipe-count
Pipes work exactly as they do in bash.

There are some Go files here. Count them **with a pipe** — list them,
and let another command do the counting.
---
verify: used-construct \|
verify: output-regexp (?m)^\s*3\s*$
hint: `ls *.go` lists them; `wc -l` counts lines.
hint: `ls *.go | wc -l`
solution: ls *.go | wc -l

## step: redirect
Redirection too.

Save just the failing requests into a file called `errs.txt`.
---
verify: used-construct >
verify: file errs.txt contains=500
hint: `>` sends stdout to a file, truncating it.
hint: `grep 500 access.log > errs.txt`
solution: grep 500 access.log > errs.txt

## step: andand
`&&` and `||` short-circuit the way you expect.

Print `found` — but only if `access.log` really exists.
---
verify: used-construct &&
verify: output-regexp (?m)^found$
hint: `test -f FILE` succeeds when the file exists.
hint: `test -f access.log && echo found`
solution: test -f access.log && echo found

## step: quoting
Single quotes are still literal — nothing inside them is expanded, by
anything. That means the awk and sed one-liners you have memorised
paste in and work.

Print the client address from the log's first line.
---
verify: output-exact 10.0.0.1
hint: `awk '{print $1}'` prints the first field of each line.
hint: Pipe it into `head -1`, or awk only the first line.
solution: awk '{print $1}' access.log | head -1
