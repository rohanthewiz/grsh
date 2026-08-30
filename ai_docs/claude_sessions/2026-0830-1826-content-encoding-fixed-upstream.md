# Session: Content-Encoding, Fixed Where It Belonged

Session: d0acc2bd-3ce0-4915-b588-b2fc0da865b2
Date: 2026-08-30

The last open item from Phase 5 that was somebody else's bug: the tour
overrode rweb's SSE `Content-Encoding` header instead of rweb not sending
it. Two repos, one fix.

## The bug, stated correctly

`SetSSEHeaders` sent `Content-Encoding: text/plain`. That header names a
content *coding* — gzip, br, zstd — applied to the body. `text/plain` is a
media type. Browsers and curl discard a body whose coding they cannot
decode, so an event stream connected, delivered its headers, and then
delivered nothing.

The reason it survived: Go's http client only ever auto-decodes gzip and
passes every other `Content-Encoding` through untouched. rweb's whole SSE
test suite is written in Go, so nothing in it could see the problem. This
is a class of bug worth naming — a header that only the *other* clients
act on is invisible to a test suite that speaks one client.

## The fix is a deletion, not a correction

The tour's workaround wrote `identity`, which is what rweb meant. Upstream
it is still wrong: RFC 9110 §8.4 defines no content-coding token for "not
encoded" — `identity` exists only as an `Accept-Encoding` value. rweb
compresses nothing, so the correct signal is no header at all.

So `SetSSEHeaders` lost the line, and gained a doc comment saying why the
absence is deliberate. A header that is absent on purpose looks exactly
like a header someone forgot; the comment is the only thing separating
them, and without it this comes back.

## The test asserts a header, which is the point

`TestSSEHeadersCarryNoContentEncoding` opens a stream and reads
`resp.Header.Get("Content-Encoding")`. It never reads an event, because no
event can carry the signal: Go leaves the header alone, so the stream looks
identical either way.

Checked the test bites rather than assuming it: reinstating the old line
fails it with `text/plain` vs `""`. A test written for a bug nobody can
reproduce in the language the tests are in is worth verifying by hand.

## Release

`v0.1.26` was fifteen commits behind master, so `v0.1.28` also ships the
TLS `Config` hook, the `critical` and `stylus` middlewares, and the
request-path fixes. Flagged before tagging rather than after. rweb tags
even numbers, lightweight, on the commit — followed.

## The grsh side

`events` is a bare `return ctx.SetSSE(...)` again. What did *not* happen is
the obvious cleanup: `TestTourSendsADecodableStream` stays, flipped to
assert the header is absent.

Deleting it would have been the natural move — the bug is upstream, the
override is gone, the test guards nothing we control. That is exactly
backwards. A dependency bump is precisely how this returns, and the tour is
the only place in either repo where a browser is the client. The assertion
costs one line and is the sole thing standing between a future rweb release
and a tour that silently stops streaming.

Verified before the bump, not after: with the override removed and a local
`replace` pointing at the fixed rweb, the stream carried no
`Content-Encoding` at all. `go mod tidy` after `go get`, since `go get`
leaves the superseded `go.sum` lines behind.

## Verification

```
# rweb
gofmt -l Response.go sse_test.go   # clean
go build ./... && go vet ./... && go test ./... -count=1

# grsh
gofmt -l .
go build ./... && go vet ./... && go test ./... -count=1
go test -race ./internal/tour -count=1
go list -deps ./cmd/grsh | grep -E 'rweb|element'   # still empty
```

## Files

```
rweb Response.go                 SetSSEHeaders: header dropped, comment added
rweb sse_test.go                 TestSSEHeadersCarryNoContentEncoding
rweb architecture/sse/sse.md     header table row → a paragraph of reasoning
rweb                             191f814, tagged v0.1.28, pushed
internal/tour/tour.go            events(): bare SetSSE
internal/tour/tour_test.go       asserts the header is absent
go.mod / go.sum                  rweb v0.1.26 → v0.1.28
ai_docs/plans/…-plan.md          Phase 5 bug list notes the upstream fix
```

## Open

Phase 5's other open items are untouched and still stand: one process /
one working directory, `cmd/grsh-tour` has no test of its own, no keyboard
interrupt from the page, the transcript bounded at 256KB. This session
closed exactly one of the five.
