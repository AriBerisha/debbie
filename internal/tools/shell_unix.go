//go:build !windows

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// FindShell picks the program that runs commands, from the config's
// shell.program: "" or "auto" for /bin/sh (SPEC §8.1); "sh", "bash" or
// "pwsh"; or the full path of a shell.
func FindShell(program string) (*Shell, error) {
	switch program {
	case "", "auto", "sh":
		return &Shell{Path: "/bin/sh", Kind: "sh", Name: "sh"}, nil
	case "bash", "pwsh":
		p, err := exec.LookPath(program)
		if err != nil {
			return nil, fmt.Errorf("shell.program is %q, but it isn't on PATH", program)
		}
		return &Shell{Path: p, Kind: program, Name: program}, nil
	case "powershell", "cmd":
		return nil, fmt.Errorf("shell.program %q exists only on Windows", program)
	}
	if !filepath.IsAbs(program) {
		return nil, fmt.Errorf("shell.program %q: use auto, sh, bash, pwsh or a full path", program)
	}
	if fi, err := os.Stat(program); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("shell.program %s: no such program", program)
	}
	switch base := filepath.Base(program); {
	case base == "pwsh":
		return &Shell{Path: program, Kind: "pwsh", Name: "pwsh"}, nil
	case base == "bash":
		return &Shell{Path: program, Kind: "bash", Name: "bash"}, nil
	case strings.HasSuffix(base, "sh"): // sh, dash, zsh, ksh
		return &Shell{Path: program, Kind: "sh", Name: base}, nil
	}
	return nil, fmt.Errorf("shell.program %s: not a shell this agent knows", program)
}

// command runs the script in a new session, so it has no controlling
// terminal: Ctrl+C in the user's terminal doesn't reach it, and a program
// that asks for a password on the terminal fails instead of waiting.
func (s *Shell) command(script string) *exec.Cmd {
	var cmd *exec.Cmd
	if s.Kind == "pwsh" {
		cmd = exec.Command(s.Path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	} else {
		cmd = exec.Command(s.Path, "-c", script)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// procTree is a started command and the processes in its process group.
type procTree struct{ pgid int }

func startTree(cmd *exec.Cmd) (*procTree, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procTree{pgid: cmd.Process.Pid}, nil
}

func (t *procTree) kill()          { syscall.Kill(-t.pgid, syscall.SIGKILL) }
func (t *procTree) killRemaining() { syscall.Kill(-t.pgid, syscall.SIGKILL) }
func (t *procTree) close()         {}

func decodeOutput(b []byte) string {
	return strings.ToValidUTF8(string(b), string(utf8.RuneError))
}

// hideConsole is for Windows.
func hideConsole(*exec.Cmd) {}

// childAttr starts a Child in a new session, without a controlling
// terminal, as commands start.
func childAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
