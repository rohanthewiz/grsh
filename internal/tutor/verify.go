package tutor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/runner"
)

// Attempt is everything a verifier may look at when grading one input
// unit. It is deliberately read-only and made of surfaces that already
// exist: the tutor never runs a hidden Eval in the user's session, since
// that would pollute $?, history, and the user's trust in what the prompt
// just did. Grading happens through the tee buffer, the eval error, the
// Session's read-only accessors (Inspect/VarInfo, LastStatus, Preview),
// and the sandbox directory on disk.
type Attempt struct {
	Input  string          // the source the user submitted, verbatim
	Output string          // what the command printed this attempt (tee buffer)
	Err    error           // eval error, nil on success
	Sess   *runner.Session // for VarInfo/LastStatus/Preview-style checks
	Dir    string          // sandbox root; the `file` verifier resolves against it
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
// The full set from the plan, and what each is for:
//
//	any-input      demo/observe steps, and the ones kept ungraded on
//	               purpose (Ctrl+Z / fg, where grading a terminal handoff
//	               would be flaky theatre rather than teaching)
//	output-regexp  the workhorse for shell and fmt lessons
//	output-exact   when the whole point is the exact bytes
//	status         exit codes, status(), errexit
//	var            the Go chapters: `n := 42` really bound n to 42
//	file           redirection and writeFile, checked on the real disk
//	classified-as  the classification chapter — grade rules 1..6 directly
//	used-construct force the intended mechanism, not just the right answer
var verifierKinds = map[string]func(arg string) (Verifier, error){
	"any-input":      newAnyInput,
	"output-regexp":  newOutputRegexp,
	"output-exact":   newOutputExact,
	"status":         newStatus,
	"var":            newVar,
	"file":           newFile,
	"classified-as":  newClassifiedAs,
	"used-construct": newUsedConstruct,
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

// All conjoins verifiers: every one must pass.
//
// One `verify:` line per step covers most content, but the interesting
// steps want two clauses joined — "capture with $(...)" is
// `used-construct` (the mechanism) AND `output-regexp` (the result), and
// grading only the result would pass a student who typed the answer
// literally. Conjunction rather than a richer spec grammar keeps each
// kind a single-purpose, independently testable predicate.
//
// A step's `verify:` lines are collected in order, so the Phase-3 loader
// maps repeated lines onto this with no new syntax.
func All(vs ...Verifier) Verifier {
	if len(vs) == 1 {
		return vs[0]
	}
	return allOf(vs)
}

// MustAll is All over raw specs, for lessons defined in Go source.
func MustAll(specs ...string) Verifier {
	vs := make([]Verifier, len(specs))
	for i, s := range specs {
		vs[i] = MustVerifier(s)
	}
	return All(vs...)
}

type allOf []Verifier

func (as allOf) Verify(a Attempt) bool {
	for _, v := range as {
		if !v.Verify(a) {
			return false
		}
	}
	return true
}

func (as allOf) Spec() string {
	parts := make([]string, len(as))
	for i, v := range as {
		parts[i] = v.Spec()
	}
	return strings.Join(parts, " && ")
}

// anyInput advances on any complete input unit. It backs demo/observe
// steps — "press on once you've seen this" — and the steps the plan keeps
// ungraded on purpose (Ctrl+Z / fg).
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

// outputExact demands the trimmed output equal a literal, for the steps
// where "close enough" is not the lesson — a formatting exercise whose
// whole content is the spacing, say. Everything looser uses
// output-regexp; reaching for this kind is a deliberate tightening.
type outputExact struct{ want string }

func newOutputExact(arg string) (Verifier, error) {
	if arg == "" {
		return nil, fmt.Errorf("output-exact needs the expected text")
	}
	return outputExact{want: arg}, nil
}

func (o outputExact) Verify(a Attempt) bool {
	if a.Err != nil {
		return false
	}
	return strings.TrimSpace(a.Output) == o.want
}

func (o outputExact) Spec() string { return "output-exact " + o.want }

// status grades the session's last exit status: `status 0`, `status 1`,
// or `status nonzero` when any failure will do.
//
// This is the chapter-6 verifier. "Run something that fails, then read
// status()" is a step whose *output* proves nothing — the teaching is
// entirely in the code the shell recorded — so it grades the number the
// prompt is about to show in its `[1]` badge.
//
// Unlike the output kinds, it does not reject a.Err: a nonzero status IS
// the expected result here, and the loop reports the same condition as an
// error for pipelines under errexit.
type status struct {
	want    int
	nonzero bool
	raw     string
}

func newStatus(arg string) (Verifier, error) {
	switch arg {
	case "":
		return nil, fmt.Errorf("status needs a code or `nonzero`")
	case "nonzero":
		return status{nonzero: true, raw: arg}, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		return nil, fmt.Errorf("status %q: want a number or `nonzero`", arg)
	}
	return status{want: n, raw: arg}, nil
}

func (s status) Verify(a Attempt) bool {
	if a.Sess == nil {
		return false
	}
	got := a.Sess.LastStatus()
	if s.nonzero {
		return got != 0
	}
	return got == s.want
}

func (s status) Spec() string { return "status " + s.raw }

// varCheck grades a Go binding through Session.VarInfo — the read-only
// half of `?name`. Spec form, with both predicates optional:
//
//	var n
//	var n type=int
//	var n value=^42$
//	var files type=[]string value=^\[a\.go b\.go\]$
//
// Keyword predicates rather than positional fields: a step usually cares
// about one of the two (that `count` is an int; that `name` says "ada"),
// and demanding a placeholder for the other would read as noise in a
// content file. Both are regexps anchored by the author, matched against
// VarInfo's raw strings, so `type=int` also matches `interface{}` unless
// the author writes `type=^int$` — documented in the content guide rather
// than guessed at here, since `type=\[\]string` needs the same care.
type varCheck struct {
	name     string
	typ, val *regexp.Regexp
	raw      string
}

func newVar(arg string) (Verifier, error) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return nil, fmt.Errorf("var needs a variable name")
	}
	v := varCheck{name: fields[0], raw: arg}
	if !isIdent(v.name) {
		return nil, fmt.Errorf("var %q: not an identifier", v.name)
	}
	for _, f := range fields[1:] {
		key, pat, ok := strings.Cut(f, "=")
		if !ok || pat == "" {
			return nil, fmt.Errorf("var %s: bad predicate %q (want type=RE or value=RE)", v.name, f)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("var %s: %s: %w", v.name, key, err)
		}
		switch key {
		case "type":
			v.typ = re
		case "value":
			v.val = re
		default:
			return nil, fmt.Errorf("var %s: unknown predicate %q", v.name, key)
		}
	}
	return v, nil
}

func (v varCheck) Verify(a Attempt) bool {
	if a.Sess == nil {
		return false
	}
	typ, val, ok := a.Sess.VarInfo(v.name)
	if !ok {
		return false // the binding doesn't exist: the step wasn't done
	}
	if v.typ != nil && !v.typ.MatchString(typ) {
		return false
	}
	return v.val == nil || v.val.MatchString(val)
}

func (v varCheck) Spec() string { return "var " + v.raw }

// fileCheck grades the sandbox filesystem: that a redirection or
// writeFile actually produced the file, and optionally what is in it.
//
//	file errs.txt
//	file errs.txt contains=500
//	file report.grsh contains=(?m)^#!/usr/bin/env grsh$
//
// The path resolves against the sandbox root, not the process cwd, so a
// step stays gradable after the student wanders off with `cd notes`.
// Absolute paths are honored as written — the capstone deliberately
// writes outside the sandbox.
type fileCheck struct {
	path     string
	contains *regexp.Regexp
	raw      string
}

func newFile(arg string) (Verifier, error) {
	path, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	if path == "" {
		return nil, fmt.Errorf("file needs a path")
	}
	f := fileCheck{path: path, raw: arg}
	if rest = strings.TrimSpace(rest); rest != "" {
		pat, ok := strings.CutPrefix(rest, "contains=")
		if !ok || pat == "" {
			return nil, fmt.Errorf("file %s: bad predicate %q (want contains=RE)", path, rest)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("file %s: contains: %w", path, err)
		}
		f.contains = re
	}
	return f, nil
}

func (f fileCheck) Verify(a Attempt) bool {
	path := f.path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.Dir, path)
	}
	if f.contains == nil {
		// Existence only: Stat rather than ReadFile, so a step can check
		// for a directory (`mkdir out`) with the same verifier kind.
		_, err := os.Stat(path)
		return err == nil
	}
	b, err := os.ReadFile(path)
	return err == nil && f.contains.Match(b)
}

func (f fileCheck) Spec() string { return "file " + f.raw }

// classifiedAs grades how grsh *read* the line, not what it did.
//
// This is the verifier no other shell tutorial can have: chapter 2 says
// "make this run as shell" and grades the student's grasp of
// classification rules 1-6 directly, by asking the classifier the same
// question the REPL asked a moment ago.
//
// Session.Preview runs on a clone, so this is genuinely read-only — no
// scope is declared, no state moves. Blank chunks are skipped and every
// remaining chunk must match: a multi-line unit that mixes shell and Go
// has not answered "make this run as shell".
type classifiedAs struct {
	kind classify.Kind
	raw  string
}

func newClassifiedAs(arg string) (Verifier, error) {
	switch arg {
	case "shell":
		return classifiedAs{kind: classify.Shell, raw: arg}, nil
	case "go":
		return classifiedAs{kind: classify.Go, raw: arg}, nil
	}
	return nil, fmt.Errorf("classified-as %q: want `shell` or `go`", arg)
}

func (c classifiedAs) Verify(a Attempt) bool {
	if a.Sess == nil {
		return false
	}
	chunks := a.Sess.Preview(a.Input)
	seen := false
	for _, ch := range chunks {
		if ch.Kind == classify.Blank {
			continue
		}
		if ch.Kind != c.kind {
			return false
		}
		seen = true
	}
	return seen // an all-blank unit classified as nothing
}

func (c classifiedAs) Spec() string { return "classified-as " + c.raw }

// usedConstruct matches the student's *input*, forcing the intended
// mechanism: `used-construct \$\(` makes the bridge chapter's step about
// command substitution rather than about knowing the answer. It is
// almost always conjoined with a result check via All — on its own it
// would pass a line that contains `$(` and does nothing else.
type usedConstruct struct {
	re  *regexp.Regexp
	raw string
}

func newUsedConstruct(arg string) (Verifier, error) {
	if arg == "" {
		return nil, fmt.Errorf("used-construct needs a pattern")
	}
	re, err := regexp.Compile(arg)
	if err != nil {
		return nil, fmt.Errorf("used-construct %q: %w", arg, err)
	}
	return usedConstruct{re: re, raw: arg}, nil
}

func (u usedConstruct) Verify(a Attempt) bool { return u.re.MatchString(a.Input) }
func (u usedConstruct) Spec() string          { return "used-construct " + u.raw }

// isIdent reports whether s is a plain Go identifier, the only thing a
// `var` verifier can name.
func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
