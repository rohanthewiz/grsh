package tutor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxFixtureCounts pins the numbers the curriculum grades
// against. These are not incidental: a lesson step says "count the Go
// files" and asserts 3, and the README's own `grep 500 access.log`
// example needs a log that actually contains 500s. Changing a fixture
// without changing its step is the failure this test catches.
func TestSandboxFixtureCounts(t *testing.T) {
	f := fixtures()

	var goFiles int
	for path := range f {
		if filepath.Dir(path) == "." && strings.HasSuffix(path, ".go") {
			goFiles++
		}
	}
	if goFiles != 3 {
		t.Errorf("%d top-level .go files, want 3 (`ls *.go | wc -l` is a lesson step)", goFiles)
	}

	log := f["access.log"]
	lines := strings.Count(log, "\n")
	if lines != 120 {
		t.Errorf("access.log has %d lines, want 120", lines)
	}
	var errs int
	for _, l := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		if strings.Contains(l, " 500 ") {
			errs++
		}
	}
	if errs != accessLogErrors {
		t.Errorf("access.log has %d 500-lines, accessLogErrors says %d", errs, accessLogErrors)
	}
}

// TestAccessLogIsDeterministic: output-based grading is only reliable if
// the fixture is byte-identical on every machine and every run. No
// time.Now, no map iteration, no shuffling.
func TestAccessLogIsDeterministic(t *testing.T) {
	if accessLog() != accessLog() {
		t.Fatal("accessLog is not deterministic")
	}
}

func TestSandboxSeedsAndCleansUp(t *testing.T) {
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	box, err := newSandbox()
	if err != nil {
		t.Fatal(err)
	}

	// The process cwd really moved: the student's `ls` and `glob("*.go")`
	// resolve through it, so a sandbox that only recorded a path would
	// teach the wrong thing at the first unresolved filename.
	cwd, _ := os.Getwd()
	if cwd != box.dir {
		t.Errorf("cwd = %q, want the sandbox %q", cwd, box.dir)
	}
	for rel := range fixtures() {
		if _, err := os.Stat(filepath.Join(box.dir, rel)); err != nil {
			t.Errorf("fixture %s missing: %v", rel, err)
		}
	}
	// The nested subtree exists as a directory, not as a flat name.
	if fi, err := os.Stat(filepath.Join(box.dir, "notes", "archive")); err != nil || !fi.IsDir() {
		t.Errorf("notes/archive should be a directory: %v", err)
	}

	box.cleanup()
	if cwd, _ := os.Getwd(); cwd != prev {
		t.Errorf("cleanup left the cwd at %q, want %q", cwd, prev)
	}
	if _, err := os.Stat(box.dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind: %v", box.dir, err)
	}
}

// TestSandboxPathIsResolved: on macOS MkdirTemp hands back /var/..., a
// symlink to /private/var. If the tutor reported one form while `pwd`
// printed the other, a student comparing them would reasonably conclude
// the tutor was lying about where they are.
func TestSandboxPathIsResolved(t *testing.T) {
	box, err := newSandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer box.cleanup()
	resolved, err := filepath.EvalSymlinks(box.dir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != box.dir {
		t.Errorf("sandbox dir %q is not the resolved path %q", box.dir, resolved)
	}
}
