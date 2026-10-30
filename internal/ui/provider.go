package ui

import (
	"context"
	"errors"
	"sort"
	"strings"

	"debbie/internal/config"
	"debbie/internal/provider"
	"debbie/internal/setup"
)

// providerUsage lists the /provider subcommands (SPEC §6).
const providerUsage = "/provider list | add | edit <name> | test [name] | trust [name] | use <name> | default <name> | remove <name>"

// said is a line a /provider subcommand said. The fullscreen UI shows them
// once it is back from line mode, where the subcommands ask their
// questions: what line mode printed is out of sight there.
type said struct {
	text string
	k    ink
}

// recorder is the console as the subcommands' Prompter: it says what they
// say, and keeps it.
type recorder struct {
	*Console
	lines []said
}

// Say says a line of the wizard's, or of a check's.
func (r *recorder) Say(text string) {
	r.Console.Say(text)
	k := ink{}
	if strings.HasPrefix(text, "  ! ") {
		k = inkYellow
	}
	r.lines = append(r.lines, said{text, k})
}

// tell says a line of the command's own, in the color k names.
func (r *recorder) tell(text string, k ink) {
	line := text
	switch k {
	case inkYellow:
		line = r.st.yellow(text)
	case inkRed:
		line = r.st.red(text)
	case inkGray:
		line = r.st.gray(text)
	}
	r.r.println(line)
	r.lines = append(r.lines, said{text, k})
}

// providerCommand runs /provider and its subcommands, and returns what it
// said. A provider just added, or the one in use just edited, is used at
// once: it was set up to be.
func (c *chat) providerCommand(arg string) []said {
	out := &recorder{Console: c.Console}
	m := c.opts.Providers
	if m == nil {
		out.tell("There is no config file to keep providers in.", inkYellow)
		return out.lines
	}
	sub, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	active := c.opts.Status.Provider
	ctx, stop := c.Interruptible()
	defer stop()

	var err error
	switch sub {
	case "":
		err = c.providerPicker(ctx, out)
	case "list":
		for _, line := range m.List(active) {
			out.tell(line, ink{})
		}
		out.tell(providerUsage, inkGray)
	case "add":
		var added string
		if added, err = m.Add(ctx, out); err == nil {
			err = c.useProvider(ctx, added, out)
		}
	case "edit":
		if err = needName(name, "edit"); err == nil {
			if err = m.Edit(ctx, out, name); err == nil && name == active {
				err = c.useProvider(ctx, name, out)
			}
		}
	case "test":
		err = m.Test(ctx, out, orActive(name, active))
	case "trust":
		err = m.Trust(ctx, out, orActive(name, active))
	case "use":
		if err = needName(name, "use"); err == nil {
			err = c.useProvider(ctx, name, out)
		}
	case "default":
		if err = needName(name, "default"); err == nil {
			if err = m.SetDefault(name); err == nil {
				out.tell(name+" is now the default provider.", ink{})
			}
		}
	case "remove":
		if err = needName(name, "remove"); err == nil {
			err = m.Remove(ctx, out, name)
			if err == nil && name == active && m.Config.Providers[name] == nil {
				// The session moves on to the default provider, if any is left.
				if next := m.Config.DefaultProvider; next != "" {
					err = c.useProvider(ctx, next, out)
				} else {
					out.tell("This session keeps using "+name+" until it ends.", inkYellow)
				}
			}
		}
	default:
		out.tell("Unknown subcommand "+sub+"; "+providerUsage, inkYellow)
	}
	switch {
	case errors.Is(err, setup.ErrCancelled), errors.Is(err, context.Canceled):
		out.tell("Cancelled; nothing changed.", inkYellow)
	case err != nil:
		out.tell("error: "+errorText(err), inkRed)
	}
	return out.lines
}

func needName(name, sub string) error {
	if name == "" {
		return errors.New("name the provider: /provider " + sub + " <name>")
	}
	return nil
}

func orActive(name, active string) string {
	if name == "" {
		return active
	}
	return name
}

// useProvider switches the session to another provider.
func (c *chat) useProvider(ctx context.Context, name string, out *recorder) error {
	if c.opts.Use == nil {
		return errors.New("this session can't switch providers")
	}
	conn, err := c.opts.Use(ctx, name)
	if err != nil {
		return err
	}
	for _, w := range conn.Warnings {
		out.tell("warning: "+w, inkYellow)
	}
	c.opts.Status.Provider = conn.Name
	c.opts.Status.Model = conn.Provider.Model
	c.opts.Status.ContextWindow = conn.ContextWindow
	c.opts.EffortLevels = conn.EffortLevels
	c.opts.AutoCompact = conn.AutoCompact
	c.lastUsage = nil
	out.tell("Now using "+conn.Name+" · "+conn.Provider.Model+".", ink{})
	return nil
}

// providerPicker asks which provider to switch to, or to add one.
func (c *chat) providerPicker(ctx context.Context, out *recorder) error {
	m := c.opts.Providers
	names := providerNames(m.Config)
	ch := setup.Choice{Question: "Which provider?"}
	for i, name := range names {
		ch.Options = append(ch.Options, setup.Option{Label: name, Detail: providerDetail(m.Config, name, c.opts.Status.Provider), Key: name})
		if name == c.opts.Status.Provider {
			ch.Default = i
		}
	}
	ch.Options = append(ch.Options, setup.Option{Label: "Add a provider", Detail: "set one up, step by step", Key: "add"})
	i, _, err := c.Choose(ch)
	if err != nil {
		return err
	}
	if i == len(names) {
		added, err := m.Add(ctx, out)
		if err != nil {
			return err
		}
		return c.useProvider(ctx, added, out)
	}
	if names[i] == c.opts.Status.Provider {
		out.tell("Already using "+names[i]+".", inkGray)
		return nil
	}
	return c.useProvider(ctx, names[i], out)
}

// providerNames lists the configured providers, sorted.
func providerNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// providerDetail says what a provider is, beside its name: its model, its
// context, whether it is the default and whether it is in use.
func providerDetail(cfg *config.Config, name, active string) string {
	p := cfg.Providers[name]
	parts := []string{p.Model}
	if p.ContextWindow > 0 {
		parts = append(parts, kTokens(p.ContextWindow)+" context")
	}
	if cfg.DefaultProvider == name {
		parts = append(parts, "default")
	}
	if name == active {
		parts = append(parts, "in use")
	}
	return strings.Join(parts, " · ")
}

// errorText is an error as the UI shows it, with what fixes it where that
// is known: a provider without an API key.
func errorText(err error) string {
	var noKey *provider.NoKeyError
	if errors.As(err, &noKey) {
		return setup.NoKeyText(noKey)
	}
	return err.Error()
}

// showSaid shows what a /provider subcommand said in line mode, which the
// fullscreen UI covered again when it came back.
func (f *fullscreen) showSaid(lines []said) {
	check := f.term.sym().check
	var text []string
	k := ink{}
	flush := func() {
		if len(text) > 0 {
			f.say(strings.Join(text, "\n"), k)
			text = nil
		}
	}
	for _, l := range lines {
		if l.k != k {
			flush()
			k = l.k
		}
		t := l.text
		if strings.HasPrefix(t, "  ✓ ") {
			t = "  " + check + t[len("  ✓"):]
		}
		text = append(text, t)
	}
	flush()
}

// Interruptible returns a context that Ctrl+C cancels, for a command that
// sends requests, and a function to call when the command is done. While
// it runs, Ctrl+C also ends a question the command is asking.
func (c *Console) Interruptible() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
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
	c.cmdDone = ctx.Done()
	return ctx, func() {
		close(done)
		cancel()
		c.cmdDone = nil
	}
}
