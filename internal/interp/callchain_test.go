package interp

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rohanthewiz/serr"
)

// ---- the script-level call chain ----
//
// A failing call records the path that reached it in the error's
// "in_func" field, which the --debug output shows. It used to be built by
// wrapping the error again at every frame on the way out; serr copies the
// whole accumulated field list on each wrap, so that was quadratic in
// call depth. It is now rendered once at the innermost failing frame and
// attached once at the outermost.
//
// These cover both halves: that the chain still says what it said, and
// that producing it costs what it should.

// callChainOf runs a script expected to fail and returns its rendered
// call chain ("" when the error carries none).
func callChainOf(t *testing.T, body string) string {
	t.Helper()
	_, out, err := evalKeep(t, body, nil)
	if err == nil {
		t.Fatalf("expected the script to fail\noutput: %q\nsource:\n%s", out, body)
	}
	v, ok := serr.WrapAsSErr(err).GetAttribute("in_func")
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("in_func = %v (%T), want a string", v, v)
	}
	return s
}

// The chain reads outermost first, as a call path.
func TestCallChainRecordsThePathToTheError(t *testing.T) {
	got := callChainOf(t, `inner := func() { fmt.Println(1 / 0) }
mid := func() { inner() }
outer := func() { mid() }
outer()`)
	if got != "outer > mid > inner" {
		t.Errorf("in_func = %q, want %q", got, "outer > mid > inner")
	}
}

// An anonymous closure has no name to contribute, so it reports the same
// placeholder the arity errors use.
func TestCallChainNamesAnonymousFrames(t *testing.T) {
	got := callChainOf(t, `f := func() { func() { fmt.Println(1 / 0) }() }
f()`)
	if got != "f > function" {
		t.Errorf("in_func = %q, want %q", got, "f > function")
	}
}

// An error raised outside any closure has no chain -- there is no call
// path to report, and an empty field would be noise.
func TestCallChainAbsentAtTopLevel(t *testing.T) {
	if got := callChainOf(t, `fmt.Println(1 / 0)`); got != "" {
		t.Errorf("in_func = %q, want no chain at all", got)
	}
}

// Recursion is the case that makes a chain unreadable and also the case
// that compresses perfectly: consecutive repeats collapse to a count.
func TestCallChainCollapsesRepeatedFrames(t *testing.T) {
	got := callChainOf(t, `f := func(n int) int {
	if n == 0 {
		return 1 / 0
	}
	return f(n - 1)
}
fmt.Println(f(5))`)
	if got != "f x6" {
		t.Errorf("in_func = %q, want %q (six frames, one entry)", got, "f x6")
	}
}

// A runaway is the extreme of the same shape: ten thousand frames render
// as one entry rather than ten thousand fields.
func TestCallChainCollapsesARunaway(t *testing.T) {
	got := callChainOf(t, `f := func(n int) int { return f(n + 1) }
fmt.Println(f(0))`)
	if got != fmt.Sprintf("f x%d", maxCallDepth) {
		t.Errorf("in_func = %q, want f x%d", got, maxCallDepth)
	}
}

// Mutual recursion alternates, so nothing collapses -- which is why the
// rendered chain is capped as well. Both ends survive; the middle is
// counted.
func TestCallChainCapsLongPaths(t *testing.T) {
	got := callChainOf(t, `even := func(n int) int {
	if n == 0 {
		return 1 / 0
	}
	return odd(n - 1)
}
odd := func(n int) int { return even(n - 1) }
fmt.Println(even(60))`)
	entries := strings.Split(got, " > ")
	// chainMaxEntries survivors plus the one elision marker.
	if len(entries) != chainMaxEntries+1 {
		t.Errorf("chain has %d entries, want %d:\n%s", len(entries), chainMaxEntries+1, got)
	}
	if !strings.Contains(got, "... 29 more ...") {
		t.Errorf("missing the elision marker:\n%s", got)
	}
	// The ends are the informative part, and both are kept.
	if !strings.HasPrefix(got, "even > odd > ") || !strings.HasSuffix(got, "> even") {
		t.Errorf("the ends of the path were not preserved:\n%s", got)
	}
}

// The chain is attached exactly once. Under the old scheme every frame
// wrapped again, so a six-deep recursion left six in_func fields on one
// error (and a runaway left ten thousand).
func TestCallChainIsAttachedOnce(t *testing.T) {
	_, _, err := evalKeep(t, `f := func(n int) int {
	if n == 0 {
		return 1 / 0
	}
	return f(n - 1)
}
fmt.Println(f(5))`, nil)
	if err == nil {
		t.Fatal("expected the script to fail")
	}
	n := 0
	for _, fld := range serr.WrapAsSErr(err).Fields() {
		if fld == "in_func" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the error carries %d in_func fields, want exactly 1", n)
	}
}

// A deferred call that fails while the body has ALSO failed has its error
// dropped -- and must not leave its own frame in the surviving error's
// chain. Without the guard in popFrame this reads "f > boom", naming a
// function that has nothing to do with the error being reported.
func TestCallChainIgnoresADroppedDeferError(t *testing.T) {
	got := callChainOf(t, `boom := func() { fmt.Println(1 / 0) }
f := func() {
	defer boom()
	fmt.Println(2 / 0)
}
f()`)
	if got != "f" {
		t.Errorf("in_func = %q, want %q -- the dropped defer's frame leaked in", got, "f")
	}
}

// ...but when the body SUCCEEDS, the deferred call's own failure is the
// one that surfaces, and the chain is its own.
func TestCallChainFollowsASurfacingDeferError(t *testing.T) {
	got := callChainOf(t, `boom := func() { fmt.Println(1 / 0) }
f := func() {
	defer boom()
}
f()`)
	if got != "f > boom" {
		t.Errorf("in_func = %q, want %q", got, "f > boom")
	}
}

// ---- the cost ----

// errUnwindSink keeps the failed run's error live so nothing in the
// measured region is optimized away.
var errUnwindSink error

// TestErrorUnwindIsLinearInDepth is the guard for the defect itself.
//
// Wrapping at every frame made an error raised N calls deep do O(N^2)
// field copying on the way out: at the 10,000-frame limit, over two
// seconds to produce a forty-character message, against ~14ms to descend
// those same frames. The metric is BYTES rather than time, for the same
// reason TestJoinShellHeredocLinear uses bytes -- a wall-clock ratio on a
// few milliseconds of work flakes, and the defect lives in copying
// anyway.
//
// The descent is itself linear in depth, so a healthy ratio tracks the
// depth ratio (8x here) rather than being flat. Quadratic behavior lands
// near 64x, so the bound sits well between the two.
//
// Verified non-vacuous by restoring the per-frame serr.Wrap: 57.4x,
// against 8.3x now. The absolute figures are the more startling half --
// five failing runs at depth 4000 allocated 7.9GB before the fix and
// 11.5MB after, so a single error four thousand calls deep was building
// something like 1.6GB of copied field slices on its way out.
func TestErrorUnwindIsLinearInDepth(t *testing.T) {
	const (
		shallow  = 500
		deep     = 4000
		maxRatio = 16.0
	)

	measure := func(depth int) uint64 {
		body := fmt.Sprintf(`f := func(n int) int {
	if n == 0 {
		return 1 / 0
	}
	return f(n - 1)
}
fmt.Println(f(%d))`, depth)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < 5; i++ {
			_, _, err := evalKeep(t, body, nil)
			if err == nil {
				t.Fatalf("depth %d: expected the script to fail", depth)
			}
			errUnwindSink = err
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	lo := measure(shallow)
	hi := measure(deep)
	if lo == 0 {
		t.Fatal("measured no allocation at all; the guard is not measuring anything")
	}
	ratio := float64(hi) / float64(lo)
	if ratio > maxRatio {
		t.Errorf("allocation grew %.1fx for an %.0fx increase in call depth "+
			"(%d bytes -> %d bytes) -- the unwind is superlinear again",
			ratio, float64(deep)/shallow, lo, hi)
	}
}
