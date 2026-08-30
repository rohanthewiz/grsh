# Jobs

Background jobs, the job table, and signals. Two of grsh's choices here
differ from bash on purpose, and the prose says which.

## step: background
A trailing `&` runs the whole and-or chain as a job — `build && notify
done &` is one job, not two. Expansion happens **eagerly, at launch**:
there is no subshell, so `$VAR`, `{expr}`, aliases and redirect targets
are resolved before the job leaves. (For the same reason, builtins
cannot be backgrounded.)

Start a long-running `sleep 30` in the background.
---
verify: used-construct &\s*$
hint: The same trailing `&` you already use.
hint: `sleep 30 &`
solution: sleep 30 &

## step: jobs-list
Jobs run in their **own process group**, so Ctrl+C at the prompt never
reaches them, and their stdin is `/dev/null` — a background job can
never steal your typing.

List the job table.
---
verify: output-regexp sleep 30
hint: One word.
hint: `jobs`
solution: jobs

## step: kill-job
`kill %N` signals the job's whole process group (TERM by default;
`-KILL` and `-9` work too). Job specs are `%N`, `%%` or `%+` — the
last two mean "the newest job", which is what you want when you have
one.

There is no `$!`: use `wait %N` and `status()` instead.

Kill the job you started, without naming its number.
---
verify: used-construct ^kill %
hint: `%%` is the newest job.
hint: `kill %%`
solution: kill %%

## step: jobs-gone
Finished jobs are reported once and then removed from the table, so
listing twice does not repeat itself. At an interactive prompt they
are also announced as they finish.

List the jobs again and watch the outcome be collected.
---
verify: any-input
hint: `jobs`
solution: jobs

## step: wait-demo
`wait` with no arguments collects every job and reports status 0; with
specs it blocks on those and takes the last one's status.

Two deliberate differences from bash, both about not lying to you:

  · **wait skips stopped jobs** with a warning instead of blocking —
    nothing could ever resume them while the shell sits waiting.
  · Ctrl+Z at the prompt really suspends a foreground command into the
    table (`fg %N` brings it back, `bg %N` resumes it in the
    background). Try that after the tutor: run `sleep 60`, press
    Ctrl+Z, then `bg %1`, then `fg %1`.

Nothing is left running. Collect anyway, and move on.
---
verify: any-input
hint: `wait`
solution: wait
