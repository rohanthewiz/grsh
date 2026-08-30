package tutor

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// liveSession builds a headless session whose output lands in a buffer,
// so a verifier can be graded against a command that really ran rather
// than against a hand-written Attempt. The verifiers that read session
// state (var, status, classified-as) are only meaningful this way.
func liveSession(t *testing.T) (*runner.Session, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return runner.NewSession(runner.Options{
		ScriptName: "tutor-test",
		Stdout:     &out,
		Stderr:     &out,
	}), &out
}

// attemptOf runs src for real and packages the result the way the engine
// does, so a verifier test exercises the same Attempt shape production
// grading sees.
func attemptOf(t *testing.T, sess *runner.Session, out *bytes.Buffer, src, dir string) Attempt {
	t.Helper()
	out.Reset()
	err := sess.Eval(src)
	return Attempt{Input: src, Output: out.String(), Err: err, Sess: sess, Dir: dir}
}

func TestVarVerifier(t *testing.T) {
	sess, out := liveSession(t)
	a := attemptOf(t, sess, out, "n := 42", "")
	if a.Err != nil {
		t.Fatalf("eval: %v", a.Err)
	}
	_ = attemptOf(t, sess, out, `who := "ada"`, "")

	tests := []struct {
		spec string
		want bool
	}{
		{"var n", true},
		{"var n type=^int$", true},
		{"var n value=^42$", true},
		{"var n type=^int$ value=^42$", true},
		{"var n value=^43$", false},
		{"var n type=^string$", false},
		{"var missing", false},
		// A string's value is its RAW contents: content files write
		// value=^ada$, never value=^"ada"$ — the quoting is the human
		// inspector's, not the data's.
		{"var who type=^string$ value=^ada$", true},
	}
	for _, tc := range tests {
		got := MustVerifier(tc.spec).Verify(Attempt{Sess: sess})
		if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// TestVarVerifierIsUntruncated guards the reason VarInfo exists rather
// than reusing Inspect's rendered line: the human inspector elides past
// 60 runes, and a verifier grading that string would pass any answer
// whose first 60 runes were right.
func TestVarVerifierIsUntruncated(t *testing.T) {
	sess, out := liveSession(t)
	long := "0123456789012345678901234567890123456789012345678901234567890123456789END"
	attemptOf(t, sess, out, `s := "`+long+`"`, "")
	if !MustVerifier("var s value=END$").Verify(Attempt{Sess: sess}) {
		typ, val, _ := sess.VarInfo("s")
		t.Errorf("long string value was truncated: type=%q value=%q", typ, val)
	}
}

func TestStatusVerifier(t *testing.T) {
	sess, out := liveSession(t)

	attemptOf(t, sess, out, "true", "")
	if !MustVerifier("status 0").Verify(Attempt{Sess: sess}) {
		t.Errorf("after `true`, status 0 should pass (got %d)", sess.LastStatus())
	}
	if MustVerifier("status nonzero").Verify(Attempt{Sess: sess}) {
		t.Error("after `true`, status nonzero should fail")
	}

	attemptOf(t, sess, out, "false", "")
	if !MustVerifier("status nonzero").Verify(Attempt{Sess: sess}) {
		t.Errorf("after `false`, status nonzero should pass (got %d)", sess.LastStatus())
	}
	if MustVerifier("status 0").Verify(Attempt{Sess: sess}) {
		t.Error("after `false`, status 0 should fail")
	}
}

// TestStatusVerifierAcceptsAFailedEval is the deliberate difference from
// the output kinds: chapter 6 asks the student to MAKE something fail and
// then read status(), so an eval error is the expected result, not a
// disqualification.
func TestStatusVerifierAcceptsAFailedEval(t *testing.T) {
	sess, out := liveSession(t)
	a := attemptOf(t, sess, out, "false", "")
	a.Err = os.ErrClosed // stand in for a loop-reported failure
	if !MustVerifier("status nonzero").Verify(a) {
		t.Error("status must grade the recorded code, not the eval error")
	}
}

func TestClassifiedAsVerifier(t *testing.T) {
	sess, out := liveSession(t)
	tests := []struct {
		src, spec string
		want      bool
	}{
		{"echo hi", "classified-as shell", true},
		{"echo hi", "classified-as go", false},
		{"n := 1", "classified-as go", true},
		{"n := 1", "classified-as shell", false},
	}
	for _, tc := range tests {
		a := attemptOf(t, sess, out, tc.src, "")
		if got := MustVerifier(tc.spec).Verify(a); got != tc.want {
			t.Errorf("%q %s = %v, want %v", tc.src, tc.spec, got, tc.want)
		}
	}
	// An all-blank unit classified as nothing must not satisfy either.
	if MustVerifier("classified-as shell").Verify(Attempt{Input: "", Sess: sess}) {
		t.Error("an empty unit should not classify as shell")
	}
}

func TestUsedConstructVerifier(t *testing.T) {
	v := MustVerifier(`used-construct \$\(`)
	if !v.Verify(Attempt{Input: "fmt.Println($(echo hi))"}) {
		t.Error("$( should match")
	}
	if v.Verify(Attempt{Input: "echo hi"}) {
		t.Error("a line without $( should not match")
	}
}

func TestFileVerifier(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "errs.txt"), []byte("a 500 line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		spec string
		want bool
	}{
		{"file errs.txt", true},
		{"file errs.txt contains=500", true},
		{"file errs.txt contains=404", false},
		{"file missing.txt", false},
		{"file out", true}, // existence-only works for a directory
	}
	for _, tc := range tests {
		if got := MustVerifier(tc.spec).Verify(Attempt{Dir: dir}); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.spec, got, tc.want)
		}
	}
}

// TestFileVerifierResolvesAgainstTheSandbox: the student may wander off
// with `cd notes`, and the step must still grade.
func TestFileVerifierResolvesAgainstTheSandbox(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "report.txt"), []byte("x"), 0o644)
	sub := filepath.Join(dir, "notes")
	os.Mkdir(sub, 0o755)

	prev, _ := os.Getwd()
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)

	if !MustVerifier("file report.txt").Verify(Attempt{Dir: dir}) {
		t.Error("a relative path must resolve against the sandbox root, not the cwd")
	}
}

func TestOutputExactVerifier(t *testing.T) {
	v := MustVerifier("output-exact hello")
	if !v.Verify(Attempt{Output: "  hello\n"}) {
		t.Error("output-exact matches the trimmed output")
	}
	if v.Verify(Attempt{Output: "hello there"}) {
		t.Error("output-exact must not match a superstring")
	}
	if v.Verify(Attempt{Output: "hello", Err: os.ErrClosed}) {
		t.Error("an errored attempt must not pass an output check")
	}
}

// TestAllConjoins is the reason the demo's bridge step no longer passes
// for `echo bridge`: the result is right, the mechanism is not.
func TestAllConjoins(t *testing.T) {
	v := MustAll(`used-construct \$\(`, "output-regexp (?m)^bridge$")
	right := Attempt{Input: "fmt.Println($(echo bridge))", Output: "bridge\n"}
	if !v.Verify(right) {
		t.Error("the intended answer should pass both clauses")
	}
	shortcut := Attempt{Input: "echo bridge", Output: "bridge\n"}
	if v.Verify(shortcut) {
		t.Error("right output via the wrong mechanism must not pass")
	}
	if v.Spec() != `used-construct \$\( && output-regexp (?m)^bridge$` {
		t.Errorf("Spec = %q", v.Spec())
	}
	// A single verifier is returned unwrapped, so Spec stays the plain
	// line a lesson author wrote.
	if got := All(MustVerifier("any-input")).Spec(); got != "any-input" {
		t.Errorf("All of one = %q, want the bare spec", got)
	}
}

func TestParseVerifierRejectsBadSpecs(t *testing.T) {
	bad := []string{
		"",
		"nope 1",
		"any-input extra",
		"output-regexp",
		"output-regexp [",
		"output-exact",
		"status",
		"status maybe",
		"var",
		"var 9lives",
		"var n bogus=1",
		"var n type=[",
		"var n type",
		"file",
		"file f.txt startswith=x",
		"file f.txt contains=[",
		"classified-as",
		"classified-as perl",
		"used-construct",
		"used-construct [",
	}
	for _, spec := range bad {
		if v, err := ParseVerifier(spec); err == nil {
			t.Errorf("ParseVerifier(%q) = %v, want an error", spec, v.Spec())
		}
	}
}

// TestVerifierSpecsRoundTrip: every kind reports the line that built it,
// which is what makes a content-test failure name the offending step's
// verifier rather than a Go type.
func TestVerifierSpecsRoundTrip(t *testing.T) {
	specs := []string{
		"any-input",
		"output-regexp (?m)^hi$",
		"output-exact hi",
		"status 1",
		"status nonzero",
		"var n type=^int$",
		"file a.txt contains=x",
		"classified-as shell",
		`used-construct \$\(`,
	}
	for _, spec := range specs {
		if got := MustVerifier(spec).Spec(); got != spec {
			t.Errorf("Spec() = %q, want %q", got, spec)
		}
	}
}

// TestEveryVerifierKindIsParseable pins the table itself: a kind added to
// the map without a sample here is a kind no test covers, which is how a
// broken constructor reaches the first content file that uses it.
func TestEveryVerifierKindIsParseable(t *testing.T) {
	samples := map[string]string{
		"any-input":      "any-input",
		"output-regexp":  "output-regexp ^x$",
		"output-exact":   "output-exact x",
		"status":         "status 0",
		"var":            "var n",
		"file":           "file a.txt",
		"classified-as":  "classified-as shell",
		"used-construct": `used-construct \$\(`,
	}
	for kind := range verifierKinds {
		spec, ok := samples[kind]
		if !ok {
			t.Errorf("verifier kind %q has no sample: add one so the kind is covered", kind)
			continue
		}
		if _, err := ParseVerifier(spec); err != nil {
			t.Errorf("kind %q rejected its own sample %q: %v", kind, spec, err)
		}
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
