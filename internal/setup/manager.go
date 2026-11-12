package setup

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"

	"debbie/internal/agent"
	"debbie/internal/config"
	"debbie/internal/effort"
	"debbie/internal/netguard"
	"debbie/internal/provider"
)

// Manager changes the providers in the config file, saving each change
// atomically.
type Manager struct {
	Path   string
	Config *config.Config
	Getenv func(string) string
	// AddSecret learns an API key the user types, so it is scrubbed from
	// all output from then on.
	AddSecret func(string)
	// Changed runs after each save, so the program can follow the config,
	// for example to keep newly named key variables from commands.
	Changed func()
	// OpenURL opens a page in the user's browser, for the wizard; nil where
	// there is none to open.
	OpenURL func(url string) error
	urls    map[string]string // presets' URLs, replaced in tests
	roots   *x509.CertPool    // stands in for the system roots in tests
}

func (m *Manager) wizard(p Prompter) *Wizard {
	return &Wizard{P: p, Config: m.Config, Getenv: m.Getenv, AddSecret: m.AddSecret, OpenURL: m.OpenURL, URLs: m.urls, roots: m.roots}
}

// List describes the providers, one line each, marking the default and
// the one in use.
func (m *Manager) List(active string) []string {
	if len(m.Config.Providers) == 0 {
		return []string{"No providers yet; /provider add sets one up."}
	}
	var lines []string
	for _, name := range sortedNames(m.Config) {
		p := m.Config.Providers[name]
		line := fmt.Sprintf("%s  %s  %s", name, p.BaseURL, p.Model)
		var marks []string
		if name == m.Config.DefaultProvider {
			marks = append(marks, "default")
		}
		if name == active {
			marks = append(marks, "in use")
		}
		if len(marks) > 0 {
			line += "  (" + strings.Join(marks, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return lines
}

// Add runs the wizard and saves the new provider. It returns its name.
func (m *Manager) Add(ctx context.Context, p Prompter) (string, error) {
	res, err := m.wizard(p).Run(ctx, "")
	if err != nil {
		return "", err
	}
	if err := m.put(res); err != nil {
		return "", err
	}
	p.Say("Saved " + summary(res.Name, res.Provider, res.MakeDefault))
	return res.Name, nil
}

// Edit runs the wizard on an existing provider.
func (m *Manager) Edit(ctx context.Context, p Prompter, name string) error {
	if err := m.exists(name); err != nil {
		return err
	}
	res, err := m.wizard(p).Run(ctx, name)
	if err != nil {
		return err
	}
	if err := m.put(res); err != nil {
		return err
	}
	p.Say("Saved " + summary(res.Name, res.Provider, res.MakeDefault))
	return nil
}

// put stores a wizard's result and saves the config, or changes nothing.
func (m *Manager) put(res *Result) error {
	return m.change(func(cfg *config.Config) {
		cfg.Providers[res.Name] = res.Provider
		if res.MakeDefault {
			cfg.DefaultProvider = res.Name
		}
	})
}

// change applies edit to a copy of the config, validates it and saves it;
// only then does the change take effect.
func (m *Manager) change(edit func(*config.Config)) error {
	next := *m.Config
	next.Providers = make(map[string]*config.Provider, len(m.Config.Providers))
	for k, v := range m.Config.Providers {
		next.Providers[k] = v
	}
	next.MCPServers = maps.Clone(m.Config.MCPServers)
	edit(&next)
	if _, err := next.Validate(); err != nil {
		return err
	}
	if err := config.Save(m.Path, &next); err != nil {
		return err
	}
	*m.Config = next
	if m.Changed != nil {
		m.Changed()
	}
	return nil
}

func (m *Manager) exists(name string) error {
	if m.Config.Providers[name] == nil {
		return fmt.Errorf("there is no provider named %q; /provider list shows them", name)
	}
	return nil
}

// Test repeats the wizard's checks for a provider and reports.
func (m *Manager) Test(ctx context.Context, p Prompter, name string) error {
	if err := m.exists(name); err != nil {
		return err
	}
	prov := *m.Config.Providers[name]
	conn, err := Connect(ctx, name, prov, m.Getenv)
	if err != nil {
		return err
	}
	for _, w := range conn.Warnings {
		p.Say("  ! " + w)
	}
	if prov.Adapter == config.AdapterAnthropic {
		return testClaude(ctx, p, name, conn)
	}
	preset, _ := FindPreset(prov.Preset)
	pr := &prober{name: name, prov: &conn.Provider, key: conn.APIKey, preset: preset}
	p.Say("Testing " + name + " (" + prov.BaseURL + ") ...")
	if healthy, err := pr.health(ctx); err != nil {
		p.Say("  ! " + err.Error())
	} else if healthy {
		p.Say("  ✓ the server is up (/health)")
	}
	models, _, err := pr.models(ctx)
	if err != nil {
		p.Say("  ! can't list the models: " + err.Error())
	} else if !slices.ContainsFunc(models, func(mi provider.ModelInfo) bool { return mi.ID == conn.Provider.Model }) && len(models) > 0 {
		p.Say("  ! the server doesn't list the model " + conn.Provider.Model)
	} else {
		p.Say("  ✓ model: " + conn.Provider.Model)
	}
	if conn.ContextWindow > 0 {
		p.Say(fmt.Sprintf("  ✓ context: %d tokens", conn.ContextWindow))
	}
	usage, err := pr.streamUsage(ctx)
	switch {
	case err != nil:
		p.Say("  ! the test request failed: " + err.Error())
		return nil
	case usage != prov.Options.StreamUsage && prov.Adapter == config.AdapterOpenAI: // the Responses API always reports it
		p.Say(fmt.Sprintf("  ! the server %s token usage now; edit the provider to update options.stream_usage", map[bool]string{true: "reports", false: "doesn't report"}[usage]))
	default:
		p.Say("  ✓ streaming works")
	}
	ok, problem, err := pr.toolCall(ctx, conn.EffortLevels)
	switch {
	case err != nil:
		p.Say("  ! the tool-calling check failed: " + err.Error())
	case !ok:
		p.Say("  ! tool calling doesn't work: " + problem + ".")
		if preset.ToolFix != "" {
			p.Say("    Likely fix: " + preset.ToolFix + ".")
		}
	default:
		p.Say("  ✓ tool calling works")
	}
	return nil
}

// Trust repeats trust on first use for a provider whose certificate
// changed, and saves the new pin (SPEC §3.2, §3.3).
func (m *Manager) Trust(ctx context.Context, p Prompter, name string) error {
	if err := m.exists(name); err != nil {
		return err
	}
	prov := m.Config.Providers[name]
	if !strings.HasPrefix(prov.BaseURL, "https://") {
		return fmt.Errorf("%s uses http, so it has no certificate to trust", name)
	}
	info, err := readCertificate(ctx, prov.BaseURL, m.roots)
	if err != nil {
		return err
	}
	pinned := ""
	if prov.TLSPinSHA256 != "" {
		pinned, _ = netguard.CanonicalPin(prov.TLSPinSHA256)
	}
	switch {
	case info.Pin == pinned:
		p.Say("The certificate matches the pinned fingerprint; nothing to change.")
		return nil
	case info.Verified && pinned == "":
		p.Say("The certificate is signed by an authority this computer trusts; it needs no pin.")
		return nil
	}
	showCertificate(p, info)
	if pinned != "" {
		p.Say("  The config pins a different certificate. If the server's certificate was replaced on purpose, trust the new one.")
	}
	ok, err := yesNo(p, "Trust this certificate?", false)
	if err != nil {
		return err
	}
	if !ok {
		p.Say("Nothing changed.")
		return nil
	}
	if err := m.change(func(cfg *config.Config) {
		next := *prov
		next.TLSPinSHA256 = info.Pin
		cfg.Providers[name] = &next
	}); err != nil {
		return err
	}
	p.Say("Saved the new pin for " + name + ".")
	return nil
}

// SetDefault makes a provider the default.
func (m *Manager) SetDefault(name string) error {
	if err := m.exists(name); err != nil {
		return err
	}
	return m.change(func(cfg *config.Config) { cfg.DefaultProvider = name })
}

// SetAutoCompact saves the context size at which a conversation with the
// provider is compacted; 0 turns auto-compaction off.
func (m *Manager) SetAutoCompact(name string, tokens int) error {
	if err := m.exists(name); err != nil {
		return err
	}
	return m.change(func(cfg *config.Config) {
		p := *cfg.Providers[name]
		p.AutoCompactTokens = &tokens
		cfg.Providers[name] = &p
	})
}

// SetReasoningBudget saves the cap on the model's thinking per reply that
// requests to a provider carry; 0 leaves it to the server.
func (m *Manager) SetReasoningBudget(name string, tokens int) error {
	if err := m.exists(name); err != nil {
		return err
	}
	return m.change(func(cfg *config.Config) {
		p := *cfg.Providers[name]
		p.ReasoningBudgetTokens = tokens
		cfg.Providers[name] = &p
	})
}

// SetPrice saves the user's price for a model, over the built-in one.
func (m *Manager) SetPrice(model string, p config.Price) error {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return errors.New("name the model the price is for")
	}
	return m.change(func(cfg *config.Config) {
		prices := maps.Clone(cfg.Prices)
		if prices == nil {
			prices = map[string]config.Price{}
		}
		prices[model] = p
		cfg.Prices = prices
	})
}

// AutoCompactFor is the context size at which a conversation with the
// provider is compacted: its own setting, else the config's, else the
// usual (agent.DefaultAutoCompact); 0 for never.
func (m *Manager) AutoCompactFor(name string) int {
	if p := m.Config.Providers[name]; p != nil && p.AutoCompactTokens != nil {
		return *p.AutoCompactTokens
	}
	if m.Config.AutoCompactTokens != nil {
		return *m.Config.AutoCompactTokens
	}
	return agent.DefaultAutoCompact
}

// SetEffort saves the reasoning effort requests to a provider carry.
func (m *Manager) SetEffort(name string, l effort.Level) error {
	if err := m.exists(name); err != nil {
		return err
	}
	return m.change(func(cfg *config.Config) {
		p := *cfg.Providers[name] // a copy: the old config stays as it was until the save succeeds
		p.ReasoningEffort = string(l)
		cfg.Providers[name] = &p
	})
}

// SetModel saves the model a provider's requests ask for, as /model
// switched the connection to it: for Claude also what the models API said
// the model supports; for an OpenAI-compatible server a stored context
// size follows the model when the server reported the new one's.
func (m *Manager) SetModel(name string, c *Conn) error {
	if err := m.exists(name); err != nil {
		return err
	}
	return m.change(func(cfg *config.Config) {
		p := *cfg.Providers[name] // a copy: the old config stays as it was until the save succeeds
		p.Model = c.Provider.Model
		if p.Adapter == config.AdapterAnthropic {
			p.ContextWindow, p.MaxOutputTokens, p.Vision = c.Provider.ContextWindow, c.Provider.MaxOutputTokens, c.Provider.Vision
		} else if p.ContextWindow != 0 && c.ContextWindow != 0 {
			p.ContextWindow = c.ContextWindow
		}
		cfg.Providers[name] = &p
	})
}

// Remove deletes a provider after asking. When it was the default, the
// first remaining provider by name becomes the default.
func (m *Manager) Remove(ctx context.Context, p Prompter, name string) error {
	if err := m.exists(name); err != nil {
		return err
	}
	ok, err := yesNo(p, "Remove "+name+"?", false)
	if err != nil || !ok {
		return err
	}
	return m.change(func(cfg *config.Config) {
		delete(cfg.Providers, name)
		if cfg.DefaultProvider == name {
			cfg.DefaultProvider = ""
			if names := sortedNames(cfg); len(names) > 0 {
				cfg.DefaultProvider = names[0]
				p.Say(names[0] + " is now the default provider.")
			}
		}
	})
}

// AddOptions are the flags of "debbie provider add".
type AddOptions struct {
	Preset, URL, Model, Name, APIKeyEnv, Pin string
	Default                                  bool
	Vision                                   bool // the model sees images
}

// AddFromFlags adds a provider without asking anything, for scripts. It
// runs the same checks as the wizard and reports to out. A certificate no
// trusted authority vouches for needs its fingerprint in --pin.
func (m *Manager) AddFromFlags(ctx context.Context, o AddOptions, out io.Writer) error {
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	preset, ok := FindPreset(o.Preset)
	if !ok {
		ids := make([]string, len(Presets))
		for i, p := range Presets {
			ids[i] = p.ID
		}
		return fmt.Errorf("--preset %q: use one of %s", o.Preset, strings.Join(ids, ", "))
	}
	if o.URL == "" && preset.Cloud {
		o.URL = preset.URL
		if u := m.urls[preset.ID]; u != "" {
			o.URL = u
		}
	}
	if o.APIKeyEnv == "" && preset.Cloud {
		o.APIKeyEnv = preset.KeyEnv
	}
	if !strings.Contains(o.URL, "://") {
		return fmt.Errorf("--url %q: give the scheme, http:// or https://", o.URL)
	}
	if err := checkURL(o.URL); err != nil {
		return fmt.Errorf("--url: %v", err)
	}
	prov := &config.Provider{
		Preset: preset.ID, Adapter: preset.Adapter, BaseURL: strings.TrimRight(o.URL, "/"), Model: o.Model, Vision: o.Vision,
		APIKeyEnv:            o.APIKeyEnv,
		HeaderTimeoutSeconds: config.DefaultHeaderTimeoutSeconds,
		StreamIdleSeconds:    config.DefaultStreamIdleSeconds,
		Options:              config.ProviderOptions{StreamUsage: true},
	}
	if strings.HasPrefix(prov.BaseURL, "https://") {
		info, err := readCertificate(ctx, prov.BaseURL, m.roots)
		if err != nil {
			return err
		}
		switch {
		case o.Pin != "":
			want, err := netguard.CanonicalPin(o.Pin)
			if err != nil {
				return fmt.Errorf("--pin: %v", err)
			}
			if want != info.Pin {
				return fmt.Errorf("the certificate's SHA-256 fingerprint is %s, not the one given with --pin", info.Fingerprint)
			}
			prov.TLSPinSHA256 = info.Pin
		case !info.Verified:
			return fmt.Errorf("no authority this computer trusts vouches for the certificate of %s (%s). "+
				"Its SHA-256 fingerprint is %s; if that is the server's, add --pin %s", prov.BaseURL, info.VerifyError, info.Fingerprint, info.Pin)
		}
	}
	key := ""
	if o.APIKeyEnv != "" {
		if key = m.Getenv(o.APIKeyEnv); key == "" {
			say("warning: %s is not set; testing without an API key", o.APIKeyEnv)
		}
	}
	if prov.Adapter == config.AdapterAnthropic {
		if err := addClaudeFromFlags(ctx, prov, key, say); err != nil {
			return err
		}
	} else if err := m.checkFlags(ctx, prov, key, preset, say); err != nil {
		return err
	}
	name := o.Name
	if name == "" {
		name = m.wizard(nil).suggestName(preset, prov.BaseURL)
	}
	if err := m.wizard(nil).validName(name); err != nil {
		return fmt.Errorf("--name %q: %v", name, err)
	}
	makeDefault := o.Default || len(m.Config.Providers) == 0
	if err := m.put(&Result{Name: name, Provider: prov, MakeDefault: makeDefault}); err != nil {
		return err
	}
	say("Saved %s", summary(name, prov, makeDefault))
	return nil
}

// checkFlags runs the checks for AddFromFlags.
func (m *Manager) checkFlags(ctx context.Context, prov *config.Provider, key string, preset Preset, say func(string, ...any)) error {
	u, _ := url.Parse(prov.BaseURL)
	pr := &prober{name: u.Host, prov: prov, key: key, preset: preset}
	if _, err := pr.health(ctx); err != nil {
		return err
	}
	models, alt, err := pr.models(ctx)
	if err != nil {
		return fmt.Errorf("can't list the models: %w", err)
	}
	if alt != "" {
		return fmt.Errorf("the API answers at %s; use that as --url", alt)
	}
	ids := make([]string, len(models))
	for i, mi := range models {
		ids[i] = mi.ID
	}
	switch {
	case prov.Model == "" && len(models) == 1:
		prov.Model = models[0].ID
	case prov.Model == "":
		return fmt.Errorf("choose a model with --model: %s", strings.Join(ids, ", "))
	case len(models) > 0 && !slices.Contains(ids, prov.Model):
		return fmt.Errorf("the server has no model %q; it has %s", prov.Model, strings.Join(ids, ", "))
	}
	var levels []effort.Level
	if preset.Props {
		if info, err := pr.serverInfo(ctx); err == nil {
			prov.ContextWindow, levels = info.ContextWindow, info.EffortLevels
		}
	}
	for _, mi := range models {
		if mi.ID == prov.Model && prov.ContextWindow == 0 {
			prov.ContextWindow = mi.ContextWindow
		}
	}
	if prov.ContextWindow == 0 {
		say("warning: the server doesn't report the context size; set context_window in the config")
	}
	usage, err := pr.streamUsage(ctx)
	if err != nil {
		return fmt.Errorf("the test request failed: %w", err)
	}
	prov.Options.StreamUsage = usage
	ok, problem, err := pr.toolCall(ctx, levels)
	switch {
	case err != nil:
		say("warning: the tool-calling check failed: %v", err)
	case !ok:
		say("warning: tool calling doesn't work: %s.", problem)
		if preset.ToolFix != "" {
			say("Likely fix: %s.", preset.ToolFix)
		}
	}
	return nil
}

// summary describes a saved provider in one line.
func summary(name string, p *config.Provider, isDefault bool) string {
	parts := []string{name, p.BaseURL, p.Model}
	if p.ContextWindow > 0 {
		parts = append(parts, kTokens(p.ContextWindow)+" context")
	}
	if isDefault {
		parts = append(parts, "default")
	}
	return strings.Join(parts, " · ")
}

func sortedNames(cfg *config.Config) []string {
	var names []string
	for name := range cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
