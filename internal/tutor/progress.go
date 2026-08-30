package tutor

import (
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/rohanthewiz/bytdb"
)

// Record is one lesson's saved place: which step the student is on, how
// many misses it has cost so far, and when it was last touched.
//
// Attempts is carried across a restart so the hint escalation resumes
// where it left off — a student who quit stuck on step 4 should come back
// to the hint they had earned, not to silence.
type Record struct {
	Lesson   string
	Step     string // step ID, not index: content edits must not teleport a student
	Attempts int
	Revealed int
	Updated  time.Time
}

// Store persists progress. The interface is the point: the plan left the
// backing choice open, so every caller talks to this and the engine never
// learns where a record lives.
type Store interface {
	Load(lesson string) (Record, bool)
	Save(r Record) error
	Close() error
}

// nopStore is the fallback when the real store can't open — a locked
// database, a read-only home, a second tutor already running. Progress is
// a convenience; losing it must never cost the student their lesson, so
// every failure downgrades to this rather than aborting.
type nopStore struct{}

func (nopStore) Load(string) (Record, bool) { return Record{}, false }
func (nopStore) Save(Record) error          { return nil }
func (nopStore) Close() error               { return nil }

// progressTable is the single table the tutor owns, keyed by lesson ID so
// a returning student resumes each chapter independently.
const progressTable = "tutor_progress"

// dbStore is the bytdb-backed Store.
//
// Why the engine API (Open/CreateTable/Insert/Update/Get) rather than the
// SQL front door: this is one row of five columns with a known primary
// key, so the SQL layer would buy parsing and planning we never use — and
// the engine API keeps the dependency to bytdb itself.
type dbStore struct{ e *bytdb.Engine }

// openStore opens ~/.grsh_tutor.db, creating the table on first run.
// It never returns an error: a store that can't open becomes a nopStore,
// because "your progress won't be saved" is a strictly better outcome
// than "the tutor won't start."
func openStore(path string) Store {
	if path == "" {
		return nopStore{}
	}
	e, err := bytdb.Open(path)
	if err != nil {
		return nopStore{}
	}
	if !slices.Contains(e.Tables(), progressTable) {
		_, err = e.CreateTable(progressTable, []bytdb.Column{
			{Name: "lesson", Type: bytdb.TString},
			{Name: "step", Type: bytdb.TString},
			{Name: "attempts", Type: bytdb.TInt},
			{Name: "revealed", Type: bytdb.TInt},
			// Stored as epoch seconds rather than TTimestamp: the field is
			// only ever shown back to the student ("last seen 3 days ago"),
			// and an int keeps the record readable from any tool.
			{Name: "updated", Type: bytdb.TInt},
		}, "lesson")
		if err != nil {
			e.Close()
			return nopStore{}
		}
	}
	return &dbStore{e: e}
}

func (d *dbStore) Load(lesson string) (Record, bool) {
	row, ok, err := d.e.Get(progressTable, lesson)
	if err != nil || !ok {
		return Record{}, false
	}
	return Record{
		Lesson:   lesson,
		Step:     asString(row.Col("step")),
		Attempts: int(asInt(row.Col("attempts"))),
		Revealed: int(asInt(row.Col("revealed"))),
		Updated:  time.Unix(asInt(row.Col("updated")), 0),
	}, true
}

// Save upserts the lesson's row. bytdb's engine API separates Insert
// (primary key must be new) from Update (must exist), so the write is an
// Update that falls back to an Insert on the first save of a lesson.
func (d *dbStore) Save(r Record) error {
	if r.Updated.IsZero() {
		r.Updated = time.Now()
	}
	set := map[string]any{
		"step":     r.Step,
		"attempts": int64(r.Attempts),
		"revealed": int64(r.Revealed),
		"updated":  r.Updated.Unix(),
	}
	updated, err := d.e.Update(progressTable, []any{r.Lesson}, set)
	if err != nil {
		return err
	}
	if updated {
		return nil
	}
	return d.e.Insert(progressTable, r.Lesson, r.Step, int64(r.Attempts), int64(r.Revealed), r.Updated.Unix())
}

func (d *dbStore) Close() error { return d.e.Close() }

// progressPath returns ~/.grsh_tutor.db, matching the ~/.grsh_history and
// ~/.grsh_units conventions already in the codebase, or "" (no
// persistence) when the home directory is unknown.
func progressPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".grsh_tutor.db")
}

// asString and asInt read a column back without assuming a width: the
// engine coerces to its column type on write, but a value read back is an
// `any` and a nil (never written by Save, but possible in a file an older
// build wrote) must degrade rather than panic.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
