package stdlibreg

import "path/filepath"

func init() {
	Register(&Package{Name: "filepath", Symbols: map[string]any{
		"Join":  filepath.Join,
		"Base":  filepath.Base,
		"Dir":   filepath.Dir,
		"Ext":   filepath.Ext,
		"Abs":   filepath.Abs,
		"Clean": filepath.Clean,
		"Rel":   filepath.Rel,
		"Glob":  filepath.Glob,
		"Match": filepath.Match,
	}})
}
