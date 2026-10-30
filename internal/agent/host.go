package agent

import (
	"context"
	"time"

	"debbie/internal/provider"
	"debbie/internal/tools"
)

// Host is the user's side of a session: it shows what happens and answers
// approval prompts. The terminal UI implements it.
type Host interface {
	// ReplyStart, Event and ReplyEnd show one streamed reply of the model.
	ReplyStart()
	Event(provider.Event)
	ReplyEnd()
	// Approve asks the user whether a call may run. An error, such as a
	// cancelled context or the end of the input, refuses the call and ends
	// the turn.
	Approve(ctx context.Context, run *ToolRun) (Approval, error)
	// ToolStart shows a call about to run and ToolEnd how it ended. A call
	// that fails or is refused before it runs gets only ToolEnd.
	ToolStart(run *ToolRun)
	ToolEnd(run *ToolRun)
}

// RunStatus is how a tool call ended.
type RunStatus int

const (
	RunOK        RunStatus = iota
	RunFailed              // the call was invalid or the tool reported an error
	RunDenied              // the user said no
	RunCancelled           // the user pressed Ctrl+C or left the prompt
)

// ToolRun describes a tool call to the host.
type ToolRun struct {
	Name    string
	Kind    tools.Kind
	Summary string   // one line, such as "edit src/main.go (+1 -1)"
	Preview string   // what an approval prompt shows: a diff or a command
	Outside []string // paths outside the workspace that the call touches
	Asked   bool     // the user approved it at a prompt
	// Agent is the subagent making the call, by its description, and
	// AgentID tells it from others; empty and 0 for the main agent.
	// Approval prompts name it.
	Agent   string
	AgentID int
	// Question replaces the approval prompt's question, and SessionFor
	// names what its "yes for this session" covers ("" for no such
	// answer); see tools.Call.Ask. Tainted says the call asks because web
	// content is in the conversation.
	Question   string
	SessionFor string
	Tainted    bool

	// Set when the call has ended.
	Status  RunStatus
	Display string // the result in brief, such as "120 lines"
	Error   string // what went wrong, as the model was told
	Warn    bool   // it ran, but something went wrong, such as a failed command
	Elapsed time.Duration
	Output  string // what the model was told, capped and with secrets removed
}

// Approval is the user's answer to an approval prompt.
type Approval struct {
	Allow bool
	// Session allows calls with the same session key for the rest of the
	// session without asking: the same file for edits, the same command for
	// the shell.
	Session bool
	// Note, when Allow is false, is passed to the model, which carries on.
	// A refusal without a note ends the turn.
	Note string
}
