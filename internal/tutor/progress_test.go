package tutor

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProgressRoundTrip(t *testing.T) {
	store := openStore(filepath.Join(t.TempDir(), "p.db"))
	defer store.Close()
	if _, ok := store.Load("demo"); ok {
		t.Fatal("a fresh store should hold no record")
	}

	want := Record{Lesson: "demo", Step: "go-expr", Attempts: 2, Revealed: 1}
	if err := store.Save(want); err != nil {
		t.Fatalf("first save (insert): %v", err)
	}
	got, ok := store.Load("demo")
	if !ok {
		t.Fatal("record not found after save")
	}
	if got.Step != want.Step || got.Attempts != want.Attempts || got.Revealed != want.Revealed {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
	if got.Updated.IsZero() {
		t.Error("Save should stamp Updated when the caller leaves it zero")
	}

	// The second save of the same lesson is an Update, not an Insert —
	// the engine API separates them and a wrong branch here would fail
	// on a duplicate primary key.
	want.Step, want.Attempts = "bridge", 0
	if err := store.Save(want); err != nil {
		t.Fatalf("second save (update): %v", err)
	}
	got, _ = store.Load("demo")
	if got.Step != "bridge" || got.Attempts != 0 {
		t.Errorf("update did not take: %+v", got)
	}

	// Lessons are keyed independently, so chapters resume separately.
	if err := store.Save(Record{Lesson: "other", Step: "one"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Load("demo"); got.Step != "bridge" {
		t.Errorf("saving another lesson disturbed demo: %+v", got)
	}
}

// TestProgressReopens: the point of persistence is surviving the process.
func TestProgressReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	store := openStore(path)
	if err := store.Save(Record{Lesson: "demo", Step: "bridge", Revealed: 2, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened := openStore(path)
	defer reopened.Close()
	got, ok := reopened.Load("demo")
	if !ok || got.Step != "bridge" || got.Revealed != 2 {
		t.Errorf("after reopen: %+v ok=%v", got, ok)
	}
}

// TestProgressDegradesToNop: losing progress must never cost a lesson.
// An unopenable path (here, a database under a file rather than a
// directory) yields a store that answers everything harmlessly.
func TestProgressDegradesToNop(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "not-a-dir.db", "p.db")} {
		store := openStore(path)
		if _, ok := store.Load("demo"); ok {
			t.Errorf("%q: a degraded store should report no record", path)
		}
		if err := store.Save(Record{Lesson: "demo", Step: "x"}); err != nil {
			t.Errorf("%q: a degraded store should swallow saves, got %v", path, err)
		}
		// And the save really went nowhere — this is what distinguishes a
		// nopStore from a store that happened to open somewhere unexpected.
		if _, ok := store.Load("demo"); ok {
			t.Errorf("%q: a degraded store must not persist", path)
		}
		if err := store.Close(); err != nil {
			t.Errorf("%q: Close: %v", path, err)
		}
	}
}

// TestResumeAt covers the reason a record stores a step ID rather than an
// index: editing the curriculum must never teleport a returning student
// into the middle of a step they have not seen.
func TestResumeAt(t *testing.T) {
	l := demoLesson()
	tests := []struct {
		name  string
		rec   Record
		found bool
		want  int
	}{
		{"no record", Record{}, false, 0},
		{"first step", Record{Step: "shell-echo"}, true, 0},
		{"middle step", Record{Step: "go-expr"}, true, 1},
		{"last step", Record{Step: "bridge"}, true, 2},
		// A finished lesson saves an empty step: re-running the tutor
		// should start the chapter, not re-print the completion banner.
		{"finished", Record{Step: ""}, true, 0},
		// A step that no longer exists (renamed or deleted content).
		{"stale id", Record{Step: "removed-in-a-later-draft"}, true, 0},
	}
	for _, tc := range tests {
		if got := resumeAt(l, tc.rec, tc.found); got != tc.want {
			t.Errorf("%s: resumeAt = %d, want %d", tc.name, got, tc.want)
		}
	}
}
