package repl

import "testing"

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
