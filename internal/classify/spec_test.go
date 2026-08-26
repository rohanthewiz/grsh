package classify

import (
	"reflect"
	"testing"
)

// Tests for the speculative classify cache.
//
// The cache is the kind of optimization that is invisible when it works and
// silently wrong when it does not: a stale entry does not crash, it just
// paints the buffer with the previous frame's classification. So these
// tests are all about invalidation, and they check the memo POINTER as well
// as the answers — identical output proves nothing when the input is
// identical too, and the pointer is what says whether work was skipped.

func newSpecClassifier() *Classifier {
	c := New([]string{"fmt", "strings"})
	c.Predeclare("len", "iff")
	return c
}

// TestSpeculateReuse: repeated derivations of the same buffer classify once.
// This is the case the editor is in on every frame.
func TestSpeculateReuse(t *testing.T) {
	c := newSpecClassifier()
	const src = "x := 1\nfmt.Println(x)"

	c.Preview(src)
	first := c.spec
	if first == nil {
		t.Fatal("Preview did not populate the cache")
	}
	c.Preview(src) // the hinter's shellLineAt
	c.Pending(src) // the hinter's breadcrumb
	if c.spec != first {
		t.Error("same source reclassified: the frame is doing 3 passes, not 1")
	}

	// A different buffer must evict — the user typed another character.
	c.Preview(src + "\n")
	if c.spec == first {
		t.Error("cache served a different source")
	}
}

// TestSpeculateAgreesWithFreshClassify pins that the cached path and a
// straight Clone+File produce the same thing, for both entry points.
func TestSpeculateAgreesWithFreshClassify(t *testing.T) {
	srcs := []string{
		"echo hi",
		"x := 1",
		"func f() {",
		"cat <<EOF\nbody",
		"xs := []int{\n1,",
		"ls | grep x &&",
		"",
	}
	for _, src := range srcs {
		t.Run(src, func(t *testing.T) {
			fresh := newSpecClassifier()
			wantChunks, wantErr := fresh.Clone().File(src)
			wantPending := freshPending(t, src)

			c := newSpecClassifier()
			if got := c.Preview(src); !reflect.DeepEqual(got, wantChunks) {
				t.Errorf("Preview: got %+v, fresh %+v", got, wantChunks)
			}
			if got := c.Pending(src); !reflect.DeepEqual(got, wantPending) {
				t.Errorf("Pending: got %+v, fresh %+v", got, wantPending)
			}
			_ = wantErr
		})
	}
}

// freshPending computes Pending the pre-cache way: one throwaway clone,
// nothing remembered.
func freshPending(t *testing.T, src string) PendingInfo {
	t.Helper()
	c := newSpecClassifier()
	return c.Pending(src) // first call on a fresh classifier is always a miss
}

// TestSpeculateInvalidatedByFile is the core staleness guard. File advances
// scope and depth, so anything speculated against the old state is wrong.
func TestSpeculateInvalidatedByFile(t *testing.T) {
	c := newSpecClassifier()
	const probe = "count.Field"

	// Undeclared: `count.Field` is just a command with a dot in its name.
	if got := c.Preview(probe)[0].Kind; got != Shell {
		t.Fatalf("before declaring count: got %v, want shell", got)
	}
	if _, err := c.File("count := 1"); err != nil {
		t.Fatal(err)
	}
	// Declared: rule 6a now claims it for Go. A stale cache would still
	// say shell, and the highlighter would paint it as a missing command.
	if got := c.Preview(probe)[0].Kind; got != Go {
		t.Errorf("after declaring count: got %v, want go (stale cache)", got)
	}
}

// TestSpeculateInvalidatedByPredeclare covers the other in-place mutation.
func TestSpeculateInvalidatedByPredeclare(t *testing.T) {
	c := newSpecClassifier()
	const probe = "helper.Run"

	if got := c.Preview(probe)[0].Kind; got != Shell {
		t.Fatalf("before predeclare: got %v, want shell", got)
	}
	c.Predeclare("helper")
	if got := c.Preview(probe)[0].Kind; got != Go {
		t.Errorf("after predeclare: got %v, want go (stale cache)", got)
	}
}

// TestSpeculateNotInherited: a clone must start with no memo. Clone is
// handed out to callers that immediately mutate it, so an inherited entry
// would describe a state that no longer exists.
func TestSpeculateNotInherited(t *testing.T) {
	c := newSpecClassifier()
	c.Preview("x := 1")
	if c.Clone().spec != nil {
		t.Error("Clone inherited the speculative cache")
	}
}

// TestSpeculateConstructsNotAliased: PendingInfo is a value the caller
// owns. Handing out the cache's own slice would let one caller's append
// rewrite the next caller's breadcrumb.
func TestSpeculateConstructsNotAliased(t *testing.T) {
	c := newSpecClassifier()
	const src = "func greet() {\nfor {"

	a := c.Pending(src)
	if len(a.Constructs) == 0 {
		t.Fatalf("expected open constructs for %q", src)
	}
	for i := range a.Constructs {
		a.Constructs[i] = "clobbered"
	}
	b := c.Pending(src)
	for _, s := range b.Constructs {
		if s == "clobbered" {
			t.Fatal("Constructs aliases the cache: one caller corrupted another")
		}
	}
}
