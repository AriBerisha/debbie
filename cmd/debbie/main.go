// Command debbie is a terminal coding agent for self-hosted and cloud
// models.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"debbie/internal/agent"
	"debbie/internal/app"
	"debbie/internal/browser"
	"debbie/internal/config"
	"debbie/internal/cost"
	"debbie/internal/effort"
	"debbie/internal/mcp"
	"debbie/internal/prompt"
	"debbie/internal/provider"
	"debbie/internal/sessions"
	"debbie/internal/setup"
	"debbie/internal/tools"
	"debbie/internal/ui"
)

// Exit codes (SPEC §12).
const (
	exitOK     = 0
	exitError  = 1
	exitConfig = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// testTerminal, set only by tests, stands in for the terminal, so a test can
// type into the setup wizard.
var testTerminal *ui.Terminal

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if browser.IsHostLaunch(args) {
		return hostCommand(args[0], stdin, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "install" {
		return installCommand(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "uninstall" {
		return uninstallCommand(args[1:], stdout, stderr)
	}
	// The program was renamed: its user's settings move to the new name
	// before anything reads them; not for installing it or --version.
	tookOver := ""
	if !slices.Contains(args, "-version") && !slices.Contains(args, "--version") {
		if former, err := config.TakeOver(); err != nil {
			fmt.Fprintf(stderr, "%s: taking over the settings of a former version: %v\n", app.Name, err)
		} else if former != "" {
			if dir, err := config.Dir(); err == nil {
				tookOver = "✓ Moved your settings from " + former + " to " + dir + ": " + app.FormerNames[0] + " is now " + app.Name + "."
			}
		}
	}
	if tookOver != "" && len(args) > 0 {
		fmt.Fprintln(stderr, strings.TrimPrefix(tookOver, "✓ "))
	}
	if len(args) > 0 && args[0] == "browser" {
		return browserCommand(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "provider" {
		return providerCommand(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "mcp" {
		return mcpCommand(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet(app.Name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the version and exit")
	configFlag := flags.String("config", "", "config file to use instead of $"+app.EnvVar("CONFIG")+" or the default location")
	urlFlag := flags.String("url", "", "chat with this OpenAI-compatible base URL instead of a configured provider")
	modelFlag := flags.String("model", "", "model to use (default: the provider's model, or the server's only model)")
	keyEnvFlag := flags.String("api-key-env", "", "environment variable holding the API key")
	modeFlag := flags.String("mode", "", "approval mode: plan, default, editor, auto-edit or yolo (default: the config's)")
	yoloFlag := flags.Bool("yolo", false, "run edits and commands without asking (asks you to confirm first)")
	classicFlag := flags.Bool("classic", false, "chat in line mode instead of the fullscreen UI")
	webFlag := flags.String("web", "", "web access: off, safe or yolo (default: the config's; yolo asks you to confirm)")
	continueFlag := flags.Bool("continue", false, "go on with the latest conversation in this directory (also -c)")
	flags.BoolVar(continueFlag, "c", false, "short for --continue")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s [flags]               chat in this directory\n"+
			"       %s install             put %s on your PATH, so typing %s starts it (%s uninstall undoes it)\n"+
			"       %s browser <command>   install the Chrome extension, check it, remove it (%s browser help)\n"+
			"       %s mcp <command> ...   add, list, check and remove MCP servers (%s mcp help)\n"+
			"       %s provider add ...    add a provider without the wizard\n\nflags:\n",
			app.Name, app.Name, app.Name, app.Name, app.Name, app.Name, app.Name, app.Name, app.Name, app.Name)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitConfig
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", app.Name, flags.Arg(0))
		return exitConfig
	}
	if *showVersion {
		fmt.Fprintln(stdout, app.Name, app.Version)
		return exitOK
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "%s: %s\n", app.Name, fmt.Sprintf(format, a...))
		return code
	}

	path, err := config.Path(*configFlag)
	if err != nil {
		return fail(exitConfig, "%v", err)
	}
	cfg, warnings, err := config.Load(path)
	firstRun := false
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = config.New()
		firstRun = *urlFlag == ""
	case err != nil:
		return fail(exitConfig, "%v", err)
	}

	webMode := cfg.Web.Mode
	switch *webFlag {
	case "":
	case config.WebOff, config.WebSafe, config.WebYolo:
		webMode = *webFlag
	default:
		return fail(exitConfig, "--web %q: use off, safe or yolo", *webFlag)
	}

	mode := cfg.ApprovalMode
	switch {
	case *yoloFlag:
		mode = config.ApprovalYolo
	case *modeFlag != "":
		m, ok := config.ParseApprovalMode(*modeFlag)
		if !ok {
			return fail(exitConfig, "--mode %q: use %s", *modeFlag, config.ApprovalModeNames)
		}
		mode = m
	}

	// The terminal, before anything asks the user a question.
	var term ui.Terminal
	if in, ok := stdin.(*os.File); ok {
		if out, ok := stdout.(*os.File); ok {
			var restore func()
			term, restore = ui.Setup(in, out)
			defer restore()
		}
	}
	if testTerminal != nil {
		term = *testTerminal
	}
	redactor := config.NewRedactor(cfg.Secrets(os.Getenv)...)
	con := ui.NewConsole(term, stdin, stdout, redactor)
	defer con.Close()
	manager := &setup.Manager{Path: path, Config: cfg, Getenv: os.Getenv, AddSecret: func(s string) { redactor.Add(s) }}
	if term.Interactive {
		// The wizard opens the page where Claude API keys are made.
		manager.OpenURL = func(page string) error {
			return ui.OpenURL(page, tools.ChildEnv(os.Environ(), cfg.SecretEnvNames(), cfg.Secrets(os.Getenv)))
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fail(exitError, "%v", err)
	}
	workspace, err := tools.NewWorkspace(cwd)
	if err != nil {
		return fail(exitError, "%v", err)
	}

	var notices []string
	if tookOver != "" {
		notices = append(notices, tookOver)
	}
	if firstRun {
		if !term.Interactive {
			return fail(exitConfig, "no config file at %s yet. Run %s in a terminal to set up a provider, add one with\n"+
				"  %s provider add --preset llama-server --url http://HOST:PORT/v1\n"+
				"or chat without a config: %s --url http://HOST:PORT/v1", path, app.Name, app.Name, app.Name)
		}
		con.Clear()
		con.Banner("setting up a model", workspace.Root())
		con.Say("Welcome to " + app.Name + ". First, connect it to a model: one on your own server, or Claude with an API key.")
		con.Say("Your answers are saved in " + path + ".")
		con.Say("")
		ctx, stop := con.Interruptible()
		added, err := manager.Add(ctx, con)
		stop()
		if err == nil {
			notices = append(notices, "✓ "+added+" is set up. /provider lists providers; /provider add adds another.")
		}
		if err != nil {
			if errors.Is(err, setup.ErrCancelled) || errors.Is(err, context.Canceled) {
				con.Say("Setup cancelled; nothing was saved.")
			} else {
				con.Say("Setup stopped: " + redactor.String(err.Error()))
			}
			if con.Quitting() {
				return ui.ExitInterrupted
			}
			return exitConfig
		}
	}

	name, prov, err := chooseProvider(cfg, *urlFlag, *modelFlag, *keyEnvFlag)
	if err != nil {
		return fail(exitConfig, "%v", err)
	}
	if *urlFlag != "" || *modelFlag != "" || *keyEnvFlag != "" {
		w, err := checkProvider(*prov)
		if err != nil {
			return fail(exitConfig, "%v", err)
		}
		warnings = append(warnings, w...)
	}
	conn, err := setup.Connect(context.Background(), name, *prov, os.Getenv)
	if err != nil {
		return fail(exitError, "%v", redactor.Error(err))
	}
	redactor.Add(conn.APIKey)
	conn.AutoCompact = manager.AutoCompactFor(name)
	warnings = append(warnings, conn.Warnings...)
	if !term.Interactive {
		// In a terminal, the chat shows them under the banner instead.
		for _, w := range warnings {
			fmt.Fprintf(stderr, "warning: %s\n", redactor.String(w))
		}
	}
	shell, err := tools.FindShell(cfg.Shell.Program)
	if err != nil {
		return fail(exitConfig, "%v", err)
	}
	shell.Timeout = time.Duration(cfg.Shell.TimeoutSeconds) * time.Second
	// Commands get the agent's environment without the API keys (SPEC §3.5),
	// also those of providers added or switched to later.
	active := conn
	followConfig := func() {
		strip := cfg.SecretEnvNames()
		if active.Provider.APIKeyEnv != "" {
			strip = append(strip, active.Provider.APIKeyEnv)
		}
		secrets := append(cfg.Secrets(os.Getenv), active.APIKey)
		redactor.Add(secrets...)
		shell.Env = tools.ChildEnv(os.Environ(), strip, secrets)
	}
	followConfig()
	manager.Changed = followConfig
	registry := tools.NewRegistry(
		tools.ReadFile{}, tools.WriteFile{}, tools.EditFile{},
		tools.ListDirectory{}, tools.Glob{}, tools.Grep{},
		tools.ShellTool{Shell: shell},
	)

	env := prompt.CurrentEnv(workspace.Root())
	env.Shell = shell.Name + " (" + shell.Path + ")"
	env.Git = redactor.String(tools.GitStatus(context.Background(), workspace.Root(), shell.Env, 50))
	env.Listing = workspace.TopLevel(100)
	session := agent.New(conn.Adapter, prompt.System(env, conn.Provider.SystemPromptAppend), conn.Provider.MaxOutputTokens)
	session.SetTools(registry, &tools.Env{Workspace: workspace, Files: tools.NewFileTracker()})
	session.SetRedactor(redactor.String)
	level, _ := effort.Parse(conn.Provider.ReasoningEffort) // validated with the config
	session.SetEffort(level)
	session.SetReasoningBudget(conn.Provider.ReasoningBudgetTokens)
	session.SetMode(mode)
	if cfg.MaxTurns > 0 {
		session.SetMaxTurns(cfg.MaxTurns)
	}

	// Costs (SPEC §12, /cost): every request's usage is added up and priced
	// from the built-in list and the config's prices; nothing is fetched.
	prices := cost.NewTable(cfg.Prices)
	session.SetLedger(agent.NewLedger(prices))

	// Sessions (SPEC §12): the conversation is saved as it goes, for
	// /resume and --continue, with the configured keys taken out.
	var recorder *sessions.Recorder
	if root, err := sessions.Root(); err != nil {
		warnings = append(warnings, "conversations aren't saved for /resume: "+err.Error())
	} else {
		recorder = sessions.NewRecorder(root, workspace.Root(), redactor.String,
			func() (string, string) { return active.Name, active.Provider.Model })
		session.SetRecorder(recorder)
	}
	resume := ""
	if recorder != nil {
		saved, _ := sessions.List(recorder.Dir())
		switch {
		case *continueFlag && len(saved) > 0:
			resume = saved[0].Path
		case *continueFlag:
			notices = append(notices, "No saved conversation in this folder yet; this one starts fresh.")
		case len(saved) > 0:
			title := []rune(saved[0].Title)
			if len(title) > 60 {
				title = append(title[:59], '…')
			}
			notices = append(notices, fmt.Sprintf("Last conversation here: %q. /resume picks one to go on with; %s -c starts with the latest.",
				string(title), app.Name))
		}
	}

	// The task tool's subagents (SPEC §10) use subagents.provider when the
	// config names one, connected at the first task call, else the
	// session's provider.
	var subagentProvider func() (provider.Provider, int, error)
	if name := cfg.Subagents.Provider; name != "" {
		var once sync.Once
		var sub *setup.Conn
		var subErr error
		subagentProvider = func() (provider.Provider, int, error) {
			once.Do(func() {
				p := cfg.Providers[name]
				if p == nil {
					subErr = fmt.Errorf("subagents.provider names %q, which isn't a configured provider", name)
					return
				}
				if sub, subErr = setup.Connect(context.Background(), name, *p, os.Getenv); subErr == nil {
					redactor.Add(sub.APIKey)
				}
			})
			if subErr != nil {
				return nil, 0, subErr
			}
			return sub.Adapter, sub.Provider.MaxOutputTokens, nil
		}
	}
	registry.Add(agent.NewTaskTool(session, func(full bool) string { return prompt.Subagent(env, full) },
		subagentProvider, cfg.Subagents.MaxParallel, cfg.Subagents.MaxTurns))
	if term.Interactive {
		// The model's questions need someone at the keyboard: piped input
		// can't pick an answer.
		registry.Add(agent.AskTool{})
	}

	// access is the session's web access, set up below.
	var access *browser.Access

	// use switches the session to another configured provider.
	use := func(ctx context.Context, name string) (*setup.Conn, error) {
		p := cfg.Providers[name]
		if p == nil {
			return nil, fmt.Errorf("there is no provider named %q; /provider list shows them", name)
		}
		c, err := setup.Connect(ctx, name, *p, os.Getenv)
		if err != nil {
			return nil, err
		}
		active = c
		c.AutoCompact = manager.AutoCompactFor(name)
		followConfig()
		session.SetProvider(c.Adapter)
		session.SetSystem(prompt.System(env, p.SystemPromptAppend))
		session.SetMaxTokens(p.MaxOutputTokens)
		level, _ := effort.Parse(p.ReasoningEffort)
		session.SetEffort(level)
		session.SetReasoningBudget(p.ReasoningBudgetTokens)
		if access != nil {
			access.Refresh() // screenshots for a model that sees images
		}
		return c, nil
	}
	// models and setModel list and switch the models of the provider in use
	// (/model).
	models := func(ctx context.Context) ([]provider.ModelInfo, error) { return active.Models(ctx) }
	setModel := func(ctx context.Context, model string) (*setup.Conn, string, error) {
		warning, err := active.SetModel(ctx, model)
		if err != nil {
			return nil, "", err
		}
		session.SetMaxTokens(active.Provider.MaxOutputTokens)
		if access != nil {
			access.Refresh()
		}
		return active, warning, nil
	}

	// MCP servers start in the background; their tools join the registry
	// when they are ready (SPEC §22).
	servers := mcp.ServersFromConfig(cfg, shell.Env, workspace.Root(), os.Getenv)
	mcpManager := mcp.NewManager(servers, registry, redactor.String)
	mcpManager.Start()
	defer mcpManager.Close()

	// Web access (SPEC §19): the browser tools and the bridge to Chrome
	// exist only while it is on.
	if runDir, err := browser.RunDir(); err != nil {
		if webMode != config.WebOff {
			warnings = append(warnings, "web access is unavailable: "+err.Error())
		}
	} else {
		web := cfg.Web
		access = &browser.Access{Session: session, Registry: registry, Bridge: browser.NewBridge(runDir, workspace.Root()),
			Web: func() config.Web { return web }, Redact: redactor.String,
			Vision: func() bool { return active.Provider.Vision }}
		defer access.Close()
		if webMode != config.WebOff {
			if note := access.SetMode(webMode); note != "" {
				warnings = append(warnings, note)
			}
		}
	}

	return ui.Chat(ui.ChatOptions{
		Session:      session,
		MCP:          mcpManager,
		Files:        workspace,
		ChildEnv:     func() []string { return shell.Env },
		Console:      con,
		Workspace:    workspace.Root(),
		EffortLevels: conn.EffortLevels,
		Providers:    manager,
		Use:          use,
		Models:       models,
		SetModel:     setModel,
		Notices:      notices,
		Warnings:     warnings,
		// yolo from a flag is confirmed at the prompt; piped input can't
		// answer, so there the flag counts as the confirmation.
		ConfirmYolo:  mode == config.ApprovalYolo && term.Interactive,
		FallbackMode: cfg.ApprovalMode,
		Web:          access,
		Sessions:     recorder,
		Resume:       resume,
		Prices:       prices,
		// The same for web yolo, which falls back to safe.
		ConfirmWebYolo: webMode == config.WebYolo && access != nil && term.Interactive,
		Classic:        *classicFlag || cfg.TUI == config.TUIClassic,
		AutoCompact:    conn.AutoCompact,
		History:        loadHistory(historyPath(path)),
		SaveHistory:    saveHistory(historyPath(path), redactor.String),
		Status: ui.StatusInfo{
			Provider:      name,
			Model:         conn.Provider.Model,
			ContextWindow: conn.ContextWindow,
		},
	})
}

// chooseProvider picks the provider for this run: one built from --url, or
// the config's default, with --model and --api-key-env applied. The result
// is a copy that the caller may change.
func chooseProvider(cfg *config.Config, rawURL, model, keyEnv string) (string, *config.Provider, error) {
	var name string
	var p config.Provider
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return "", nil, fmt.Errorf("--url %q is not a URL such as http://192.168.1.20:8084/v1", rawURL)
		}
		name = u.Host
		p = config.Provider{
			Adapter: config.AdapterOpenAI,
			BaseURL: rawURL,
			Options: config.ProviderOptions{StreamUsage: true},
		}
	} else {
		n, active, ok := cfg.ActiveProvider()
		if !ok {
			return "", nil, errors.New("the config has no provider yet; run " + app.Name + " in a terminal to set one up, or pass --url http://HOST:PORT/v1")
		}
		name, p = n, *active
	}
	if model != "" {
		p.Model = model
	}
	if keyEnv != "" {
		p.APIKeyEnv, p.APIKey = keyEnv, ""
	}
	return name, &p, nil
}

// checkProvider validates a provider assembled from flags, before any
// request is sent. A missing model is fine: it comes from the server.
func checkProvider(p config.Provider) ([]string, error) {
	if p.Model == "" {
		p.Model = "(from the server)"
	}
	check := config.New()
	check.Providers["url"] = &p
	return check.Validate()
}

// providerCommand runs "debbie provider add", which adds a provider
// without asking anything, for scripts (SPEC §6).
func providerCommand(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s provider add --preset <preset> [--url <base URL>] [--model <id>] [--name <name>]\n"+
			"         [--api-key-env <variable>] [--pin <sha256>] [--vision] [--default] [--config <file>]\n", app.Name)
		return exitConfig
	}
	if len(args) == 0 || args[0] != "add" {
		return usage()
	}
	ids := make([]string, len(setup.Presets))
	for i, p := range setup.Presets {
		ids[i] = p.ID
	}
	flags := flag.NewFlagSet(app.Name+" provider add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o setup.AddOptions
	flags.StringVar(&o.Preset, "preset", "", "the kind of server: "+strings.Join(ids, ", "))
	flags.StringVar(&o.URL, "url", "", "the base URL, such as http://192.168.1.20:8080/v1 (claude, openai and gemini know theirs)")
	flags.StringVar(&o.Model, "model", "", "the model (default: the server's only model)")
	flags.StringVar(&o.Name, "name", "", "a name for the provider (default: <preset>-<host>)")
	flags.StringVar(&o.APIKeyEnv, "api-key-env", "", "the environment variable that holds the API key")
	flags.StringVar(&o.Pin, "pin", "", "the SHA-256 fingerprint of a certificate no trusted authority vouches for")
	flags.BoolVar(&o.Default, "default", false, "make it the default provider")
	flags.BoolVar(&o.Vision, "vision", false, "the model sees images: the browser tools may send it screenshots")
	configFlag := flags.String("config", "", "config file to use instead of $"+app.EnvVar("CONFIG")+" or the default location")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitConfig
	}
	if flags.NArg() > 0 || o.Preset == "" || o.URL == "" {
		return usage()
	}
	path, err := config.Path(*configFlag)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitConfig
	}
	cfg, _, err := config.Load(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = config.New()
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitConfig
	}
	redactor := config.NewRedactor(cfg.Secrets(os.Getenv)...)
	if o.APIKeyEnv != "" {
		redactor.Add(os.Getenv(o.APIKeyEnv))
	}
	m := &setup.Manager{Path: path, Config: cfg, Getenv: os.Getenv}
	if err := m.AddFromFlags(context.Background(), o, redactor.Writer(stdout)); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, redactor.Error(err))
		return exitError
	}
	return exitOK
}

// historyPath is where the messages sent are kept for the fullscreen UI's
// ↑, beside the config file.
func historyPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "history.json")
}

// maxHistory is how many messages the history keeps.
const maxHistory = 500

func loadHistory(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var history []string
	if json.Unmarshal(data, &history) != nil {
		return nil
	}
	return history
}

// saveHistory returns a function that keeps the newest messages, secrets
// removed, in an owner-only file. Saving is best effort.
func saveHistory(path string, redact func(string) string) func([]string) {
	return func(history []string) {
		if len(history) > maxHistory {
			history = history[len(history)-maxHistory:]
		}
		clean := make([]string, len(history))
		for i, m := range history {
			clean[i] = redact(m)
		}
		if data, err := json.Marshal(clean); err == nil {
			config.WritePrivate(path, data)
		}
	}
}

// autoCompactTokens is the config's auto_compact_tokens, or the default.
// mcpCommand runs "mcp ...", which changes and checks the MCP servers in
// the config, as "claude mcp" does (SPEC §22).
func mcpCommand(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s mcp <command> [--config <file>]\n%s\n", app.Name, setup.MCPUsage)
		return exitConfig
	}
	// --config may come anywhere before a server's own command.
	configFile := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			configFile = v
			continue
		}
		if a == "--config" || a == "-config" {
			if i+1 >= len(args) {
				return usage()
			}
			configFile, i = args[i+1], i+1
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help" {
		return usage()
	}
	path, err := config.Path(configFile)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitConfig
	}
	cfg, _, err := config.Load(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfg = config.New()
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, err)
		return exitConfig
	}
	redactor := config.NewRedactor(cfg.Secrets(os.Getenv)...)
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", app.Name, redactor.Error(err))
		return exitError
	}
	m := &setup.Manager{Path: path, Config: cfg, Getenv: os.Getenv}
	cmd, rest := rest[0], rest[1:]
	switch cmd {
	case "add":
		name, s, err := setup.ParseMCPAdd(rest)
		if err != nil {
			fmt.Fprintf(stderr, "%s mcp add: %v\n", app.Name, err)
			return usage()
		}
		if err := m.AddMCPServer(name, s); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Added the MCP server %s: %s\nIt starts with the next session; in a running one, /mcp add starts it at once.\n", name, setup.DescribeMCP(s))
	case "add-json":
		if len(rest) != 2 {
			return usage()
		}
		s, err := setup.ParseMCPJSON(rest[1])
		if err != nil {
			return fail(err)
		}
		if err := m.AddMCPServer(rest[0], s); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Added the MCP server %s: %s\n", rest[0], setup.DescribeMCP(s))
	case "remove", "rm":
		if len(rest) != 1 {
			return usage()
		}
		if err := m.RemoveMCPServer(rest[0]); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Removed the MCP server %s.\n", rest[0])
	case "enable", "disable":
		if len(rest) != 1 {
			return usage()
		}
		if _, err := m.SetMCPServerDisabled(rest[0], cmd == "disable"); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "The MCP server %s is %sd; the change takes effect in the next session.\n", rest[0], cmd)
	case "list", "ls", "get":
		names := slices.Sorted(maps.Keys(cfg.MCPServers))
		if cmd == "get" {
			if len(rest) != 1 {
				return usage()
			}
			if cfg.MCPServers[rest[0]] == nil {
				return fail(fmt.Errorf("there is no MCP server named %q", rest[0]))
			}
			names = rest
		} else if len(rest) != 0 {
			return usage()
		}
		if len(names) == 0 {
			fmt.Fprintf(stdout, "No MCP servers are configured. Add one with\n  %s mcp add <name> -- <command> [args...]\n  %s mcp add <name> <url>\n", app.Name, app.Name)
			return exitOK
		}
		checkMCP(stdout, cfg, names, cmd == "get", redactor)
	default:
		return usage()
	}
	return exitOK
}

// checkMCP connects to each server, all at once, and says how it went;
// with tools, it names each server's tools too. Local servers run as they
// would in a session, without API keys in their environment, in the
// current directory.
func checkMCP(out io.Writer, cfg *config.Config, names []string, listTools bool, redactor *config.Redactor) {
	env := tools.ChildEnv(os.Environ(), cfg.SecretEnvNames(), cfg.Secrets(os.Getenv))
	dir, _ := os.Getwd()
	type result struct {
		text  string
		tools []string
	}
	results := make([]result, len(names))
	fmt.Fprintf(out, "Checking %d MCP server%s...\n", len(names), map[bool]string{true: "", false: "s"}[len(names) == 1])
	var wg sync.WaitGroup
	for i, name := range names {
		s := cfg.MCPServers[name]
		if s.Disabled {
			results[i].text = "switched off · " + app.Name + " mcp enable " + name + " switches it on"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			client, err := mcp.Connect(ctx, mcp.FromConfig(name, s, env, dir, os.Getenv))
			if err != nil {
				results[i].text = "failed: " + redactor.String(err.Error())
				return
			}
			defer client.Close()
			list, _, err := client.ListTools(ctx)
			if err != nil {
				results[i].text = "failed to list its tools: " + redactor.String(err.Error())
				return
			}
			text := fmt.Sprintf("connected · %s · MCP %s", tools.Count(len(list), "tool", "tools"), client.Version())
			if client.ServerInfo != "" {
				text += " · " + client.ServerInfo
			}
			results[i].text = redactor.String(text)
			for _, t := range list {
				results[i].tools = append(results[i].tools, t.Name)
			}
		}()
	}
	wg.Wait()
	for i, name := range names {
		fmt.Fprintf(out, "\n%s  %s\n  %s\n", name, redactor.String(setup.DescribeMCP(cfg.MCPServers[name])), results[i].text)
		if listTools {
			for _, t := range results[i].tools {
				fmt.Fprintf(out, "    %s\n", redactor.String(t))
			}
		}
	}
}
