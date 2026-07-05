# grsh v2 — background jobs, pipefail, full Ctrl+Z job control

Session ID: `5616e8cd-8111-41ba-bf75-030e84071c09` (same session as the
REPL doc — this is part 2)
Date: 2026-07-05
Repo: `~/projs/go/grsh` → https://github.com/rohanthewiz/grsh
Previous docs: `2026-0705-0117-v2-repl-complete.md` (REPL),
`2026-0704-1230-grsh-v1-milestones-1-4.md` (architecture/package map).

## Commits this doc covers

- `7fd91ec` background jobs — & launch, job table, jobs/wait/fg/kill %N
- `7dea03a` pipefail(true)
- `98014f9` job control phase 2 — Ctrl+Z, stopped jobs, fg/bg + tty
  handoff

State: `go test -race ./...` green, gofmt/vet clean. NOT pushed.
Remaining v2: heredocs, struct methods, --explain trace mode.

## Background jobs (`&`) — design

- Parser: lone `&` at CmdList level marks the preceding AndOr
  `Background` and separates like `;` (`sleep 9 & echo hi`). parseCommand
  just breaks on `&` (was an error); `&&`/`&>`/`2>&1` untouched.
- **Eager expansion**: launchJob expands ALL argv/aliases/redirect
  targets synchronously (`resolveRedirs` split out of applyRedirs; the
  fd-only half `applyResolved` is goroutine-safe). The async runner
  (runJob → runPreparedPipeline) touches only the JobTable under lock.
  Deviations vs bash (documented §LANGUAGE.md): no lazy subshell,
  builtins can't background, `$!` doesn't exist (wait %N + status()).
- Jobs run in their own pgroup (leader Setpgid, followers join), stdin
  /dev/null. Job output: only *os.File writers pass through
  (jobSafeWriter); buffers → /dev/null (concurrent-write safety; capture
  contexts discard bg output — redirect to a file to keep it).
- JobTable: pgidSet chan closed on first start (Signal waits on it — no
  kill-vs-launch race); Wait/Reap; Notifications drains Done (reaps) and
  newly-Stopped (stays). `wait` REAPS collected jobs (bash) — this fix
  was caught by eyeballing the golden: stale job 1 satisfied `wait %1`.
- kill %N is a builtin ONLY when an arg has `%` (hasJobSpec in
  runSimple); plain-pid kill stays external. -SIG names + numbers.
- Signaled procs report 128+signal (was Go's -1; prompt showed [-1]).
- Pre-existing race fixed: pipeline cmds sharing buffer-backed stderr →
  one syncWriter per distinct writer, single mutex, identity preserved
  so exec's Stdout==Stderr fd dedup still works.

## pipefail

`pipefail(true)` builtin (mirrors errexit) → State.PipeFail;
pipelineStatus(statuses, pipefail) = rightmost nonzero. Captured at
launch for bg jobs (test flips it after launch). Golden: pipefail.grsh.

## Job control phase 2 (the hairy one)

- State.Interactive (set by repl.Run via Session.SetInteractive). Gate:
  interactiveTTY(st, stdio) = Interactive && stdio.In is *os.File tty.
  Scripts/captures never enter jc paths.
- Foreground path (shellexec/jobcontrol.go runForegroundJobControl):
  leader gets SysProcAttr{Setpgid, Foreground: true, Ctty: tty fd} — Go
  does tcsetpgrp in the forked child pre-exec; followers join the pgroup.
  Shell waits syscall.Wait4(-pgid, WUNTRACED). On ws.Stopped():
  AdoptStopped(remaining pids in pipeline order, per-position statuses,
  pipefail) → "[N]  Stopped  cmd" to stderr → return 128+StopSignal.
  defer tcSetpgrp(tty, shellPgid) restores the terminal.
- Adopted jobs: waited by whoever resumes. ResumeForeground = tty
  handoff + SIGCONT + waitAdopted (re-suspendable). ResumeBackground =
  CONT + watcher goroutine; stop-again (SIGTTIN) re-marks Stopped for
  REPL announcement. fg on a RUNNING adopted job: StopForFg (SIGSTOP +
  poll) then ResumeForeground. fg on running &-job: plain Wait (stdin is
  /dev/null; nothing to hand it).
- kill on stopped adopted job (any sig but SIGSTOP): after delivering,
  ResumeBackground so CONT flows and a collector reaps the outcome.
- **wait skips Stopped jobs with a warning** (bash blocks): a blocked
  shell could never resume them and the REPL absorbs ^C → deadlock.
  Documented deviation.

## THE two signal gotchas (hard-won, remember these)

1. **Never signal.Ignore(SIGTSTP/TTOU/TTIN) process-wide in a shell.**
   SIG_IGN survives exec — children inherited ignored SIGTSTP, so ^Z
   echoed but nothing stopped (verified: `/bin/sh -c 'kill -TSTP $$;
   echo NOT-STOPPED'` printed NOT-STOPPED). bash resets dispositions in
   its fork; Go cannot. Fix: SIGTTOU ignored only inside tcSetpgrp
   (Ignore → ioctl → Reset). grsh needs no TSTP protection: readline is
   raw at the prompt (^Z filtered), and a running job owns the terminal.
2. **chzyer/readline ^Z at the prompt SIGTSTPs the PARENT on macOS**
   (SuspendMe in utils_unix.go) — would suspend the user's outer shell.
   Disabled via Config.FuncFilterInputRune dropping CharCtrlZ.

## Debug method that cracked it

pty harness: `{ paced printf; } | script -q /dev/null ./grsh`, with
`\032` for ^Z (input must be PACED — readline drops bytes buffered
before its first read). When ^Z didn't work: (a) ps -o
pid,pgid,tpgid,stat from outside proved the fg handoff was correct
(tpgid == sleep's pgid), (b) same harness against `/bin/bash --norc -i`
proved the harness delivers SIGTSTP, (c) the sh -c kill-TSTP probe
isolated the inherited-SIG_IGN cause. Baseline-against-bash is the move.

## Tests added

- shellparse: & forms (trailing, separator, chain, after redirect;
  errors `& ls`, `ls && &`).
- shellexec/jobs_test.go: bg+wait via files, chain short-circuit in bg,
  wait %N status, jobs list/reap, fg status, notifications drain,
  builtin-in-bg rejected, kill %1 → wait = 143.
- shellexec/pipefail_test.go + jobcontrol_test.go (SIGSTOP a Setpgid
  child = synthetic ^Z; bg resume, fg resume sans tty, jobs Stopped,
  kill-stopped collected, wait-skips-stopped deadlock guard, bg-on-
  running refused).
- repl: notification announced before prompt (delay step in fakeReader;
  explicitly waited jobs are reaped silently — bash behavior).
- Goldens: background_jobs.grsh, pipefail.grsh (regenerate with
  -update, ALWAYS eyeball — caught the wait-reap bug this session).
- Live pty verification of everything incl. ^Z → bg → kill → wait →
  143 and ^Z → fg → ^Z → fg → done.

## Gotchas / notes for future work

- exec.Cmd.Wait has no WUNTRACED — jc paths use raw syscall.Wait4 and
  never call c.Wait (fine: tty stdio = no exec copy goroutines).
- bg (&) jobs stopped externally (kill -STOP) still show Running — their
  goroutine's exec.Wait can't see stops. Corner case, undocumented.
- `script`-based pty tests are manual verification (Bash tool), not Go
  tests; unit tests fake ^Z with SIGSTOP on a Setpgid child.
- macOS SIGTSTP=18 → suspend status 146 (linux 148); tests use
  128+int(sig), never hardcode.
- Job IDs reset to 1 when the table drains (bash-like).

## v2 roadmap remaining

1. Heredocs (`<<EOF`, `<<-EOF`) — shellparse + classify NeedsMore must
   treat heredoc bodies as raw continuation lines.
2. Struct methods (interp).
3. Wider registry (net/http?, bufio scanning).
4. --explain trace mode; bash-porting error hints.
