package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Save validates cfg and writes it to path atomically: the content goes to a
// new owner-only file in the same directory, which is flushed to disk and
// then renamed over path, so readers see the old file or the new one and
// never a partial write. A missing directory is created, private to the
// user. When path is a symbolic link, the file it points to is replaced.
// Settings left at zero get their defaults.
func Save(path string, cfg *Config) error {
	cfg.applyDefaults()
	if _, err := cfg.Validate(); err != nil {
		return fmt.Errorf("not saving an invalid config: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := WritePrivate(path, append(data, '\n')); err != nil {
		return fmt.Errorf("saving the config: %w", err)
	}
	return nil
}

// WritePrivate writes data to path the way Save writes the config:
// atomically, to an owner-only file, creating a missing directory private
// to the user.
func WritePrivate(path string, data []byte) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	dir := filepath.Dir(target)
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("creating the directory: %w", err)
	}
	tmp, err := createTemp(dir, filepath.Base(target))
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmp.Name(), target); err != nil {
		return err
	}
	done = true
	syncDir(dir) // best effort: the rename has happened either way
	return nil
}

// ensurePrivateDir creates dir, private to the user, if it doesn't exist.
// An existing directory is left as it is.
func ensurePrivateDir(dir string) error {
	fi, err := os.Stat(dir)
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := mkdirPrivate(dir); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// createTemp creates a new private file with a random name in dir.
func createTemp(dir, base string) (*os.File, error) {
	for range 10 {
		var b [6]byte
		rand.Read(b[:])
		f, err := createPrivateFile(filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(b[:])))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, errors.New("could not create a temporary file")
}

// PrivateDir makes sure dir exists and only the user may use it, creating
// it, and a missing parent, the way WritePrivate does. A directory that
// other users may read or change is an error.
func PrivateDir(dir string) error {
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	warning, err := checkPrivate(dir)
	if err != nil {
		return err
	}
	if warning != "" {
		return errors.New(warning)
	}
	return nil
}
