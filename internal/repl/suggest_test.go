package repl

import (
	"fmt"
	"slices"
	"testing"
)

// TestSuggesterMatch pins the ghost-text matching rules: newest unit wins,
// the buffer must be a strict prefix, and neither a multi-line buffer nor a
// multi-line unit may take part (an inline suggestion is measured and
// painted as a single logical line).
func TestSuggesterMatch(t *testing.T) {
	store := openHistory("")
	for _, u := range []string{
		"echo old",
		"go build ./...",
		"echo hello world",
		"func f() {\n  return 1\n}", // multi-line unit: never suggestable
		"ls -la",
	} {
		store.Append(u)
	}
	s := newSuggester(store)

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"prefix match", "echo h", "echo hello world"},
		{"newest wins", "echo", "echo hello world"},
		{"older still reachable", "echo o", "echo old"},
		{"whole line, nothing to add", "ls -la", ""},
		{"no match", "cargo", ""},
		{"blank buffer", "   ", ""},
		{"empty buffer", "", ""},
		{"multi-line buffer", "func f() {\n", ""},
		{"multi-line unit skipped", "func f(", ""},
		{"exact prefix of a multi-line unit only", "func", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.suggest(tc.src); got != tc.want {
				t.Errorf("suggest(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestSuggesterMemo: the display engine refreshes on cursor-only movement,
// so repeated calls for the same buffer must not rescan history. Proven by
// mutating the store behind the memo and expecting the stale answer.
func TestSuggesterMemo(t *testing.T) {
	store := openHistory("")
	store.Append("echo one")
	s := newSuggester(store)

	if got := s.suggest("echo"); got != "echo one" {
		t.Fatalf("first suggest = %q", got)
	}
	store.Append("echo two")
	if got := s.suggest("echo"); got != "echo one" {
		t.Errorf("memoized suggest = %q, want the cached %q", got, "echo one")
	}
	if got := s.suggest("echo t"); got != "echo two" {
		t.Errorf("changed buffer must rescan; got %q", got)
	}
}

// TestSuggesterDedupes: a command run twice is one index entry carrying the
// LATER position, so re-running an old command makes it win against things
// typed since. The linear scan got this for free by walking newest-first;
// the index has to carry recency explicitly, so it needs a test.
func TestSuggesterDedupes(t *testing.T) {
	store := openHistory("")
	for _, u := range []string{"git status", "git push", "git status"} {
		store.Append(u)
	}
	s := newSuggester(store)
	if got := s.suggest("git "); got != "git status" {
		t.Errorf("suggest(%q) = %q, want the re-run %q", "git ", got, "git status")
	}
	if n := len(s.idx.units); n != 2 {
		t.Errorf("index holds %d entries, want 2 (the repeat deduped)", n)
	}
}

// TestSuggesterIncrementalMatchesBulk pins the merge: absorbing a history
// one unit at a time (the steady state, one per accepted command) must build
// exactly the index that absorbing it in one pass does (the first prompt).
// The two are the same code path by design — this is what says so.
func TestSuggesterIncrementalMatchesBulk(t *testing.T) {
	units := []string{
		"ls -la", "go test ./...", "ls -la", "func f() {\n\treturn\n}",
		"go build", "cd /tmp", "go test ./internal/...", "ls",
	}
	var bulk, incr ghostIndex
	bulk.absorb(units, 0)
	for i := range units {
		incr.absorb(units[:i+1], i)
	}
	if !slices.Equal(bulk.units, incr.units) {
		t.Errorf("units differ:\n bulk %q\n incr %q", bulk.units, incr.units)
	}
	if !slices.Equal(bulk.seq, incr.seq) {
		t.Errorf("recency differs:\n bulk %v\n incr %v", bulk.seq, incr.seq)
	}
	if !slices.Equal(bulk.block, incr.block) {
		t.Errorf("block summaries differ:\n bulk %v\n incr %v", bulk.block, incr.block)
	}
	if slices.Contains(bulk.units, units[3]) {
		t.Errorf("multi-line unit %q must never enter the index", units[3])
	}
}

// TestGhostIndexNewestAcrossBlocks exercises the block summaries, which only
// come into play once a prefix run is longer than blockSize. The winner is
// planted at each of the three places the range walk reads differently: the
// partial block at the head of the run, a whole block in the middle (reached
// only through g.block), and the partial block at the tail.
//
// Recency is the store position, so a unit is made newest by appending it
// again — which is also what a user does to it.
func TestGhostIndexNewestAcrossBlocks(t *testing.T) {
	const n = 3*blockSize + 7 // three whole blocks and two partial ends
	base := make([]string, 0, n)
	for i := range n {
		base = append(base, fmt.Sprintf("job run --id %03d", i))
	}
	for _, want := range []int{0, 1, blockSize + 5, 2 * blockSize, n - 1} {
		t.Run(fmt.Sprintf("newest-at-%d", want), func(t *testing.T) {
			store := openHistory("")
			for _, u := range base {
				store.Append(u)
			}
			store.Append(base[want]) // re-run: now the newest match
			s := newSuggester(store)
			if got := s.suggest("job run "); got != base[want] {
				t.Errorf("suggest = %q, want %q", got, base[want])
			}
		})
	}
}

// TestGhostIndexPrefixRunBounds: the range walk trusts that [lo, hi) holds
// exactly the entries carrying the prefix, so the neighbours on both sides
// have to stay out of it — including entries that sort between the prefix
// and its run ("job" itself), and the one equal to the buffer (there is
// nothing left of it to suggest).
func TestGhostIndexPrefixRunBounds(t *testing.T) {
	store := openHistory("")
	for _, u := range []string{
		"job", "job run", "jobs -l", "joba", "job run --id 7", "kill %1",
	} {
		store.Append(u)
	}
	s := newSuggester(store)
	cases := []struct{ src, want string }{
		{"job run", "job run --id 7"}, // exact entry skipped, longer one wins
		{"job r", "job run --id 7"},   // "jobs -l"/"joba" are outside the run
		{"jobs", "jobs -l"},           // a neighbour has its own run
		{"jobs -l", ""},               // the only match IS the buffer
		{"job x", ""},                 // inside the "job" neighbourhood, no run
		{"k", "kill %1"},
	}
	for _, tc := range cases {
		if got := s.suggest(tc.src); got != tc.want {
			t.Errorf("suggest(%q) = %q, want %q", tc.src, got, tc.want)
		}
	}
}
