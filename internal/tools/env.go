package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// minEnvSecret is the shortest secret that ChildEnv looks for in values.
// Shorter ones could match unrelated values, such as part of PATH.
const minEnvSecret = 16

// childSettings keep commands from waiting for a pager or a password.
var childSettings = [][2]string{
	{"GIT_PAGER", "cat"},
	{"PAGER", "cat"},
	{"GIT_TERMINAL_PROMPT", "0"},
}

// ChildEnv returns the environment for processes the agent starts (SPEC
// §3.5): env without the variables named in strip and without any whose
// value holds one of the secrets, plus settings that keep commands from
// waiting for a pager or a password.
func ChildEnv(env, strip, secrets []string) []string {
	drop := map[string]bool{}
	for _, name := range strip {
		drop[envKey(name)] = true
	}
	for _, kv := range childSettings {
		drop[envKey(kv[0])] = true
	}
	var out []string
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "" { // Windows keeps per-drive directories as "=C:=C:\dir"
			out = append(out, kv)
			continue
		}
		if drop[envKey(name)] || holdsSecret(value, secrets) {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range childSettings {
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
}

func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name) // Windows environment names ignore case
	}
	return name
}

func holdsSecret(value string, secrets []string) bool {
	for _, s := range secrets {
		if len(s) >= minEnvSecret && strings.Contains(value, s) {
			return true
		}
	}
	return false
}

// GitStatus returns the short status of the repository holding dir, branch
// first, at most maxLines lines; "" when git isn't installed or dir isn't
// in a repository. The repository's fsmonitor hook is turned off, since
// the repository's own config is not to be trusted with running commands.
func GitStatus(ctx context.Context, dir string, env []string, maxLines int) string {
	git, err := exec.LookPath("git")
	if err != nil {
		return "" // includes a git.exe planted in the current directory
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-c", "core.fsmonitor=false", "-c", "core.quotepath=false",
		"--no-optional-locks", "status", "--short", "--branch")
	cmd.Dir, cmd.Env = dir, env
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], fmt.Sprintf("... and %d more", len(lines)-maxLines))
	}
	return strings.Join(lines, "\n")
}

// TopLevel lists the entries of the workspace root that the ignore rules
// keep, directories with a trailing slash, at most max of them.
func (w *Workspace) TopLevel(max int) []string {
	var names []string
	more := 0
	w.walker().walk(context.Background(), w.root, func(_, rel string, d fs.DirEntry) error {
		switch {
		case len(names) >= max:
			more++
		case d.IsDir():
			names = append(names, rel+"/")
		default:
			names = append(names, rel)
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	if more > 0 {
		names = append(names, fmt.Sprintf("... and %d more", more))
	}
	return names
}
