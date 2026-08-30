# Helpers and the registry

Two surfaces are pre-declared in every script and every prompt: a short
list of shell-shaped helpers (`glob`, `lines`, `fields`, `readFile`,
`trim`, `exists`, `env`, `pwd`, …) and a curated set of real Go packages.
No imports needed — `import "strings"` is accepted and validated, but
optional.

While you type a registry call, the dim line under the input shows its
signature, reflected from the registry itself so it can never go stale.

## step: glob-helper
`glob(pat) []string` is filename expansion from the Go side, with the
same patterns the shell uses.

The `notes/` directory holds markdown files (and an `archive/`
subdirectory). Print how many `.md` files are directly inside it.
---
verify: used-construct glob\(
verify: output-regexp (?m)^2$
hint: `len` of the glob result.
hint: `fmt.Println(len(glob("notes/*.md")))`
solution: fmt.Println(len(glob("notes/*.md")))

## step: lines
`readFile(p) (string, error)` reads a whole file; `lines(s)` splits on
newlines and ignores a trailing one, so it counts what `wc -l` counts.

Print the number of lines in `access.log` without leaving Go.
---
verify: used-construct lines\(
verify: output-regexp (?m)^120$
hint: Nest them: `lines(readFile("access.log"))`, then `len`.
hint: `fmt.Println(len(lines(readFile("access.log"))))`
solution: fmt.Println(len(lines(readFile("access.log"))))

## step: fields
`fields(s)` splits on any whitespace — awk's `$1` without awk.

Print the client address from the log's first line again, this time
from Go, capturing the command with `$(...)`.
---
verify: used-construct fields\(
verify: output-exact 10.0.0.1
hint: `fields(...)` returns a `[]string`; index it.
hint: `fmt.Println(fields($(head -1 access.log))[0])`
solution: fmt.Println(fields($(head -1 access.log))[0])

## step: writefile
`writeFile(p, s) error` and `appendFile(p, s) error` are the writing
half. Both take a plain string.

Write the line `errors: 17` into `summary.txt`.
---
verify: used-construct writeFile\(
verify: file summary.txt contains=errors: 17
hint: `writeFile("summary.txt", "errors: 17\n")`
solution: writeFile("summary.txt", "errors: 17\n")

## step: json
`json` is adapted for scripting, since scripts have no pointers:
`json.Parse(s) (any, error)` replaces `Unmarshal`, and
`json.Marshal`/`MarshalIndent` return strings.

`data.json` holds one small object. Parse it and print the result.
---
verify: used-construct json\.Parse
verify: output-regexp service:api
hint: `readFile` gives you the text; `json.Parse` gives you the value.
hint: `fmt.Println(json.Parse(readFile("data.json")))`
solution: fmt.Println(json.Parse(readFile("data.json")))

## step: strings-pkg
Registry packages and helpers mix freely.

Print `DONE` by trimming the spaces from `"  done  "` and upper-casing
what is left.
---
verify: used-construct strings\.
verify: output-exact DONE
hint: `trim` is the helper; `strings.ToUpper` is the package call.
hint: `fmt.Println(strings.ToUpper(trim("  done  ")))`
solution: fmt.Println(strings.ToUpper(trim("  done  ")))
