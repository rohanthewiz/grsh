package shellexec

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

var builtinNames = map[string]bool{
	"cd": true, "export": true, "unset": true, "exit": true,
	"alias": true, "unalias": true, "source": true, ".": true,
}

func isBuiltin(name string) bool { return builtinNames[name] }

func runBuiltin(st *State, name string, args []string, stdio Stdio) (int, error) {
	switch name {
	case "cd":
		return builtinCd(args, stdio)
	case "export":
		return builtinExport(args, stdio)
	case "unset":
		for _, a := range args {
			_ = os.Unsetenv(a)
		}
		return 0, nil
	case "exit":
		code := 0
		if len(args) > 0 {
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return userErr(stdio, serr.New("exit: numeric argument required", "got", args[0]))
			}
			code = n
		}
		return code, ExitErr{Code: code}
	case "alias":
		return builtinAlias(st, args, stdio)
	case "unalias":
		for _, a := range args {
			delete(st.Aliases, a)
		}
		return 0, nil
	case "source", ".":
		return builtinSource(st, args, stdio)
	}
	return userErr(stdio, serr.New("unknown builtin", "name", name))
}

func builtinCd(args []string, stdio Stdio) (int, error) {
	var dir string
	switch {
	case len(args) == 0:
		home, err := os.UserHomeDir()
		if err != nil {
			return userErr(stdio, serr.Wrap(err, "op", "cd"))
		}
		dir = home
	case args[0] == "-":
		dir = os.Getenv("OLDPWD")
		if dir == "" {
			return userErr(stdio, serr.New("cd: OLDPWD not set"))
		}
		fmt.Fprintln(stdio.Out, dir)
	default:
		dir = args[0]
	}
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(stdio.Err, "grsh: cd: %s: %s\n", dir, cdReason(err))
		return 1, nil
	}
	now, _ := os.Getwd()
	_ = os.Setenv("OLDPWD", prev)
	_ = os.Setenv("PWD", now)
	return 0, nil
}

func cdReason(err error) string {
	if os.IsNotExist(err) {
		return "no such file or directory"
	}
	if os.IsPermission(err) {
		return "permission denied"
	}
	return err.Error()
}

func builtinExport(args []string, stdio Stdio) (int, error) {
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if k == "" {
			return userErr(stdio, serr.New("export: invalid name", "arg", a))
		}
		if ok {
			_ = os.Setenv(k, v)
		}
		// `export NAME` without a value: env vars are always exported
		// here (we use the real process environment), so it's a no-op.
	}
	return 0, nil
}

func builtinAlias(st *State, args []string, stdio Stdio) (int, error) {
	if len(args) == 0 {
		names := make([]string, 0, len(st.Aliases))
		for k := range st.Aliases {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(stdio.Out, "alias %s='%s'\n", k, st.Aliases[k])
		}
		return 0, nil
	}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			if v, exists := st.Aliases[k]; exists {
				fmt.Fprintf(stdio.Out, "alias %s='%s'\n", k, v)
				continue
			}
			return userErr(stdio, serr.New("alias: not found", "name", k))
		}
		st.Aliases[k] = v
	}
	return 0, nil
}

func builtinSource(st *State, args []string, stdio Stdio) (int, error) {
	if len(args) == 0 {
		return userErr(stdio, serr.New("source: filename argument required"))
	}
	if st.SourceFn == nil {
		return userErr(stdio, serr.New("source: not available in this context"))
	}
	if err := st.SourceFn(args[0]); err != nil {
		if _, isExit := err.(ExitErr); isExit {
			return 0, err // exit inside a sourced file exits the script
		}
		return 1, err
	}
	return st.LastStatus, nil
}
