package repl

// One []rune → string conversion per frame, shared by everything the
// display engine derives from the buffer.
//
// The editor hands every per-refresh callback the live buffer as []rune,
// and each consumer wants a string: the highlighter, the hint lane and the
// ghost-text scan all memoize on the buffer text, and Pending is asked the
// same question on accept. Before this, each of them ran its own
// `string(line)` — a fresh allocation and a full UTF-8 encode of the whole
// buffer, paid even when nothing had changed and every one of those memos
// was about to report a hit. On a 20-line buffer that was ~1.2 KB of
// garbage and ~2.6us per cursor-only refresh (arrow keys), for an answer
// already computed.
//
// The conversion is now done once, here, and the SAME string handed to all
// of them. Two things fall out of that:
//
//   - The conversion is skipped entirely when the buffer is unchanged;
//     runeIntern compares the incoming runes against the string it already
//     holds, which allocates nothing.
//   - The downstream memos get a pointer-equal string, so their own
//     `src == h.lastSrc` compares short-circuit in the runtime instead of
//     running a memcmp over the buffer. That is the second half of the win
//     and the reason for sharing one intern rather than giving each
//     consumer its own.
//
// Not reset between prompts, unlike the hint memo: this is a pure function
// of the runes handed in, with no session state behind it, so a stale entry
// can only ever cause one extra compare.
//
// Like the memos it feeds, it runs on the editor's single read loop and
// needs no locking.
import "unicode/utf8"

type runeIntern struct {
	// s is the last buffer, converted. n is its rune count, kept so the
	// common miss — a keystroke, which always changes the length — costs
	// one integer compare instead of a walk.
	s string
	n int
}

// str returns line as a string, reusing the previous result when the runes
// are unchanged.
func (ri *runeIntern) str(line []rune) string {
	if ri.n == len(line) && runesEqual(line, ri.s) {
		return ri.s
	}
	ri.s, ri.n = string(line), len(line)
	return ri.s
}

// runesEqual reports whether s is exactly what string(line) would produce,
// without producing it. Decoding s is preferred over expanding line into a
// second []rune buffer: the check then needs no storage of its own, and the
// ASCII fast path walks one byte per rune.
//
// An invalid rune in line (which string() would fold to U+FFFD) simply
// never matches, so such a buffer converts on every frame — correct, and
// not a state a terminal can put the editor in anyway.
func runesEqual(line []rune, s string) bool {
	for _, r := range line {
		if len(s) == 0 {
			return false
		}
		if c := s[0]; c < utf8.RuneSelf { // ASCII: the overwhelming case
			if rune(c) != r {
				return false
			}
			s = s[1:]
			continue
		}
		sr, size := utf8.DecodeRuneInString(s)
		if sr != r {
			return false
		}
		s = s[size:]
	}
	return len(s) == 0 // trailing bytes mean s is longer than line
}
