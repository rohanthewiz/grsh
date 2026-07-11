# Embedding grsh

The root `grsh` package is the module's only public API — everything
under `internal/` can change without notice. It exists so another Go
program (an editor's terminal panel, a bot, a task runner) can host a
persistent grsh session.

```go
import "github.com/rohanthewiz/grsh"

sess := grsh.NewSession(grsh.Options{Stdout: out, Stderr: out})
err := sess.Eval(`ls | wc -l`)
```

## The host loop

`Eval` runs one complete input unit and blocks until it finishes, so a
UI calls it from a worker goroutine. `NeedsMore` drives the
continuation prompt exactly like the standalone REPL does:

```go
buf = append(buf, line)
src := strings.Join(buf, "\n")
if sess.NeedsMore(src) {
    showContinuationPrompt()
    return
}
buf = buf[:0]
go func() {
    err := sess.Eval(src)
    // post a "done" event back to the UI loop, then:
    if code, ok := grsh.ExitCode(err); ok {
        closePanel(code)
    } else if err != nil {
        show(grsh.UserMessage(err))
    }
}()
```

Session state — Go variables, aliases, the classifier's scope,
background jobs — persists across `Eval` calls. `Idents` feeds tab
completion, `Notifications` yields "[1] Done cmd &" lines to print
before the next prompt, and `LastStatus`/`Cwd` decorate it.

## The embedding contract

**Streaming.** `Stdout`/`Stderr` receive child output as it is
produced, not when the command exits. The writers may be called from
os/exec copier goroutines — make them goroutine-safe (a mutex, or a
wrapper that posts events to your UI loop).

**Cancellation.** Foreground pipelines run in their own process group
(never the host's), so `Interrupt` (SIGINT) and `Kill` (SIGKILL) are
your stop button: safe to call from any goroutine while `Eval` is
blocked, and they cannot signal the host process. Both return false
when nothing signalable is running — builtins and pure-Go evaluation
(an interpreted `for {}`) are not currently interruptible.

**No terminal claims.** An embedded session never calls `tcsetpgrp`,
and children — including `$()` substitutions — read EOF instead of the
process stdin unless `Options.Stdin` is set. A raw-mode tty owned by
the host (tcell, bubbletea) stays untouched. The flip side: full-screen
interactive programs cannot run inside an embedded session; that
requires a PTY, which is out of scope by design.

**Process-global state.** `cd` and `export` mutate the host process's
working directory and environment — grsh's deliberate design (see
`internal/shellexec`). Hosts should use absolute paths for their own
file operations and treat the cwd as shared with the session. Run one
session per process unless they may share this state.

**Serialized Evals.** `Eval` holds an internal mutex; a second call
blocks until the first finishes. The read methods reflect state as of
the last completed `Eval` — only `Interrupt`/`Kill` are intended for
use mid-Eval.

**Panics stay inside.** `Eval` recovers interpreter panics and returns
them as errors (with the stack attached as the serr attribute `stack`),
so an interpreter bug can't take the host down.

**Background job output.** A `&` job whose stdout/stderr is an
in-process writer has that output discarded (`jobSafeWriter`) — the job
runs on its own goroutine and cannot safely share the host's buffer.
Use explicit file redirection (`cmd > log &`) to keep it.
