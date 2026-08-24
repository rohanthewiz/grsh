package zshimport

import (
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	src := `# my zshrc
export EDITOR=vim
alias gs='git status'
alias weird='ls | wc -l'
alias -g G='| grep'
setopt AUTO_CD
bindkey -e
PATH=$HOME/bin:$PATH
path+=(/opt/tools/bin)
FOO=bar
source $ZSH/oh-my-zsh.sh
eval "$(starship init zsh)"
greet() {
  echo "hi $1"
}
if [[ -d ~/x ]]; then
  export HAS_X=1
fi
mkdir -p ~/scratch
`
	r := Translate(src)
	out := r.Output

	active := []string{
		"export EDITOR=vim",
		"alias gs='git status'",
		"export PATH=$HOME/bin:$PATH",
		`export PATH="$PATH:/opt/tools/bin"`,
		"mkdir -p ~/scratch",
	}
	for _, want := range active {
		if !hasActiveLine(out, want) {
			t.Errorf("missing active line %q in:\n%s", want, out)
		}
	}

	commented := []string{
		"alias weird=", "alias -g", "setopt", "bindkey",
		"source $ZSH", "eval ", "greet()", "if [[", "FOO=bar",
	}
	for _, frag := range commented {
		if hasActiveLine(out, frag) {
			t.Errorf("zsh-specific line %q leaked as active in:\n%s", frag, out)
		}
		if !strings.Contains(out, frag) {
			t.Errorf("line %q was dropped entirely (should be preserved as a comment)", frag)
		}
	}

	if r.Todos < 2 { // function + eval at minimum
		t.Errorf("Todos = %d, want >= 2", r.Todos)
	}
	if !strings.Contains(out, "port function \"greet\"") {
		t.Errorf("function porting hint missing:\n%s", out)
	}
}

// hasActiveLine reports whether out contains a non-comment line that
// contains frag.
func hasActiveLine(out, frag string) bool {
	for _, ln := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(ln, frag) {
			return true
		}
	}
	return false
}
