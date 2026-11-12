package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"debbie/internal/agent"
	"debbie/internal/browser"
	"debbie/internal/config"
	"debbie/internal/cost"
	"debbie/internal/effort"
	"debbie/internal/mcp"
	"debbie/internal/provider"
	"debbie/internal/sessions"
	"debbie/internal/setup"
	"debbie/internal/tools"
)

// Exit codes Chat returns.
const (
	ExitOK          = 0
	ExitInterrupted = 130
)

// ChatOptions configures Chat.
type ChatOptions struct {
	Session *agent.Session
	// Console is the terminal to use. Without one, Chat makes its own from
	// Terminal, In, Out and Redactor.
	Console  *Console
	Terminal Terminal
	In       io.Reader
	Out      io.Writer
	Redactor *config.Redactor // applied to everything written to Out
	Status   StatusInfo
	// Workspace is the directory the agent works in, for the banner.
	Workspace string
	// EffortLevels are the reasoning-effort levels the model supports,
	// weakest first; nil when unknown.
	EffortLevels []effort.Level
	// Providers manages the providers in the config file, for /provider.
	Providers *setup.Manager
	// Use switches the session to the named provider, for /provider use.
	Use func(ctx context.Context, name string) (*setup.Conn, error)
	// Models lists the models the provider in use offers, and SetModel
	// switches the session to one of them, returning the connection as it
	// is then and a warning, if any; both for /model. Nil when the session
	// can't switch models.
	Models   func(ctx context.Context) ([]provider.ModelInfo, error)
	SetModel func(ctx context.Context, model string) (conn *setup.Conn, warning string, err error)
	// Notices and Warnings are shown under the banner, which starts with a
	// cleared screen. A notice starting with "✓ " reports something done;
	// the check is drawn in a symbol the terminal's font has.
	Notices, Warnings []string
	// ConfirmYolo asks the user to confirm the session's yolo mode before
	// the first message; without a yes, the mode becomes FallbackMode.
	ConfirmYolo  bool
	FallbackMode string
	// Classic keeps an interactive chat in line mode instead of the
	// fullscreen UI.
	Classic bool
	// AutoCompact is the context size, in tokens, at which the conversation
	// is compacted after a turn; 0 never.
	AutoCompact int
	// MCP runs the configured MCP servers, if there are any.
	MCP *mcp.Manager
	// Files is the workspace, for @path completion and attachments.
	Files *tools.Workspace
	// ChildEnv, if set, is the environment for programs the UI starts
	// itself, such as a clipboard program: the one commands get, without
	// API keys.
	ChildEnv func() []string
	// History holds the messages sent in earlier sessions, oldest first,
	// for ↑ in the fullscreen UI; SaveHistory, if set, keeps the list after
	// each message.
	History     []string
	SaveHistory func([]string)
	// Web is the session's web access, for /web; nil without
	// tools. ConfirmWebYolo asks the user to confirm web yolo before the
	// first message; without a yes, web access becomes safe.
	Web            *browser.Access
	ConfirmWebYolo bool
	// Sessions saves the conversation as it goes, for /resume; nil saves
	// nothing. Resume, when set, is a saved session to resume at the start
	// (--continue).
	Sessions *sessions.Recorder
	// Prices price what the session spends (/cost); nil shows no cost.
	Prices *cost.Table
	Resume string
}

// Chat runs the conversation until the input ends, the user quits, or
// Ctrl+C is pressed twice within a second. Ctrl+C once cancels the reply or
// tool in progress, or the message being typed. It returns the exit code.
func Chat(opts ChatOptions) int {
	con := opts.Console
	if con == nil {
		con = NewConsole(opts.Terminal, opts.In, opts.Out, opts.Redactor)
		defer con.Close()
	}
	defer con.out.Flush()
	c := &chat{Console: con, opts: opts}
	if c.term.Interactive && c.term.Decorate && !opts.Classic {
		if code, ok := runFullscreen(c); ok {
			return code
		}
	}

	if c.term.Interactive {
		c.banner()
		for _, n := range opts.Notices {
			if rest, ok := strings.CutPrefix(n, "✓ "); ok {
				n = c.term.sym().check + " " + rest
			}
			c.r.println(c.st.gray(n))
		}
		for _, w := range opts.Warnings {
			c.r.println(c.st.yellow("warning: " + w))
		}
		if mode := opts.Session.Mode(); mode != config.ApprovalDefault && !opts.ConfirmYolo {
			c.r.println("Approval mode: " + c.modeText() + c.st.gray(" ("+modeDescriptions[mode]+")"))
		}
		c.r.println(c.st.gray(`Enter sends · end a line with \ or wrap lines in """ for more · Ctrl+C cancels, twice quits · /help`))
	}
	if opts.Resume != "" {
		c.resume(opts.Resume)
	}
	if opts.ConfirmYolo {
		switch ok, quit := c.confirmYolo(); {
		case quit:
			return ExitInterrupted
		case ok:
			c.r.println("Approval mode: " + c.modeText())
		default:
			opts.Session.SetMode(opts.FallbackMode)
			c.r.println("Approval mode: " + c.modeText())
		}
	}
	if opts.ConfirmWebYolo {
		ok, quit := c.confirmWebYolo()
		if quit {
			return ExitInterrupted
		}
		if !ok {
			msg, _, _ := setWeb(&c.opts, config.WebSafe)
			c.r.println(msg)
		}
	}
	if c.opts.MCP != nil {
		c.opts.MCP.Subscribe(c.mcp.add)
	}
	var msg assembler
	for {
		for _, e := range c.mcp.take() {
			if e.Failed {
				c.r.println(c.st.yellow(e.Text))
			} else {
				c.r.println(c.st.gray(e.Text))
			}
		}
		c.prompt(msg.continuing())
		select {
		case <-c.quit:
			c.dropBox()
			c.r.println("")
			return ExitInterrupted
		case <-c.interrupts:
			c.input.interrupted()
			msg.reset()
			if !c.dropBox() {
				c.r.println("")
			}
		case line, ok := <-c.input.next():
			c.input.received()
			if !ok {
				if c.term.Interactive && !c.dropBox() {
					c.r.println("")
				}
				return ExitOK
			}
			if c.inBox {
				c.typed = append(c.typed, line)
			}
			text, done := msg.add(line)
			if !done {
				continue
			}
			c.closeBox(text)
			if strings.TrimSpace(text) == "" {
				continue
			}
			if strings.HasPrefix(text, "/") {
				if quit := c.command(strings.TrimSpace(text)); quit {
					return ExitOK
				}
				continue
			}
			text = c.attachMentions(text)
			if c.goal != nil {
				c.goal.paused, c.goal.quiet = false, 0
				if c.runGoal(text) {
					return ExitInterrupted
				}
				continue
			}
			if c.turn(text) {
				return ExitInterrupted
			}
		}
	}
}

// attachMentions attaches the files and MCP resources a message mentions
// with @, saying what it attached.
func (c *chat) attachMentions(text string) string {
	var attach func(string) (tools.Attachment, error)
	if c.opts.Files != nil {
		attach = c.opts.Files.Attach
	}
	var resource func(string, string) (string, error)
	servers := map[string]bool{}
	if m := c.opts.MCP; m != nil {
		servers = m.Servers()
		resource = func(server, uri string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return m.ReadResource(ctx, server, uri)
		}
	}
	full, notes := expandMentions(text, attach, resource, servers)
	for _, n := range notes {
		if n.ok {
			c.r.println(c.st.gray("  " + c.term.sym().elbow + " " + n.text))
		} else {
			c.r.println(c.st.yellow("  " + c.term.sym().elbow + " " + n.text))
		}
	}
	return full
}

type chat struct {
	*Console
	opts   ChatOptions
	header *agent.ToolRun // the call whose "●" line was printed last

	// The input box (box.go).
	inBox     bool     // a box is on screen
	typed     []string // the lines typed into it so far
	lastUsage []string // the last turn's context use and speed, for its footer

	// The goal (goal.go), and how the last turn went.
	goal, lastGoal *goal
	turnErr        error
	turnStopped    string // why the agent loop ended the last turn early
	unsavedWarned  bool   // said that the conversation can't be saved
	turnTools      int    // tool calls in the turn

	btws []*btwExchange // the side questions asked (btw.go), oldest first
	mcp  mcpNotes       // MCP news not shown yet, in line mode

	subMu    sync.Mutex         // subagents working at once report together
	subShown map[int]subPrinted // what was printed of each subagent at work
}

// subPrinted is what line mode printed of a subagent.
type subPrinted struct {
	started bool
	calls   int
}

// Subagent shows a subagent in line mode: its start, each call as it ends
// and how it ended, printed above the spinner of its task call.
func (c *chat) Subagent(task *agent.ToolRun, s agent.SubagentState) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if c.subShown == nil {
		c.subShown = map[int]subPrinted{}
	}
	elbow := "  " + c.term.sym().elbow + " "
	p := c.subShown[s.ID]
	if !p.started && !s.Waiting && !s.Done {
		kind := "read-only"
		if s.Full {
			kind = "full"
		}
		c.r.printAbove(c.st.gray(elbow+"subagent ") + s.Description + c.st.gray(" started ("+kind+")"))
		p.started = true
	}
	for ; p.calls < len(s.Calls) && !s.Calls[p.calls].Running; p.calls++ {
		call := s.Calls[p.calls]
		line := "      " + s.Description + ": " + call.Summary
		switch {
		case call.Status == agent.RunFailed:
			line += " · error: " + firstLine(call.Error, 120)
		case call.Status == agent.RunDenied:
			line += " · declined"
		case call.Display != "":
			line += " · " + call.Display
		}
		c.r.printAbove(c.st.gray(line))
	}
	if s.Done {
		delete(c.subShown, s.ID)
		if s.Err != "" {
			c.r.printAbove(c.st.yellow(elbow + "subagent " + s.Description + " stopped: " + s.Err))
		} else {
			c.r.printAbove(c.st.gray(elbow+"subagent ") + s.Description + c.st.gray(" finished · "+tools.Count(len(s.Calls), "call", "calls")))
		}
		return
	}
	c.subShown[s.ID] = p
}

// SubagentHost has no view of a subagent's conversation to give: line mode
// prints its calls through Subagent.
func (c *chat) SubagentHost(agent.SubagentState) agent.Host { return nil }

func (c *chat) prompt(continuing bool) {
	if !c.term.Interactive {
		return
	}
	if c.boxed() {
		c.drawBox(continuing)
		return
	}
	p := c.st.cyan("> ")
	if continuing {
		p = c.st.gray(". ")
	}
	io.WriteString(c.out, p)
	c.out.Flush()
}

// command runs a slash command and reports whether the user wants to quit.
func (c *chat) command(cmd string) (quit bool) {
	l := &lineCommands{c: c}
	runCommand(l, cmd)
	return l.quitting
}

// runPrompt runs an MCP prompt command: the server's prompt, filled in with
// the arguments typed, is sent as the user's message. It reports whether
// the user wants to quit.
func (c *chat) runPrompt(name, arg string) (quit bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	text, err := c.opts.MCP.RunPrompt(ctx, name, arg)
	cancel()
	if err != nil {
		c.r.println(c.st.yellow(err.Error()))
		return false
	}
	n := strings.Count(text, "\n") + 1
	c.r.println(c.st.gray(fmt.Sprintf("  %s sent the MCP prompt's text · %d line%s", c.term.sym().elbow, n, pluralS(n))))
	return c.turn(text)
}

// modeDescriptions explain the approval modes (SPEC §9).
var modeDescriptions = map[string]string{
	config.ApprovalPlan:     "reads and searches only; the model presents a plan",
	config.ApprovalDefault:  "asks before edits and commands",
	config.ApprovalEditor:   "reads and edits without asking, runs no commands",
	config.ApprovalAutoEdit: "edits without asking, asks before commands",
	config.ApprovalYolo:     "runs edits and commands without asking",
}

// modeText names the current approval mode: editor in gold, yolo in red.
func (c *chat) modeText() string {
	switch mode := c.opts.Session.Mode(); mode {
	case config.ApprovalYolo:
		return c.st.red(mode)
	case config.ApprovalEditor:
		return c.st.yellow(mode)
	default:
		return mode
	}
}

// modeCommand shows the approval mode, or changes it. Yolo needs a
// confirmation.
func (c *chat) modeCommand(arg string) (quit bool) {
	s := c.opts.Session
	mode, ok := config.ParseApprovalMode(arg)
	switch {
	case arg == "":
	case !ok:
		c.r.println(c.st.yellow(fmt.Sprintf("Unknown mode %q; choose %s.", arg, config.ApprovalModeNames)))
		return false
	case mode != config.ApprovalYolo:
		s.SetMode(mode)
	default:
		ok, quit := c.confirmYolo()
		if quit {
			return true
		}
		if !ok {
			c.r.println("Approval mode stays " + c.modeText() + ".")
			return false
		}
	}
	c.r.println("Approval mode: " + c.modeText() + c.st.gray(" ("+modeDescriptions[s.Mode()]+"; choices: "+config.ApprovalModeNames+")"))
	return false
}

// confirmYolo asks before switching to yolo mode and switches if the user
// types yes.
func (c *chat) confirmYolo() (ok, quit bool) {
	c.r.println(c.st.red("yolo mode runs every edit and command the model asks for without asking you,") +
		" in the workspace and outside it.")
	io.WriteString(c.out, "Type yes to turn it on: ")
	line, err := c.readLine(true)
	if err != nil || strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return false, errors.Is(err, errQuit)
	}
	c.opts.Session.SetMode(config.ApprovalYolo)
	return true, false
}

// confirmWebYolo asks before web yolo and switches if the user types yolo
// (SPEC §19.1).
func (c *chat) confirmWebYolo() (ok, quit bool) {
	c.r.println(c.st.red(webYoloWarning))
	io.WriteString(c.out, "Type yolo to turn it on: ")
	line, err := c.readLine(true)
	if err != nil || strings.TrimSpace(strings.ToLower(line)) != "yolo" {
		return false, errors.Is(err, errQuit)
	}
	msg, _, _ := setWeb(&c.opts, config.WebYolo)
	c.r.println(msg)
	return true, false
}

// toolsCommand lists the tools and whether the mode lets them run.
func (c *chat) toolsCommand() {
	s := c.opts.Session
	reg := s.Tools()
	if reg == nil {
		c.r.println("No tools in this session.")
		return
	}
	kinds := map[tools.Kind]string{tools.Read: "reads", tools.Edit: "changes files", tools.Execute: "runs commands"}
	for _, name := range reg.Names() {
		tool, _ := reg.Lookup(name)
		line := fmt.Sprintf("%-16s %s", name, kinds[tool.Kind()])
		if !s.Available(tool.Kind()) {
			line += c.st.gray(" (not in " + s.Mode() + " mode)")
		}
		c.r.println(line)
	}
}

// effortCommand shows the reasoning effort, or sets it for the session.
// Without a level, in a terminal, it opens the slider.
func (c *chat) effortCommand(arg string) (quit bool) {
	levels := c.opts.EffortLevels
	if arg == "" && c.boxed() && c.openKeys != nil && c.term.columns() >= minSliderColumns {
		if kb, err := c.openKeys(inputOptions{}); err == nil {
			defer kb.close()
			return c.effortSlider(kb)
		}
	}
	if arg != "" {
		l, ok := effort.Parse(arg)
		if !ok {
			c.r.println(c.st.yellow(fmt.Sprintf("Unknown effort %q; choose %s.", arg, effortChoices(levels))))
			return false
		}
		c.opts.Session.SetEffort(l)
	}
	l := c.opts.Session.Effort()
	var desc string
	switch sent := effort.Fit(l, levels); {
	case l == effort.Default:
		desc = "the server's default"
	case l == effort.None:
		desc = "none (no reasoning)"
	case l == effort.UltraWorkflow:
		desc = ultraWorkflowText(sent)
	case sent != l:
		desc = fmt.Sprintf("%s, sent as %s, the nearest level the model supports", l, sent)
	default:
		desc = string(l)
	}
	c.r.println("Reasoning effort: " + desc + c.st.gray(" (choices: "+effortChoices(levels)+")"))
	return false
}

// effortSlider lets the user pick the effort on the slider. Enter saves
// the level as the provider's default; s keeps it to this session.
func (c *chat) effortSlider(kb keyboard) (quit bool) {
	s := newEffortSlider(c.opts.EffortLevels, c.opts.Session.Effort())
	choice, ok, save, quit := c.pickEffort(s, kb)
	switch {
	case quit:
		return true
	case !ok:
		c.r.println(c.st.gray("Reasoning effort stays " + s.nowName + "."))
		return false
	}
	c.opts.Session.SetEffort(choice.level)
	line := "Reasoning effort: " + choice.name
	switch {
	case !save:
		line += c.st.gray(", for this session")
	case c.opts.Providers == nil:
		line += c.st.gray(", for this session (there is no config file to save it in)")
	default:
		if err := c.opts.Providers.SetEffort(c.opts.Status.Provider, choice.level); err != nil {
			line += c.st.yellow(", for this session; not saved: " + err.Error())
		} else {
			line += c.st.gray(", saved for " + c.opts.Status.Provider)
		}
	}
	c.r.println(line)
	return false
}

// effortShown is the selected effort, or ultraworkflow's local mode name;
// "" for the server's default.
func (c *chat) effortShown() string {
	l := c.opts.Session.Effort()
	if l == effort.Default {
		return ""
	}
	if l == effort.UltraWorkflow {
		return string(l)
	}
	return string(effort.Fit(l, c.opts.EffortLevels))
}

// effortChoices lists the levels /effort accepts: the model's, when known,
// plus the CLI's ultraworkflow mode.
func effortChoices(levels []effort.Level) string {
	if len(levels) == 0 {
		levels = effort.Ladder
	}
	names := []string{string(effort.None)}
	for _, l := range levels {
		names = append(names, string(l))
	}
	return strings.Join(append(names, string(effort.UltraWorkflow), "default"), ", ")
}

// turn sends one message and shows what follows. It reports whether the
// user asked to quit while it ran.
func (c *chat) turn(text string) (quit bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-c.interrupts:
			cancel()
		case <-c.quit:
			cancel()
		case <-done:
		}
	}()

	start := time.Now()
	c.turnTools = 0
	c.opts.Session.SetAutoCompact(agent.AutoCompactAt(c.opts.AutoCompact, c.opts.Status.ContextWindow), c.opts.Status.ContextWindow)
	res, err := c.opts.Session.Send(ctx, text, c)
	c.turnErr, c.turnStopped = err, res.Stopped
	close(done)

	select {
	case <-c.quit:
		return true
	default:
	}
	switch {
	case errors.Is(err, context.Canceled):
		c.r.println(c.st.yellow("[cancelled]"))
	case err != nil:
		c.r.println(c.st.red("error: " + errorText(err)))
	case res.Stopped != "":
		c.r.println(c.st.yellow("[stopped: " + res.Stopped + "]"))
	case res.StopReason == provider.StopMaxTokens:
		c.r.println(c.st.yellow("[stopped: the reply reached the output token limit]"))
	case res.StopReason == provider.StopRefusal:
		c.r.println(c.st.yellow(refusalText(res.StopDetail)))
	}
	if w := c.unsavedWarning(); w != "" {
		c.r.println(c.st.yellow("warning: " + w))
	}
	info := c.opts.Status
	info.Mode = c.opts.Session.Mode()
	info.Effort = c.effortShown()
	parts := append(c.r.status(info, res, time.Since(start)), c.costPart()...)
	base := 3 // provider, model and mode, then the effort if any
	if info.Effort != "" {
		base++
	}
	c.lastUsage = parts[base:]
	if !c.boxed() { // the box's footer shows it otherwise
		c.r.println(c.statusLine(parts))
	}
	if u := res.Usage; u != nil && err == nil {
		if at := agent.AutoCompactAt(c.opts.AutoCompact, c.opts.Status.ContextWindow); at > 0 && u.Input+u.Output >= at {
			c.r.println(c.st.gray(fmt.Sprintf("The conversation reached %s tokens of context; compacting it to make room (/compact auto).", kTokens(u.Input+u.Output))))
			c.compactCommand("")
		}
	}
	c.out.Flush()
	return false
}

// compactCommand summarizes the conversation; Ctrl+C stops it.
func (c *chat) compactCommand(focus string) {
	ctx, stop := c.Interruptible()
	defer stop()
	c.r.startSpinner("compacting the conversation")
	res, err := c.opts.Session.Compact(ctx, focus, c.opts.Status.ContextWindow, nil)
	c.r.stopSpinner()
	switch {
	case errors.Is(err, agent.ErrNothingToCompact):
		c.r.println(c.st.gray("There is nothing to compact yet."))
	case errors.Is(err, agent.ErrNotSmaller):
		c.r.println(c.st.gray(fmt.Sprintf("The summary wasn't smaller than the conversation (about %s tokens), so the conversation stays as it is.", kTokens(res.Before))))
	case errors.Is(err, context.Canceled):
		c.r.println(c.st.yellow("Compaction interrupted; the conversation is unchanged."))
	case err != nil:
		c.r.println(c.st.yellow("Compaction failed: " + err.Error()))
	default:
		c.r.println(c.st.gray(fmt.Sprintf("Conversation compacted · %d messages, about %s → %s tokens", res.Messages, kTokens(res.Before), kTokens(res.After))))
		c.lastUsage = contextEstimate(res.After)
	}
}

// CompactStart shows that a compaction started while a turn runs.
func (c *chat) CompactStart() { c.r.startSpinner("compacting the conversation") }

// Compacted shows a compaction made while a turn ran.
func (c *chat) Compacted(res agent.CompactResult, err error) {
	c.r.stopSpinner()
	switch {
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		c.r.printAbove(c.st.yellow("Compaction failed: " + err.Error()))
		return
	}
	c.r.printAbove(c.st.gray(fmt.Sprintf("Conversation compacted · %d messages, about %s → %s tokens", res.Messages, kTokens(res.Before), kTokens(res.After))))
}

// The agent.Host methods.

func (c *chat) ReplyStart() { c.r.begin() }
func (c *chat) Event(ev provider.Event) {
	if r, ok := ev.(provider.Retrying); ok {
		c.r.stopSpinner()
		c.r.println(c.st.gray(retryText(r)))
		return
	}
	c.r.event(ev)
}
func (c *chat) ReplyEnd() { c.r.end() }

// showHeader prints a call's "●" line, once.
func (c *chat) showHeader(run *agent.ToolRun) {
	if c.header == run {
		return
	}
	c.header = run
	c.r.println(c.st.cyan(c.term.sym().bullet) + " " + run.Summary)
}

func (c *chat) ToolStart(run *agent.ToolRun) {
	c.turnTools++
	c.showHeader(run)
	c.r.startSpinner("running " + run.Name)
}

func (c *chat) ToolEnd(run *agent.ToolRun) {
	c.r.stopSpinner()
	c.showHeader(run)
	// Show what an edit changed when nobody was asked about it.
	if run.Status == agent.RunOK && run.Kind == tools.Edit && !run.Asked && run.Preview != "" {
		c.printDiff(run.Preview, 60)
	}
	var result string
	switch run.Status {
	case agent.RunOK:
		result = run.Display
		if result == "" {
			result = "done"
		}
		if run.Elapsed >= 2*time.Second {
			result += fmt.Sprintf(" · %.1fs", run.Elapsed.Seconds())
		}
		if run.Warn {
			result = c.st.yellow(result)
		} else {
			result = c.st.gray(result)
		}
	case agent.RunFailed:
		result = c.st.red("error: " + firstLine(run.Error, 200))
	case agent.RunDenied:
		result = c.st.yellow("declined")
	case agent.RunCancelled:
		result = c.st.yellow("cancelled")
	}
	c.r.println(c.st.gray("  └ ") + result)
	c.out.Flush()
}

// Approve shows what a call would do and asks whether it may.
func (c *chat) Approve(ctx context.Context, run *agent.ToolRun) (agent.Approval, error) {
	c.r.stopSpinner() // a subagent asks while its task call's spinner runs
	c.showHeader(run)
	switch {
	case run.Preview == "":
	case run.Kind == tools.Edit:
		c.printDiff(run.Preview, 200)
	case strings.Contains(run.Preview, "\n") || !strings.HasSuffix(run.Summary, run.Preview):
		// Show a command in full unless the "●" line already does.
		for _, l := range strings.Split(run.Preview, "\n") {
			c.r.println("    " + l)
		}
	}
	for _, p := range run.Outside {
		c.r.println(c.st.yellow("    outside the workspace: " + p))
	}
	question, session := "Allow this?", "yes for this session"
	switch {
	case run.Kind == tools.Edit && run.Question == "" && run.SessionFor != "":
		question, session = "Apply this change?", "yes to "+run.SessionFor+" for the session"
	case run.Kind == tools.Edit:
		question, session = "Apply this change?", "yes to this file for the session"
	case run.Kind == tools.Execute:
		question, session = "Run this command?", "yes to this command for the session"
		if strings.HasPrefix(run.Name, "mcp__") {
			question, session = "Call this MCP tool?", "yes to this tool for the session"
		}
	case len(run.Outside) > 0:
		question, session = "Read outside the workspace?", "yes to reads outside the workspace for the session"
	}
	if run.Question != "" {
		question, session = run.Question, ""
		if run.SessionFor != "" {
			session = "yes to " + run.SessionFor + " for the session"
		}
	}
	if run.Tainted {
		question += c.st.gray(" (web content is in the conversation)")
	}
	if run.Agent != "" {
		question = "The subagent " + run.Agent + " asks: " + question
	}
	choices := c.st.gray("y yes · s " + session + " · n no · n: <note> no, and tell the model why")
	if session == "" {
		choices = c.st.gray("y yes · n no · n: <note> no, and tell the model why")
	}
	for {
		io.WriteString(c.out, c.st.yellow(question)+" "+choices+"\n"+c.st.yellow("? "))
		c.out.Flush()
		select {
		case <-ctx.Done():
			c.input.interrupted()
			c.r.println("")
			return agent.Approval{}, ctx.Err()
		case line, ok := <-c.input.next():
			c.input.received()
			if !ok {
				c.r.println("")
				return agent.Approval{}, io.EOF
			}
			c.echo(line)
			if a, ok := parseApproval(line); ok {
				return a, nil
			}
			c.r.println(c.st.gray("Answer y, s or n, or n: followed by a note for the model."))
		}
	}
}

// parseApproval reads an answer to an approval prompt.
func parseApproval(line string) (agent.Approval, bool) {
	s := strings.TrimSpace(line)
	switch strings.ToLower(s) {
	case "y", "yes":
		return agent.Approval{Allow: true}, true
	case "s", "session":
		return agent.Approval{Allow: true, Session: true}, true
	case "n", "no":
		return agent.Approval{}, true
	}
	lower := strings.ToLower(s)
	for _, prefix := range []string{"n:", "no:"} {
		if strings.HasPrefix(lower, prefix) {
			return agent.Approval{Note: strings.TrimSpace(s[len(prefix):])}, true
		}
	}
	return agent.Approval{}, false
}

// printDiff shows a unified diff in color, indented, up to maxLines lines.
func (c *chat) printDiff(diff string, maxLines int) {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	for i, l := range lines {
		if i == maxLines {
			c.r.println(c.st.gray(fmt.Sprintf("    … %d more lines", len(lines)-maxLines)))
			break
		}
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			l = c.st.gray(l)
		case strings.HasPrefix(l, "@@"):
			l = c.st.cyan(l)
		case strings.HasPrefix(l, "+"):
			l = c.st.green(l)
		case strings.HasPrefix(l, "-"):
			l = c.st.red(l)
		}
		c.r.println("    " + l)
	}
}

// firstLine shortens text to its first line and at most n characters.
func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
