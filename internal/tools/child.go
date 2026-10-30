package tools

import "os/exec"

// Child is a long-running program the agent starts, such as an MCP server.
// It runs the way commands do: in its own hidden console on Windows and its
// own session elsewhere, so Ctrl+C in the terminal doesn't reach it and it
// can't draw over the agent's output; and Kill ends it together with
// everything it started.
type Child struct {
	Cmd  *exec.Cmd
	tree *procTree
}

// NewChild prepares the program at path to run as a Child. The caller sets
// its environment, directory and pipes, then calls StartChild.
func NewChild(path string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	childAttr(cmd)
	return cmd
}

// StartChild starts a command made by NewChild.
func StartChild(cmd *exec.Cmd) (*Child, error) {
	t, err := startTree(cmd)
	if err != nil {
		return nil, err
	}
	return &Child{Cmd: cmd, tree: t}, nil
}

// Kill ends the program and everything it started.
func (c *Child) Kill() { c.tree.kill() }

// Close releases what the child holds after it has exited; on Windows that
// ends anything it left running.
func (c *Child) Close() { c.tree.close() }
