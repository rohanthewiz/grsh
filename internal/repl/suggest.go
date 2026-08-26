package repl

// Fish-style ghost text (inline autosuggestion) for the reeflective editor.
//
// While you type, the most recent history unit that STARTS WITH the current
// buffer is drawn dimmed after the cursor; → / ^F accepts all of it, and a
// forward-word key accepts one word. Nothing is ever inserted implicitly:
// the ghost lives in the display engine's inline-suggestion slot, not in the
// line buffer, so what `Readline` returns is only ever what was typed or
// explicitly accepted.
//
// Why a hand-rolled matcher instead of the library's `history-autosuggest`:
// that option suggests from the merged history sources with no filtering,
// and grsh's history is a UNIT store — a recalled entry can be a whole
// multi-line `func`. Ghost text containing a newline is unrenderable here
// (the display engine measures the suggestion as a single logical line, and
// the secondary-prompt gutter belongs between real buffer rows), so those
// entries must be skipped rather than shown. Owning the match also lets us
// pick when a suggestion is appropriate at all — see updateGhost.
//
// # Why there is an index
//
// The first version walked the whole store newest-first per keystroke. That
// is exactly linear in the history size, and history is the one input to
// the display path the user grows without bound: measured at 0.6us per
// keystroke over 100 units, 6us over 1000, and 53us over 10000 — by then
// the largest single item in a typing frame, and still climbing.
//
// So the units are kept lexicographically sorted instead (ghostIndex).
// Every unit sharing a prefix then occupies one contiguous run, which a
// binary search finds in O(log n): a buffer that matches nothing — the
// common case, since it is every keystroke that is not on a remembered
// path — stops after the search instead of touching the store at all.
//
//	10000 units, per keystroke   scan     index
//	buffer matches nothing       53us     51ns
//	buffer matches every unit     14ns    238ns
//
// The second row is the trade, and it is deliberate. The scan answered a
// prefix shared by the NEWEST unit on its first comparison, and paid the
// full 53us for anything else — including a prefix whose only matches are
// old, which is most of what a long history holds. The index answers both
// in a fixed few hundred nanoseconds, so the shape of the history stops
// deciding whether the ghost keeps up with typing. What makes the second
// row bounded rather than a walk of the whole run is the block summary —
// see newestIn.

import (
	"slices"
	"sort"
	"strings"
)

// suggester finds ghost text for a buffer by prefix-matching the unit
// history, newest match first.
//
// The result is memoized on the buffer string because the display engine
// refreshes on cursor-only movement too: without the memo every arrow key
// would re-search.
type suggester struct {
	hist *historyStore
	idx  ghostIndex
	fed  int // units already absorbed from hist into idx

	lastSrc string
	lastSug string
}

func newSuggester(hist *historyStore) *suggester { return &suggester{hist: hist} }

// suggest returns the full suggested line (buffer prefix included, which is
// the shape the display engine expects), or "" when nothing matches.
//
// It runs on the editor's single read loop, so the memo needs no locking —
// same contract as the highlighter's.
func (s *suggester) suggest(src string) string {
	if src == s.lastSrc {
		return s.lastSug
	}
	sug := s.match(src)
	s.lastSrc, s.lastSug = src, sug
	return sug
}

func (s *suggester) match(src string) string {
	// An all-blank buffer matches nearly everything, which makes the ghost
	// flicker on the first keystroke of every prompt; wait for real input.
	// A buffer that already spans rows cannot carry ghost text at all.
	//
	// Both tests come before the sync below so a buffer that can never be
	// suggested for does not pay to index a history it will not read.
	if strings.TrimSpace(src) == "" || strings.Contains(src, "\n") {
		return ""
	}
	if s.fed < s.hist.LenUnits() {
		units := s.hist.Units()
		s.idx.absorb(units, s.fed)
		s.fed = len(units)
	}
	return s.idx.newestWithPrefix(src)
}

// ghostIndex holds the suggestable history units sorted by text, each
// carrying the position of its newest occurrence in the store.
//
// Two parallel slices rather than a slice of structs, so the prefix lookup
// is a plain sort.SearchStrings over the text — the comparisons are the
// whole cost of the query, and this keeps them a direct string compare with
// no per-element indirection or closure call.
//
// Units are DEDUPED: re-running a command overwrites the recency of the
// entry it already has. Ghost text only ever shows the newest match, so the
// older occurrences are unreachable by construction, and a shell history is
// mostly repeats — dropping them shrinks the searched range as well as the
// memory.
//
// Multi-line units never enter at all (they are unrenderable as ghost text;
// see the file comment), so the index is a subset of the store, never a
// copy of it. The strings themselves are shared with the store — only the
// two slices of headers are new.
type ghostIndex struct {
	units []string // distinct, single-line, sorted lexicographically
	seq   []int32  // seq[i] is units[i]'s newest position in the store
	// block[b] is the index of the newest entry in units[b*blockSize:],
	// capped at blockSize entries — the summary that keeps a prefix run
	// from being walked one entry at a time. See newestWithPrefix.
	block []int32
}

// blockSize is how many entries one block summarizes. The query walks at
// most 2*(blockSize-1) individual entries (the partial blocks at each end of
// the prefix run) plus one step per whole block in between, so the two terms
// balance around here for the run lengths that matter: a 10k-entry run costs
// ~280 steps at 64, against ~640 at 16 and ~450 at 256. It is also 256 bytes
// of int32 — four cache lines, so a block summary is one fetch.
const blockSize = 64

// absorb merges the store's units at positions [from:] into the index.
//
// One sorted merge rather than N insertions into the sorted slice. At the
// first prompt `from` is 0 and an entire history file arrives at once,
// where insertion-at-a-time would shift O(N²) bytes — ~1GB of memmove for a
// 10k-unit file, on the first keystroke of the session. Afterwards `from`
// trails by exactly one (a unit is appended per accepted command) and the
// merge degenerates to a copy of the slice, which is the same memmove an
// in-place insert would have paid. So there is no second code path for the
// steady state; it is already the cheap end of this one.
//
// Measured (BenchmarkGhostAbsorb, 10k units): 725us to build the index from
// a whole history file, once, on the first keystroke of the session; 35us
// and 205KB to fold in one accepted command, on the first keystroke after
// each Enter. Both are off the per-keystroke path — the second one only
// just, which is why it is worth knowing it is linear.
func (g *ghostIndex) absorb(units []string, from int) {
	// Positions of the newcomers worth indexing, sorted by (text, position).
	// Sorting positions rather than the strings keeps recency attached to
	// each entry without a second array to permute.
	add := make([]int32, 0, len(units)-from)
	for i := from; i < len(units); i++ {
		if !strings.Contains(units[i], "\n") {
			add = append(add, int32(i))
		}
	}
	slices.SortFunc(add, func(a, b int32) int {
		if c := strings.Compare(units[a], units[b]); c != 0 {
			return c
		}
		return int(a - b) // equal text: oldest first, so the run ends newest
	})
	if len(add) == 0 {
		return
	}

	outU := make([]string, 0, len(g.units)+len(add))
	outS := make([]int32, 0, len(g.units)+len(add))
	i, j := 0, 0
	for i < len(g.units) || j < len(add) {
		if j == len(add) || (i < len(g.units) && g.units[i] < units[add[j]]) {
			outU, outS = append(outU, g.units[i]), append(outS, g.seq[i])
			i++
			continue
		}
		// The newcomer sorts first, or ties an existing entry. Consume the
		// whole run of equal texts and keep the last position in it: that is
		// the most recent run of this command, which is the only one ghost
		// text can ever show.
		text, seq := units[add[j]], add[j]
		for j+1 < len(add) && units[add[j+1]] == text {
			j++
			seq = add[j]
		}
		j++
		if i < len(g.units) && g.units[i] == text {
			i++ // superseded: the newcomer carries a later position
		}
		outU, outS = append(outU, text), append(outS, seq)
	}
	g.units, g.seq = outU, outS

	// The merge produced fresh slices, so the summaries are rebuilt whole
	// rather than patched. That is O(n) over an O(n) merge — free — and it
	// is the reason absorb is the only writer this index has.
	g.block = make([]int32, (len(g.seq)+blockSize-1)/blockSize)
	for b := range g.block {
		best := b * blockSize
		for i := best + 1; i < len(g.seq) && i < (b+1)*blockSize; i++ {
			if g.seq[i] > g.seq[best] {
				best = i
			}
		}
		g.block[b] = int32(best)
	}
}

// newestWithPrefix returns the most recently run unit that src is a strict
// prefix of, or "" when there is none.
func (g *ghostIndex) newestWithPrefix(src string) string {
	// Everything carrying the prefix is one contiguous run starting at the
	// first entry >= src. Nothing without the prefix can fall inside it: a
	// unit u >= src that does not start with src must differ from src at
	// some byte inside src's length, and differ upwards there — which puts
	// it after EVERY string that starts with src, not between them. So the
	// run is [lo, hi), and both ends are a binary search.
	lo := sort.SearchStrings(g.units, src)
	if lo == len(g.units) || !strings.HasPrefix(g.units[lo], src) {
		return "" // the common keystroke: answered without reading the run
	}
	// The one entry equal to src sorts first in the run. Skipping it is what
	// makes the prefix STRICT: the buffer is already that whole command, so
	// there is nothing left to suggest.
	if len(g.units[lo]) == len(src) {
		lo++
	}
	hi := lo + sort.Search(len(g.units)-lo, func(k int) bool {
		return !strings.HasPrefix(g.units[lo+k], src)
	})
	if best := g.newestIn(lo, hi); best >= 0 {
		return g.units[best]
	}
	return ""
}

// newestIn returns the index of the highest-seq entry in [lo, hi), or -1
// when the range is empty.
//
// Walking it entry by entry would put the whole prefix run back in the
// per-keystroke path — 23us over a 10k-entry run, which is what the first
// version of this index cost on a prefix shared by every unit (typing the
// first rune of a command you run constantly). The block summaries collapse
// the interior of the run to one step per blockSize entries; only the
// partial blocks at the two ends are read individually, and those are
// bounded by blockSize whatever the history holds.
//
//	units  [ ... | ###### | ###### | ###### | ... ]
//	              ^^^^ lo   whole blocks   hi ^^
//	              read      one step each      read
//	              per entry (g.block[b])       per entry
func (g *ghostIndex) newestIn(lo, hi int) int {
	best := -1
	take := func(i int) {
		if best < 0 || g.seq[i] > g.seq[best] {
			best = i
		}
	}
	for lo < hi && lo%blockSize != 0 { // partial head block
		take(lo)
		lo++
	}
	for lo+blockSize <= hi { // whole blocks: one summary each
		take(int(g.block[lo/blockSize]))
		lo += blockSize
	}
	for lo < hi { // partial tail block
		take(lo)
		lo++
	}
	return best
}
