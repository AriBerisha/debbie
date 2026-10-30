package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"debbie/internal/app"
	"debbie/internal/config"
)

// dataDir is where the extension and the host program go: %LOCALAPPDATA%
// on Windows, Application Support on macOS, $XDG_DATA_HOME (or
// ~/.local/share) elsewhere.
func dataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" && filepath.IsAbs(d) {
			return filepath.Join(d, app.Name), nil
		}
		return "", errors.New("LOCALAPPDATA isn't set")
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", app.Name), nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, app.Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", app.Name), nil
}

// RunDir is the private directory of the bridge's socket and token:
// $XDG_RUNTIME_DIR on Linux when set, else under the data directory.
func RunDir() (string, error) {
	if runtime.GOOS == "linux" {
		if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && filepath.IsAbs(d) {
			return filepath.Join(d, app.Name), nil
		}
	}
	d, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "run"), nil
}

// chromeHostsDir is where Chrome looks for a user's native messaging host
// manifests on Linux and macOS; on Windows the registry says where.
func chromeHostsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts"), nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "google-chrome", "NativeMessagingHosts"), nil
}

// keyPath is the extension's private key, in the config directory.
func keyPath() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "browser-extension-key.pem"), nil
}
