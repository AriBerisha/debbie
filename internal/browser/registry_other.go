//go:build !windows

package browser

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// On Linux and macOS, Chrome finds a user's native messaging host by its
// manifest's place: <name>.json in its NativeMessagingHosts directory.

func registerHost(name, manifest string) error {
	dir, err := chromeHostsDir()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644)
}

func unregisterHost(name string) error {
	dir, err := chromeHostsDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, name+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// registeredHost returns the path of the manifest Chrome finds; "" for
// none.
func registeredHost(name string) (string, error) {
	dir, err := chromeHostsDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".json")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return path, nil
}

// hostRegistration says where the host is registered, for messages.
func hostRegistration(name string) string {
	dir, err := chromeHostsDir()
	if err != nil {
		return "Chrome's NativeMessagingHosts directory"
	}
	return filepath.Join(dir, name+".json")
}
