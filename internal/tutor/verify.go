package tutor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// Attempt is everything a verifier may look at when grading one input
// unit. It is deliberately read-only and made of surfaces that already
// exist: the tutor never runs a hidden Eval in the user's session, since
// that would pollute $?, history, and the user's trust in what the prompt
// just did. Grading happens through the tee buffer, the eval error, and
// the Session's read-only accessors (Inspect, LastStatus, Idents).
type Attempt struct {
	Input  string          // the source the user submitted, verbatim
	Output string          // what the command printed this attempt (tee buffer)
	Err    error           // eval error, nil on success
	Sess   *runner.Session // for Inspect/LastStatus-style checks
}

// Verifier grades one attempt at one step.
type Verifier interface {
	// Verify reports whether the attempt satisfies the step.
	Verify(a Attempt) bool
	// Spec returns the verifier line that produced this verifier, so
	// failures and tests can name it.
	Spec() string
}

// verifierKinds is the table the lesson format's `verify:` line resolves
// against. Adding a verifier kind is adding one entry here plus its type
// — content files never need engine changes to use it.
//
// Phase 1 ships the two kinds the demo lesson needs; the remaining kinds
// from the plan (status, var, file, classified-as, used-construct) slot
// in the same way.
var verifierKinds = map[string]func(arg string) (Verifier, error){
	"any-input":     newAnyInput,
	"output-regexp": newOutputRegexp,
}

// ParseVerifier builds a verifier from a `verify:` line: the first field
// is the kind, the rest of the line (untrimmed after the single
// separating space) is that kind's argument.
func ParseVerifier(spec string) (Verifier, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty verifier spec")
	}
	kind, arg, _ := strings.Cut(spec, " ")
	make, ok := verifierKinds[kind]
	if !ok {
		return nil, fmt.Errorf("unknown verifier kind %q", kind)
	}
	return make(strings.TrimSpace(arg))
}

// MustVerifier is ParseVerifier for lessons defined in Go source, where a
// bad spec is a programming error rather than bad user content.
func MustVerifier(spec string) Verifier {
	v, err := ParseVerifier(spec)
	if err != nil {
		panic("tutor: " + err.Error())
	}
	return v
}

// anyInput advances on any complete input unit. It backs demo/observe
// steps — "press on once you've seen this" — and the steps the plan keeps
// ungraded on purpose (Ctrl+Z / fg, where grading a terminal handoff
// would be flaky theatre rather than teaching.)
type anyInput struct{}

func newAnyInput(arg string) (Verifier, error) {
	if arg != "" {
		return nil, fmt.Errorf("any-input takes no argument, got %q", arg)
	}
	return anyInput{}, nil
}

func (anyInput) Verify(Attempt) bool { return true }
func (anyInput) Spec() string        { return "any-input" }

// outputRegexp matches the attempt's captured output against a regexp.
//
// The match runs against the *trimmed* output, and the pattern is
// compiled with no implicit flags — a lesson that needs per-line anchors
// writes them itself, e.g. `output-regexp (?m)^42$`. Trimming is what
// makes `^hello$` work for `echo hello` without every content file
// having to spell the trailing newline.
type outputRegexp struct {
	re  *regexp.Regexp
	raw string
}

func newOutputRegexp(arg string) (Verifier, error) {
	if arg == "" {
		return nil, fmt.Errorf("output-regexp needs a pattern")
	}
	re, err := regexp.Compile(arg)
	if err != nil {
		return nil, fmt.Errorf("output-regexp %q: %w", arg, err)
	}
	return outputRegexp{re: re, raw: arg}, nil
}

func (o outputRegexp) Verify(a Attempt) bool {
	// A step that printed the right thing but errored out still failed:
	// the point of `ls missing && echo ok` is that the && short-circuits.
	if a.Err != nil {
		return false
	}
	return o.re.MatchString(strings.TrimSpace(a.Output))
}

func (o outputRegexp) Spec() string { return "output-regexp " + o.raw }
