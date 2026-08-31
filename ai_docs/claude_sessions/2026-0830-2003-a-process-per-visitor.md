# Session: A Process Per Visitor

Session: a32cbfbd-b796-49ea-8e00-32fdb2f322bb
Date: 2026-08-30

The last three items on Phase 5's open list. Two were the ones that had
been left because they were architecture, not work; the small one first,
because it was small.

## The URL went out before the listener existed

`run` built `"http://" + *addr` and printed it, then called `serve`. With
`-addr host:0` the port in `*addr` is 0, so the port the kernel actually
chose never reached anyone. Nobody passes port 0 on purpose — but the
tests do, and one of them had grown an assertion on `http://127.0.0.1:0`,
which is not a place.

The announcement now waits for the listener. That is a goroutine, because
`serve` blocks until a signal, and `run` joins it before the shutdown
notice so nothing prints after "closing playgrounds" and a test reading
stdout is reading a finished buffer.

The fan-out is worth knowing: rweb **sends** one value on `ReadyChan`
(non-blocking) rather than closing it, so exactly one receiver can have
it. One goroutine reads it and closes `started`, and that is what both the
announcement and the `serve` hook wait on.

Two bugs for the price of one — the browser was also being launched at an
address that might not yet be accepting connections. And a server that
never listens now prints no URL at all, only the error.

`displayURL` also turns `-addr :7654` into `127.0.0.1` (every interface is
not a URL) and uses `JoinHostPort`, so an IPv6 literal keeps its brackets.

## One process, one working directory — closed properly

The remaining half was the serialization, and the note said what it would
cost: a process each. It does.

Each visitor now gets a worker — this same binary re-executed with
`GRSH_TOUR_WORKER=1`. `engine` is the interface a visitor drives;
`localEngine` is the old in-process driver, kept and still correct, and
`remoteEngine` is a driver in a process of its own.

Three inherited descriptors, not stdin/stdout:

    fd 3  requests   parent → child, one JSON value each
    fd 4  frames     child → parent, out/reply/save interleaved
    fd 5  control    parent → child, interrupts, out of band

Separate fds because the child is a whole shell, and anything it or a
dependency prints to fd 1 would otherwise land in the middle of a JSON
stream with no way to resynchronise. Control is a third fd because it must
not queue: interrupt's entire reason to exist is the moment the request
pipe is not being read.

The ordering guarantee is the load-bearing part. One goroutine reads the
frame stream and writes transcript bytes to the sink *synchronously*
before delivering the reply behind them, so a Submit's caller cannot see
the View describing a finished command until every byte that command
printed is already in the transcript. The page depends on that to
re-enable its input at the right moment.

Progress travels rather than being shared: the database is a file one
process holds open, so the worker gets a snapshot of the student's records
at startup and posts its saves back as fire-and-forget frames. Two tabs no
longer see each other's saves land live — a fair price locally, and
arguably better, since two tabs on two chapters no longer overwrite each
other's place.

Measured on a live server, two tabs:

| | worker each | `-in-process` |
|---|---|---|
| B's command while A sleeps 8s | 44 ms | 4418 ms |
| shutdown while A still sleeps | 19 ms | — |

### Three real bugs fell out of building it

**The protocol fds leaked into the student's own commands.**
`exec.Cmd.ExtraFiles` deliberately does not set close-on-exec — the fds
have to survive the exec that creates the worker. But a worker's whole job
is to fork the student's commands, and every one of them inherited them
too. So `sleep 300` held the write end of the frame pipe, and the parent —
which learns its worker died by reading EOF from that pipe — learned
nothing until the student's command finished. `syscall.CloseOnExec` on all
three, in the child, because the parent's ends are different descriptors.
This alone took shutdown from 10.003s to 102ms.

**Teardown waited on the visitor's lock**, which is held for the whole of
a running command — five minutes of `sleep 300` on every Ctrl+C, every
reaper pass, and every Reset. `visitor.stop` now kills first, off the
lock; `engine.Close` carries a documented requirement to be safe from
another goroutine, which `localEngine` meets with a lock of its own and
`remoteEngine` meets by construction. `Server.Close` closes visitors
concurrently.

And one kill is not enough: `echo hi; sleep 30` is two pipelines, and a
signal arriving in the gap finds nothing to signal, reports so honestly,
and leaves the sleep to start a millisecond later. `remoteEngine.Close`
retries every 100ms until the worker answers.

**Embedded chapters handed children `os.Stdin`.** `newChapter` never set
`runner.Options.Stdin`, so every child got the process's. In the web tour
that means a student typing a bare `cat` in a browser reads the keyboard
of whoever started the server; in a worker it would read the wire the
process is driven over. `session.go` had stated the rule all along —
"embedded children must never read the host's stdin" — and the tutor's own
chapters were the one place that had not adopted it.

## A test that runs a browser

Two of this feature's four bugs were invisible to Go. There is now a test
that would have seen them.

It drives real headless Chrome over `--remote-debugging-pipe`: CDP as
NUL-terminated JSON on two inherited descriptors, which needs no WebSocket
implementation, so the whole client is sixty lines and **go.mod does not
grow**. That list is a stated property of this project; a test dependency
would still be in it.

Five phases, two seconds: cold start over a real EventSource, typing
through the page's own keydown handler, Ctrl+C dispatched at the
*document* while the input is disabled, reload and replay, and a 300KB
flood followed by a reload. Skips loudly with no browser
(`GRSH_TOUR_CHROME` overrides the search).

Checked that it bites, rather than assuming:

| reintroduced bug | result |
|---|---|
| Ctrl+C bound to the input | FAIL — ctrl+C phase |
| trim's line search unbounded | FAIL — flood phase |
| `Content-Encoding: text/plain` on SSE | **passes** |

The third is the honest part and it is written into the file. Today's
Chrome renders the stream perfectly well with the bad header on it, so a
browser does not guard that regression — `TestTourSendsADecodableStream`
does, and it still fails when the header comes back. A browser is evidence
about one browser, on one day; it is not a substitute for pinning the
wire.

## Verification

Every bite check above was run, not reasoned about. The `cat` test was
confirmed by removing the fix and watching it hang for 15 seconds.

```
gofmt -l .
go build ./... && go vet ./... && go test ./... -count=1
go test -race ./internal/tour ./internal/tutor ./cmd/grsh-tour -count=1
go list -deps ./cmd/grsh | grep -E 'rweb|element'   # still empty
```

By hand, against a live server: two tabs produce two worker processes;
the second student answers in 44ms while the first sleeps eight seconds;
SIGTERM mid-sleep tears down in 19ms with no playground and no stray
worker left behind; `-in-process` produces zero children and 4.4 seconds,
which is the eval gate doing exactly what it is documented to do.

## Files

```
cmd/grsh-tour/main.go          announcement waits for the listener; displayURL;
                               worker dispatch before flags; -in-process
cmd/grsh-tour/main_test.go     kernel-chosen port; no URL without a listener;
                               displayURL table
internal/tour/worker.go        new — the wire, the child, RunWorkerProcess,
                               CloseOnExec, proxyStore
internal/tour/remote.go        new — engine interface, localEngine, remoteEngine
internal/tour/visitor.go       engine instead of *tutor.Driver; stop() off the lock
internal/tour/tour.go          Options.InProcess; concurrent Close; package doc
internal/tour/main_test.go     new — TestMain makes the test binary a worker
internal/tour/worker_test.go   new — isolation, shutdown, dead worker, progress
internal/tour/browser_test.go  new — headless Chrome over a CDP pipe
internal/tutor/tutor.go        embedded chapters get an EOF stdin
internal/tutor/drive.go        SnapshotProgress; evalGate comment points at the fix
internal/tutor/drive_test.go   `cat` must not read the host's stdin
```

## Open

- **A worker's background jobs still outlive it.** `sleep 300 &` gets its
  own process group, so neither the close nor the kill reaches it. The
  playground goes; the job does not. Same as before workers, but workers
  make it fixable — the parent knows the child's pgid.
- **The in-process path's shutdown still waits.** `localEngine.Close`
  takes its own lock, and `tutor.Driver` is not safe for concurrent use,
  so a teardown there genuinely waits for the command in progress. It is
  the documented cost of `-in-process`, not an oversight, but it is a
  difference between the two paths that nothing warns about at runtime.
- **The browser test knows one browser.** It skips where Chrome is not
  installed, which is most CI images, and the Content-Encoding case above
  shows that a passing browser test is weaker evidence than it looks.
