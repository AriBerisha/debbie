package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"debbie/internal/prompt"
)

// Shell runs commands for the shell tool.
type Shell struct {
	Path string // the program
	Kind string // bash, sh, powershell, pwsh or cmd
	Name string // how the prompt and the UI name it, such as "Git Bash"
	// Env is the environment commands get: the agent's own without secrets
	// (SPEC §3.5, see ChildEnv).
	Env []string
	// Timeout applies when a call names none.
	Timeout time.Duration
}

// MaxShellTimeout bounds the timeout a call may ask for.
const MaxShellTimeout = 600 * time.Second

// ShellResult is what a command did.
type ShellResult struct {
	Output   string // stdout and stderr in the order they came, cleaned up
	ExitCode int    // -1 when unknown
	TimedOut bool
}

// Run runs script in dir and waits until it ends, the timeout passes or
// ctx is done. A command that runs out of time or is cancelled is killed
// along with every process it started; so are processes still running
// when it ends, since v1 has no background processes (SPEC §8.1). When ctx
// is done, Run returns the output so far with ctx's error.
func (s *Shell) Run(ctx context.Context, script, dir string, timeout time.Duration) (ShellResult, error) {
	if timeout <= 0 {
		timeout = s.Timeout
	}
	r, w, err := os.Pipe()
	if err != nil {
		return ShellResult{}, err
	}
	cmd := s.command(script)
	cmd.Dir, cmd.Env = dir, s.Env
	cmd.Stdout, cmd.Stderr = w, w // one pipe keeps the two in order
	tree, err := startTree(cmd)
	w.Close()
	if err != nil {
		r.Close()
		return ShellResult{}, fmt.Errorf("can't start %s: %w", s.Name, err)
	}
	defer tree.close()

	var out capture
	copied := make(chan struct{})
	go func() {
		io.Copy(&out, r)
		close(copied)
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	res := ShellResult{ExitCode: -1}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	exited := false
	select {
	case <-waited:
		exited = true
	case <-timer.C:
		res.TimedOut = true
	case <-ctx.Done():
	}
	if !exited {
		tree.kill()
		select {
		case <-waited:
			exited = true
		case <-time.After(5 * time.Second): // the kill failed; don't hang
		}
	}
	tree.killRemaining()
	select {
	case <-copied:
	case <-time.After(2 * time.Second): // something escaped the kill and holds the pipe
	}
	r.Close()
	if exited {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	res.Output = out.text()
	if !res.TimedOut && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

// Limits on the output a command may leave in memory: its start and its
// end are kept, the middle is counted.
const (
	captureHead = 512 << 10
	captureTail = 512 << 10
)

// capture keeps the start and the end of a command's output.
type capture struct {
	mu    sync.Mutex
	head  []byte
	tail  []byte // holds at least the last captureTail bytes after head
	total int64
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	c.total += int64(n)
	if room := captureHead - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if len(c.tail) > 2*captureTail {
			c.tail = append(c.tail[:0], c.tail[len(c.tail)-captureTail:]...)
		}
	}
	return n, nil
}

// text returns the output decoded and cleaned up, with a marker where the
// middle was left out.
func (c *capture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	head, tail := c.head, c.tail
	if len(tail) > captureTail {
		tail = tail[len(tail)-captureTail:]
	}
	omitted := c.total - int64(len(head)) - int64(len(tail))
	if omitted == 0 {
		return cleanOutput(decodeOutput(append(append([]byte(nil), head...), tail...)))
	}
	// Don't let the cuts split a character, which would make valid UTF-8
	// look like another encoding.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	if i := lastRuneStart(head); i >= 0 && !utf8.FullRune(head[i:]) {
		head = head[:i]
	}
	return cleanOutput(decodeOutput(head)) +
		fmt.Sprintf("\n[... %s of output left out ...]\n", sizeText(omitted)) +
		cleanOutput(decodeOutput(tail))
}

func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return -1
}

// ansiRE matches terminal escape sequences: CSI (colors, cursor movement),
// OSC (titles, links) and two-byte escapes.
var ansiRE = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

// cleanOutput removes escape sequences and what carriage returns overwrite,
// such as the steps of a progress bar, and trailing blank space.
func cleanOutput(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.Contains(s, "\r") {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			l = strings.TrimRight(l, "\r")
			if j := strings.LastIndexByte(l, '\r'); j >= 0 {
				l = l[j+1:]
			}
			lines[i] = l
		}
		s = strings.Join(lines, "\n")
	}
	return strings.TrimRight(s, " \t\n")
}

// ShellTool runs commands (SPEC §8.1).
type ShellTool struct{ Shell *Shell }

func (t ShellTool) Name() string        { return "shell" }
func (t ShellTool) Kind() Kind          { return Execute }
func (t ShellTool) Description() string { return prompt.ShellTool(t.Shell.Name, t.Shell.Kind) }

func (t ShellTool) Params() []Param {
	return []Param{
		{Name: "command", Type: "string", Required: true, Description: "The command to run.", Aliases: []string{"cmd", "script"}},
		{Name: "timeout_seconds", Type: "integer", Aliases: []string{"timeout"},
			Description: fmt.Sprintf("How long it may run, at most %d seconds; %d when omitted.", int(MaxShellTimeout/time.Second), int(t.Shell.Timeout/time.Second))},
		{Name: "directory", Type: "string", Aliases: []string{"cwd", "workdir", "dir_path"},
			Description: "A directory inside the workspace to run in; the workspace root when omitted."},
	}
}

func (t ShellTool) Prepare(_ context.Context, env *Env, args Args) (*Call, error) {
	command := args.String("command")
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("the command is empty")
	}
	dir, shownDir := env.Workspace.Root(), ""
	if d := args.String("directory"); d != "" && d != "." {
		abs, inside, err := env.Workspace.Resolve(d)
		if err != nil {
			return nil, err
		}
		if !inside {
			return nil, fmt.Errorf("%s is outside the workspace; commands run inside it", abs)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", env.Workspace.Rel(abs))
		}
		if rel := env.Workspace.Rel(abs); rel != "." {
			dir, shownDir = abs, rel
		}
	}
	timeout := t.Shell.Timeout
	if args.Has("timeout_seconds") {
		n := args.Int("timeout_seconds", 0)
		if n > int(MaxShellTimeout/time.Second) {
			n = (n + 999) / 1000 // milliseconds, which other agents' shell tools take
		}
		if n > 0 {
			timeout = min(time.Duration(n)*time.Second, MaxShellTimeout)
		}
	}

	summary := "shell " + oneLine(command, 100)
	preview := command
	if shownDir != "" {
		summary += " (in " + shownDir + ")"
		preview += "\n(in " + shownDir + ")"
	}
	call := &Call{Kind: Execute, Summary: summary, Preview: preview, SessionKey: "shell:" + shownDir + "\x00" + command}
	call.Run = func(ctx context.Context) (Result, error) {
		res, err := t.Shell.Run(ctx, command, dir, timeout)
		if err != nil && ctx.Err() == nil {
			return Result{}, err
		}
		output := res.Output
		lines := countLines(output)
		if output == "" {
			output = "(no output)"
		}
		switch {
		case err != nil: // cancelled; the caller reports that
			return Result{Output: output}, err
		case res.TimedOut:
			secs := int(timeout / time.Second)
			return Result{
				Output:  fmt.Sprintf("%s\n[stopped after %d seconds: the command ran out of time, so it and every process it started were killed]", output, secs),
				Display: fmt.Sprintf("timed out after %ds", secs),
				Warn:    true,
			}, nil
		}
		display := fmt.Sprintf("exit %d", res.ExitCode)
		if lines > 0 {
			display += " · " + Lines(lines)
		}
		return Result{
			Output:  fmt.Sprintf("%s\nExit code: %d", output, res.ExitCode),
			Display: display,
			Warn:    res.ExitCode != 0,
		}, nil
	}
	return call, nil
}

// oneLine shortens text to its first line and at most n characters.
func oneLine(s string, n int) string {
	first, rest, more := strings.Cut(strings.TrimSpace(s), "\n")
	first = strings.TrimRight(first, "\r")
	if utf8.RuneCountInString(first) > n {
		return firstRunes(first, n) + "…"
	}
	if more && strings.TrimSpace(rest) != "" {
		return first + " …"
	}
	return first
}
