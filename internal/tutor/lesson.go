package tutor

// Step is one graded exercise. The field set mirrors the markdown lesson
// format the plan specifies, so the Phase-2 loader has nothing to invent:
//
//	## step: pipe-count
//	Count the Go files in this directory using a pipe.
//	Try: `ls *.go | wc -l`
//	---
//	verify: output-regexp ^\s*3\s*$
//	hint: `ls *.go` lists them; pipe into `wc -l` like in bash.
//	solution: ls *.go | wc -l
type Step struct {
	ID       string   // stable slug, used by progress files and tests
	Prose    []string // the task, one entry per rendered line
	Try      string   // optional literal to show as a starting point
	Hints    []string // revealed one at a time as attempts pile up
	Solution string   // canonical answer; must pass Verify (content_test)
	Verify   Verifier
}

// Lesson is one chapter: an ordered list of steps plus its display title.
type Lesson struct {
	ID    string
	Title string
	Steps []Step
}

// demoLesson is Phase 1's hardcoded chapter. Its job is to exercise every
// seam end to end — the loop interceptor, the output tee, both shipped
// verifier kinds, and the three languages-in-one-prompt ideas the real
// curriculum is built on (shell, Go, and the `$(...)` bridge between
// them). It is replaced by the embedded content files in Phase 3.
func demoLesson() Lesson {
	return Lesson{
		ID:    "demo",
		Title: "A three-step tour",
		Steps: []Step{
			{
				ID: "shell-echo",
				Prose: []string{
					"This is a real shell. Everything you already know works,",
					"unchanged — pipes, redirection, globs, quoting.",
					"",
					"Print the word hello.",
				},
				Try:      "echo hello",
				Hints:    []string{"Any command that prints `hello` counts — `echo hello` is the short way."},
				Solution: "echo hello",
				Verify:   MustVerifier("output-regexp (?m)^hello$"),
			},
			{
				ID: "go-expr",
				Prose: []string{
					"The same prompt is also Go. A line with Go syntax runs as Go —",
					"no mode switch, no subshell, no different language for scripts.",
					"",
					"Print 42, computed rather than typed.",
				},
				Try:      "fmt.Println(6 * 7)",
				Hints:    []string{"`fmt` is available at the prompt.", "Anything that evaluates to 42 works: `fmt.Println(6 * 7)`."},
				Solution: "fmt.Println(6 * 7)",
				Verify:   MustVerifier("output-regexp (?m)^42$"),
			},
			{
				ID: "bridge",
				Prose: []string{
					"And the two meet: `$(cmd)` runs a command and hands its",
					"trimmed output back to Go as a string.",
					"",
					"Print a command's output through Go — capture `echo bridge`",
					"and print it with fmt.",
				},
				Try:      `fmt.Println($(echo bridge))`,
				Hints:    []string{"`$(...)` is an expression here, not a string splice.", "Try `fmt.Println($(echo bridge))`."},
				Solution: `fmt.Println($(echo bridge))`,
				// Two clauses, conjoined: the mechanism AND the result.
				// Grading the output alone would tick over for a student who
				// typed `echo bridge`, which is the one thing this step is
				// not about.
				Verify: MustAll(`used-construct \$\(`, "output-regexp (?m)^bridge$"),
			},
		},
	}
}

// lessons is the chapter list `grsh tutor [chapter]` indexes into.
func lessons() []Lesson { return []Lesson{demoLesson()} }
