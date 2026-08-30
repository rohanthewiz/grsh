# Writing a tutor chapter

Chapters are data. Adding or rewriting one never means touching engine
code — that is why they live here as markdown rather than as Go structs.

Files are named `NN-slug.md`. The number is the chapter order (`fs.Glob`
sorts, and the numbers are zero-padded, so there is no index to keep in
sync) and `NN-slug` is the **lesson ID** that progress records are keyed
by. Renaming a file resets that chapter's saved place, which is the
honest outcome: its steps were renumbered too.

Only `[0-9]*.md` is embedded, so this guide is not a chapter.

## Shape

```markdown
# Chapter title

Anything before the first step is a note to whoever edits the file. The
engine has nowhere to show it, so it is never rendered.

## step: stable-id
Prose. Rendered verbatim in the panel above the prompt, blank lines and
all, with `backticked spans` picked out in the code colour.
---
try: an optional literal shown under the prose as a starting point
verify: output-regexp (?m)^42$
hint: revealed one at a time, after two misses or on `:hint`
hint: a second hint, revealed after the first
solution: fmt.Println(6 * 7)
```

Prose runs from the `## step:` heading to the `---`; directives follow.
A step needs at least one `verify:` line and — because the content
self-check runs it — a `solution:`.

Repeat a directive rather than reaching for nested syntax:

- several `hint:` lines are the ordered hint list;
- several `verify:` lines are **conjoined** — every one must pass.

A directive with an empty value opens an indented block, which is how a
multi-line answer survives the format. The block is dedented by its
first line's indent, so the file stays readable while the student sees
the answer at column zero:

```markdown
solution:
  for _, f := range glob("*.go") {
      fmt.Println(f)
  }
```

## Verifier kinds

| kind | argument | grades |
|---|---|---|
| `any-input` | — | anything: demo/observe steps |
| `output-regexp` | regexp | the attempt's captured output, trimmed |
| `output-exact` | literal | the trimmed output, byte for byte |
| `status` | `N` or `nonzero` | `status()` after the attempt |
| `var` | `name [type=RE] [value=RE]` | a live Go binding |
| `file` | `path [contains=RE]` | the sandbox on disk |
| `classified-as` | `shell` or `go` | how grsh *read* the line |
| `used-construct` | regexp | the student's input |

Everything after the kind is that kind's argument, trimmed. Patterns are
Go regexps, so escape what you mean literally: `used-construct \$\(`.

### Choosing one

Prefer `var`, `file` and `status` over output matching where a step
gives you the choice: they do not depend on `ls` ordering, locale, or a
BSD/GNU difference in a tool's spacing. Reach for `output-exact` only
when the exact bytes *are* the lesson.

`used-construct` on its own passes any line that merely contains the
pattern, so it is nearly always conjoined with a result check. That pair
— mechanism AND result — is what stops "capture it with `$(...)`" from
ticking over for a student who typed the answer literally.

### Anchor your `var` predicates

`type=` and `value=` are unanchored regexps matched against raw strings,
so `type=int` also matches `interface{}` and `value=1` matches `17`.
Write `type=^int$` and `value=^17$` unless you mean otherwise —
`type=^\[\]string$` needs the same care.

`var` predicates are whitespace-separated, so a pattern that has to match
a space writes `\s`: `value=^old\slogs$`.

The strings come from `Session.VarInfo`, not from the rendered `?name`
line, so they are never truncated and a string's value is its contents
(`value=^ada$`, no quotes).

## Fixtures

The playground is built by `sandbox.go`, and its numbers are part of the
curriculum: three top-level `.go` files, a 120-line `access.log` holding
17 `500`s and 9 `404`s, a `notes/` subtree, and an `old logs/` directory
whose name contains a space. Change a fixture and the step that grades
against it in the same commit — `TestSandboxFixtureCounts` exists to
catch the half of that pair you forget.

## The check that matters

`TestContentSolutionsPass` runs every chapter's solutions **in order, in
one session, in a fresh playground**, exactly as a student would, and
requires each to satisfy its own step. Steps may therefore build on each
other — chapter 4 captures into a variable chapter 4 later splices — and
a step whose canonical answer no longer works fails CI rather than a
student.
