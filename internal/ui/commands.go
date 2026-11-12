package ui

import (
	"fmt"
	"strings"

	"debbie/internal/effort"
	"debbie/internal/setup"
)

// The slash commands: one table, for /help and the completion, and one
// dispatcher, for both UIs. What differs between the UIs sits behind
// commandUI: lineCommands for line mode, screenCommands for the fullscreen UI.

// command is a slash command, for /help and the completion.
type command struct {
	name, args, about string
	subs              []subcommand
}

// subcommand is the first argument a command may take.
type subcommand struct {
	name, about string
	args        bool // more arguments follow
}

// commandList is what /help lists and the completion offers. subs are a
// command's subcommands, which the completion offers after its name.
var commandList = []command{
	{name: "/btw", args: "[question]", about: "ask a side question; /btw alone lists them"},
	{name: "/clear", about: "start a new conversation"},
	{name: "/compact", args: "[focus | auto <tokens|on|off>]", about: "summarize the conversation to free context, or set when it happens by itself",
		subs: []subcommand{{"auto", "when the conversation is compacted by itself: a slider, tokens, on or off", true}}},
	{name: "/cost", args: "[model [in out]]", about: "what this session cost, by model; set a model's prices in USD per million tokens"},
	{name: "/effort", args: "[level | budget <tokens|auto>]", about: "how hard the model reasons: a slider, a level, or a cap on its thinking (llama-server)",
		subs: []subcommand{{"budget", "cap how long the model thinks per reply (llama-server): 16k, or auto", true}}},
	{name: "/goal", args: "[condition|clear]", about: "keep working until a condition is met",
		subs: []subcommand{{"clear", "end the goal", false}}},
	{name: "/help", args: "[tools]", about: "the commands and keys, or the tools the model can use",
		subs: []subcommand{{"tools", "the tools the model can use", false}}},
	{name: "/mcp", args: "[add|remove|enable|disable|reconnect] [name]", about: "the MCP servers: list, add, remove, switch on or off",
		subs: []subcommand{{"add", "add a server: a command, or a URL", true}, {"add-json", "add a server from its JSON", true},
			{"remove", "remove a server", true}, {"enable", "switch a server on", true}, {"disable", "switch a server off", true},
			{"reconnect", "connect to a server again", true}, {"tools", "a server's tools", true}}},
	{name: "/mode", args: "[mode]", about: "the approval mode: plan, default, editor, auto or yolo; alone, pick one",
		subs: []subcommand{{"plan", "reads and searches only; the model presents a plan", false},
			{"default", "asks before edits and commands", false}, {"editor", "edits without asking, runs no commands", false},
			{"auto", "edits without asking, asks before commands", false}, {"yolo", "runs edits and commands without asking", false}}},
	{name: "/model", args: "[id]", about: "switch the model of the provider in use: pick from the server's list, or name one"},
	{name: "/provider", args: "[subcommand]", about: "list, add, edit, test, trust, use, default or remove providers; choose what agents run on",
		subs: []subcommand{{"list", "the providers", false}, {"add", "add a provider, step by step", false},
			{"edit", "change a provider, vision among it", true}, {"test", "check a provider", true},
			{"trust", "trust a server's certificate", true}, {"use", "switch to a provider", true},
			{"default", "the provider sessions start with", true}, {"remove", "remove a provider", true},
			{"workers", "what agents run on: a provider, a model, an effort; or off", true}}},
	{name: "/quit", about: "leave (or ctrl+c twice, or ctrl+d)"},
	{name: "/resume", args: "[text | rename <name>]", about: "go back to an earlier conversation in this folder, or name this one",
		subs: []subcommand{{"rename", "name this conversation, for /resume", true}}},
	{name: "/web", args: "[off|safe|yolo|browser [install]]", about: "web access: the browser tools and whether they ask, and Chrome's connection",
		subs: []subcommand{{"safe", "the browser tools ask before opening a site", false}, {"off", "no web tools", false},
			{"yolo", "the browser tools run without asking", false}, {"browser", "whether Chrome is connected", false},
			{"browser install", "set up the Chrome extension, step by step", false}}},
}

// commandUI is what the commands need from the UI they run in.
type commandUI interface {
	options() *ChatOptions
	say(text string)
	warn(text string)
	quit()
	clearConversation()
	help()
	helpTools()
	effort(arg string)
	compact(focus string)
	compactAuto(arg string)
	goal(arg string)
	btw(arg string)
	mcp(arg string)
	mode(arg string)
	model(arg string)
	resume(arg string)
	rename(arg string)
	cost(arg string)
	web(arg string)
	browserInstall()
	provider(arg string)
	prompt(name, arg string)
}

// subArgument reports whether arg starts with the word sub, and what
// follows it.
func subArgument(arg, sub string) (string, bool) {
	first, rest, _ := strings.Cut(arg, " ")
	return strings.TrimSpace(rest), first == sub
}

// runCommand runs a slash command in a UI.
func runCommand(u commandUI, cmd string) {
	name, arg, _ := strings.Cut(cmd, " ")
	arg = strings.TrimSpace(arg)
	opts := u.options()
	report := func(msg string, ok bool) {
		if ok {
			u.say(msg)
		} else {
			u.warn(msg)
		}
	}
	switch name {
	case "/quit":
		u.quit()
	case "/clear":
		u.clearConversation()
	case "/help":
		if arg == "tools" {
			u.helpTools()
		} else {
			u.help()
		}
	case "/effort":
		if rest, ok := subArgument(arg, "budget"); ok {
			report(reasoningBudget(opts, rest))
			return
		}
		u.effort(arg)
	case "/compact":
		if rest, ok := subArgument(arg, "auto"); ok && isCompactAuto(rest) {
			u.compactAuto(rest)
			return
		}
		u.compact(arg)
	case "/goal":
		u.goal(arg)
	case "/btw":
		u.btw(arg)
	case "/mcp":
		u.mcp(arg)
	case "/mode":
		u.mode(arg)
	case "/model":
		u.model(arg)
	case "/resume":
		if rest, ok := subArgument(arg, "rename"); ok {
			u.rename(rest)
			return
		}
		u.resume(arg)
	case "/cost":
		u.cost(arg)
	case "/web":
		if rest, ok := subArgument(arg, "browser"); ok {
			switch rest {
			case "":
				u.say(strings.Join(browserCommand(opts), "\n"))
			case "install", "setup":
				u.browserInstall()
			default:
				u.warn("/web browser shows Chrome's connection; /web browser install sets it up.")
			}
			return
		}
		u.web(arg)
	case "/provider":
		if rest, ok := subArgument(arg, "workers"); ok {
			report(workersCommand(opts, rest))
			return
		}
		u.provider(arg)
	default:
		if strings.HasPrefix(name, "/mcp__") && opts.MCP != nil {
			u.prompt(name, arg)
			return
		}
		u.warn("Unknown command " + name + "; /help lists the commands.")
	}
}

// ultraWorkflowText describes ultraworkflow, given the level the model gets.
func ultraWorkflowText(sent effort.Level) string {
	if sent == effort.Default {
		return "ultraworkflow (several agents; the server's default reasoning effort)"
	}
	return fmt.Sprintf("ultraworkflow (several agents; reasoning effort %s)", sent)
}

// isCompactAuto reports whether what follows "/compact auto" sets the
// automatic compaction, rather than being a focus that starts with "auto".
func isCompactAuto(arg string) bool {
	if arg == "" || arg == "show" {
		return true
	}
	_, ok := setup.ParseTokens(arg)
	return ok
}

// commandHelp lists the commands for line mode.
func commandHelp() string {
	width := 0
	for _, c := range commandList {
		width = max(width, textWidth(c.name+" "+c.args))
	}
	var lines []string
	for _, c := range commandList {
		lines = append(lines, fmt.Sprintf("%-*s  %s", width, strings.TrimSpace(c.name+" "+c.args), c.about))
	}
	return strings.Join(append(lines,
		"@path in a message attaches a file; @server:uri an MCP resource.",
		`Enter sends. End a line with \ to continue it, or wrap lines in """ to send them as they are.`), "\n")
}

// workersCommand runs /provider workers for both UIs: it says what agents
// run on, or changes it. It returns what to tell the user, and false for a
// bad argument.
func workersCommand(opts *ChatOptions, arg string) (string, bool) {
	m := opts.Providers
	if m == nil {
		return "There is no config file to keep providers in.", false
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return workersText(m), true
	}
	name, model, level := fields[0], "", effort.Default
	if name == "off" {
		if len(fields) > 1 {
			return workersUsage, false
		}
		name = ""
	}
	for _, f := range fields[1:] {
		if l, ok := effort.Parse(f); ok && l != effort.Default && l != effort.UltraWorkflow {
			level = l
		} else if model == "" {
			model = f
		} else {
			return workersUsage, false
		}
	}
	if err := m.SetWorkers(name, model, level); err != nil {
		return "error: " + errorText(err), false
	}
	return workersText(m), true
}

// workersUsage says what /provider workers takes.
const workersUsage = "/provider workers <provider> [model] [effort], or /provider workers off for the conversation's own."

// workersText describes what agents run on.
func workersText(m *setup.Manager) string {
	sc := m.Config.Subagents
	on := "the conversation's provider"
	if sc.Provider != "" {
		on = sc.Provider
		if p := m.Config.Providers[sc.Provider]; p != nil && sc.Model == "" {
			on += " · " + p.Model
		}
	}
	if sc.Model != "" {
		on += " · " + sc.Model
	}
	level := "the conversation's effort, at most medium"
	if sc.Effort != "" {
		level = "effort " + sc.Effort
	}
	return "Agents run on " + on + ", at " + level + ". " + workersUsage
}

// lineCommands runs the commands in line mode.
type lineCommands struct {
	c        *chat
	quitting bool
}

func (l *lineCommands) options() *ChatOptions { return &l.c.opts }
func (l *lineCommands) say(text string)       { l.c.r.println(text) }
func (l *lineCommands) warn(text string)      { l.c.r.println(l.c.st.yellow(text)) }
func (l *lineCommands) quit()                 { l.quitting = true }
func (l *lineCommands) clearConversation() {
	l.c.opts.Session.Reset()
	l.c.lastUsage, l.c.goal = nil, nil
	l.c.r.println(l.c.st.gray("Conversation cleared."))
}
func (l *lineCommands) help()                { l.c.r.println(commandHelp()) }
func (l *lineCommands) helpTools()           { l.c.toolsCommand() }
func (l *lineCommands) effort(arg string)    { l.quitting = l.c.effortCommand(arg) }
func (l *lineCommands) compact(focus string) { l.c.compactCommand(focus) }
func (l *lineCommands) compactAuto(arg string) {
	c := l.c
	if arg == "" && c.boxed() && c.openKeys != nil && c.term.columns() >= minSliderColumns {
		if kb, err := c.openKeys(inputOptions{}); err == nil {
			l.quitting = c.autoCompactSlider(kb)
			kb.close()
			return
		}
	}
	if arg == "show" {
		arg = ""
	}
	msg, ok := autoCompact(&c.opts, arg)
	if ok {
		l.say(msg)
	} else {
		l.warn(msg)
	}
}
func (l *lineCommands) goal(arg string) { l.quitting = l.c.goalCommand(arg) }
func (l *lineCommands) btw(arg string)  { l.c.btwCommand(arg) }
func (l *lineCommands) mcp(arg string) {
	l.c.r.println(encodeSpans(l.c.mcpCommand(arg), l.c.term))
}
func (l *lineCommands) mode(arg string)   { l.quitting = l.c.modeCommand(arg) }
func (l *lineCommands) model(arg string)  { l.c.modelCommand(arg) }
func (l *lineCommands) resume(arg string) { l.c.resumeCommand(arg) }
func (l *lineCommands) rename(arg string) { l.c.renameCommand(arg) }
func (l *lineCommands) cost(arg string)   { l.c.costCommand(arg) }
func (l *lineCommands) web(arg string) {
	c := l.c
	msg, ok, yolo := webCommand(&c.opts, arg)
	switch {
	case yolo:
		done, quit := c.confirmWebYolo()
		l.quitting = quit
		if !quit && !done {
			l.say("Web access stays " + c.opts.Web.Mode() + ".")
		}
	case ok:
		l.say(msg)
	default:
		l.warn(msg)
	}
}
func (l *lineCommands) browserInstall()     { l.c.browserInstall() }
func (l *lineCommands) provider(arg string) { l.c.providerCommand(arg) }
func (l *lineCommands) prompt(name, arg string) {
	l.quitting = l.c.runPrompt(name, arg)
}

// screenCommands runs the commands in the fullscreen UI.
type screenCommands struct{ f *fullscreen }

func (s screenCommands) options() *ChatOptions { return &s.f.opts }
func (s screenCommands) say(text string)       { s.f.say(text, ink{}) }
func (s screenCommands) warn(text string)      { s.f.say(text, inkYellow) }
func (s screenCommands) quit()                 { s.f.done = true }
func (s screenCommands) clearConversation() {
	f := s.f
	if f.busy {
		f.flash("wait for the turn to end, or press esc")
		return
	}
	f.opts.Session.Reset()
	f.tr.entries = f.tr.entries[:1]
	f.forgetSubagents()
	f.lastUsage, f.scroll, f.goal = nil, 0, nil
	f.scr.redraw()
}
func (s screenCommands) help()             { s.f.help() }
func (s screenCommands) helpTools()        { s.f.toolsList() }
func (s screenCommands) effort(arg string) { s.f.effortCommand(arg) }
func (s screenCommands) compact(focus string) {
	if s.f.busy {
		s.f.flash("wait for the turn to end, or press esc")
		return
	}
	s.f.compact(focus, s.f.sendQueued)
}
func (s screenCommands) compactAuto(arg string) { s.f.autoCompactCommand(arg) }
func (s screenCommands) goal(arg string)        { s.f.goalCommand(arg) }
func (s screenCommands) btw(arg string)         { s.f.btwCommand(arg) }
func (s screenCommands) mcp(arg string) {
	s.f.tr.add(&noteBlock{spans: s.f.mcpCommand(arg)})
	s.f.scroll = 0
}
func (s screenCommands) mode(arg string)   { s.f.modeCommand(arg) }
func (s screenCommands) model(arg string)  { s.f.modelCommand(arg) }
func (s screenCommands) resume(arg string) { s.f.resumeCommand(arg) }
func (s screenCommands) rename(arg string) { s.f.renameCommand(arg) }
func (s screenCommands) cost(arg string)   { s.f.costCommand(arg) }
func (s screenCommands) web(arg string) {
	f := s.f
	msg, ok, yolo := webCommand(&f.opts, arg)
	switch {
	case yolo:
		f.confirmWebYolo(false)
	case ok:
		f.say(msg, ink{})
	default:
		f.say(msg, inkYellow)
	}
}
func (s screenCommands) browserInstall()         { s.f.browserInstall() }
func (s screenCommands) provider(arg string)     { s.f.providerCommand(arg) }
func (s screenCommands) prompt(name, arg string) { s.f.runPrompt(name, arg) }
