package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"debbie/internal/app"
)

// Path returns the config file to use: the --config flag value when given,
// then $DEBBIE_CONFIG (or the former $DEBBIE_CLI_CONFIG), then DefaultPath.
func Path(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	for _, name := range append([]string{app.EnvVar("CONFIG")}, app.FormerEnvVars("CONFIG")...) {
		if v := os.Getenv(name); v != "" {
			return filepath.Abs(v)
		}
	}
	return DefaultPath()
}

// DefaultPath is config.json in Dir.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Dir returns the default config directory: %APPDATA%\debbie on
// Windows, and $XDG_CONFIG_HOME/debbie or ~/.config/debbie elsewhere,
// macOS included.
func Dir() (string, error) {
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, app.Name), nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, app.Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("can't find the home directory for the config file; set " + app.EnvVar("CONFIG"))
	}
	return filepath.Join(home, ".config", app.Name), nil
}

// TakeOver moves the config directory of a former name of the program
// (app.FormerNames) to Dir, when Dir doesn't exist yet, so that a renamed
// program keeps its user's providers, history and browser extension key.
// A file in use stops the move on Windows; then it copies. It returns the
// directory taken over, or "" when there was none.
func TakeOver() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	for _, name := range app.FormerNames {
		former := filepath.Join(filepath.Dir(dir), name)
		if fi, err := os.Stat(former); err != nil || !fi.IsDir() {
			continue
		}
		if os.Rename(former, dir) == nil {
			return former, nil
		}
		if err := copyDir(former, dir); err != nil {
			return "", err
		}
		return former, nil
	}
	return "", nil
}

// copyDir copies a directory's files, keeping their permissions.
func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
