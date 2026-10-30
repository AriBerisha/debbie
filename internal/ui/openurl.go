package ui

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// OpenURL opens an https page in the user's browser, such as the one where
// Claude API keys are made. env is the launcher's environment, without the
// API keys (tools.ChildEnv). Only https pages are opened: a URL is never a
// way to start some other program.
func OpenURL(page string, env []string) error {
	u, err := url.Parse(page)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("only https pages are opened")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		cmd = exec.Command(filepath.Join(root, "System32", "rundll32.exe"), "url.dll,FileProtocolHandler", u.String())
	case "darwin":
		cmd = exec.Command("open", u.String())
	default:
		cmd = exec.Command("xdg-open", u.String())
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
