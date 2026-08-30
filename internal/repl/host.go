package repl

import (
	"io"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// UnitLog is the loop's completed-unit dispatch, minus the terminal: the
// in-memory unit history plus the prompt-only commands that read it
// (`?name`, `session save`). It exists so a host that feeds input units
// programmatically takes the SAME path a student's keystrokes take.
//
// The consumer is the tutor's content self-check. Two of the curriculum's
// steps are prompt affordances rather than language — chapter 3 asks for
// `?count`, and the capstone's whole point is `session save report.grsh`
// followed by sourcing the script back — and neither reaches Eval. A test
// that reimplemented the dispatch could pass while the real prompt
// diverged, and the capstone is exactly where that divergence would go
// unnoticed, because its evidence is a file written by the branch under
// test.
//
// Deliberately not a general "headless REPL": there is no line editor, no
// continuation state, and no interceptor here. Those live in loop, which
// is still the only place that decides when an input unit is complete.
type UnitLog struct{ h *historyStore }

// NewUnitLog returns an empty log with persistence disabled — an
// off-terminal host must never append to the user's ~/.grsh_units.
func NewUnitLog() *UnitLog { return &UnitLog{h: openHistory("")} }

// Submit runs one complete input unit exactly as loop does past the point
// where the unit is known to be complete: prompt-only commands first (they
// never reach history or Eval), then history, then evaluation. It returns
// the eval error, or nil when the unit was handled as a prompt command.
func (u *UnitLog) Submit(src string, sess *runner.Session, outW, errW io.Writer) error {
	if replCommand(src, sess, u.h, outW, errW) {
		return nil
	}
	u.h.Append(src)
	return sess.Eval(src)
}

// Units returns the units submitted so far, oldest first.
func (u *UnitLog) Units() []string { return u.h.Units() }

// UserMessage renders an eval error the way loop prints it — the concise
// one-liner plus, when the interpreter reported a column, the offending
// source line with a caret under it.
//
// It is exported for the same reason Submit is: a host that feeds units
// programmatically should report a failure in the words the prompt would
// have used. runner.UserMessage alone is not that — it stops at the text,
// and the caret block is the half that tells a student WHERE they went
// wrong, which is most of the value in a tutorial.
func UserMessage(src string, err error) string { return userMsg(src, err) }
