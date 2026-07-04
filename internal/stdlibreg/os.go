package stdlibreg

import "os"

func init() {
	Register(&Package{Name: "os", Symbols: map[string]any{
		"Getenv":      os.Getenv,
		"Setenv":      os.Setenv,
		"Unsetenv":    os.Unsetenv,
		"Environ":     os.Environ,
		"Getwd":       os.Getwd,
		"Hostname":    os.Hostname,
		"UserHomeDir": os.UserHomeDir,
		"TempDir":     os.TempDir,
		"MkdirAll":    func(path string) error { return os.MkdirAll(path, 0755) },
		"Mkdir":       func(path string) error { return os.Mkdir(path, 0755) },
		"Remove":      os.Remove,
		"RemoveAll":   os.RemoveAll,
		"Rename":      os.Rename,
		"ReadFile": func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		"WriteFile": func(path, data string) error {
			return os.WriteFile(path, []byte(data), 0644)
		},
		"Getpid": os.Getpid,
	}})
}
