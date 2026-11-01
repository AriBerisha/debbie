package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"debbie/internal/app"
	"debbie/internal/effort"
	"debbie/internal/netguard"
)

var (
	providerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

// reservedBodyKeys are request fields each adapter sets itself, which
// options.extra_body may not override.
var reservedBodyKeys = map[string][]string{
	AdapterOpenAI: {"model", "messages", "stream", "stream_options", "tools", "tool_choice",
		"functions", "function_call", "max_tokens", "max_completion_tokens", "n", "chat_template_kwargs",
		"reasoning_effort", "reasoning_budget_tokens", "thinking_budget_tokens"},
	AdapterAnthropic: {"model", "messages", "system", "stream", "tools", "tool_choice", "max_tokens",
		"thinking", "output_config", "cache_control", "temperature", "top_p", "top_k"},
}

// reservedBodyHints point from a reserved field to the setting for it.
var reservedBodyHints = map[string]string{
	"chat_template_kwargs": "; use options.chat_template_kwargs",
	"reasoning_effort":     "; use reasoning_effort",
	// llama-server reads both names; the adapter sends both.
	"reasoning_budget_tokens": "; use reasoning_budget_tokens",
	"thinking_budget_tokens":  "; use reasoning_budget_tokens",
}

// ValidationError lists every problem found in a config.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  %s", len(e.Problems), strings.Join(e.Problems, "\n  "))
}

// Validate checks the config. The error, a *ValidationError, lists every
// problem; the warnings describe settings that are allowed but risky.
func (c *Config) Validate() (warnings []string, err error) {
	v := &validator{}
	c.validate(v)
	if len(v.problems) > 0 {
		return v.warnings, &ValidationError{Problems: v.problems}
	}
	return v.warnings, nil
}

type validator struct {
	problems []string
	warnings []string
}

func (v *validator) errorf(path, format string, args ...any) {
	v.problems = append(v.problems, path+": "+fmt.Sprintf(format, args...))
}

func (v *validator) warnf(format string, args ...any) {
	v.warnings = append(v.warnings, fmt.Sprintf(format, args...))
}

func (c *Config) validate(v *validator) {
	if c.Version > CurrentVersion {
		v.errorf("version", "%d is newer than this build of %s supports (%d)", c.Version, app.Name, CurrentVersion)
	} else if c.Version < 0 {
		v.errorf("version", "must be %d", CurrentVersion)
	}

	names := sortedKeys(c.Providers)
	byLower := map[string]string{}
	for _, name := range names {
		path := "providers." + name
		if !providerNameRE.MatchString(name) {
			v.errorf(path, "invalid name: use up to 64 letters, digits, '.', '_' and '-'")
		}
		if other, dup := byLower[strings.ToLower(name)]; dup {
			v.errorf(path, "same name as %q apart from case", other)
		}
		byLower[strings.ToLower(name)] = name
		if p := c.Providers[name]; p == nil {
			v.errorf(path, "must be an object")
		} else {
			p.validate(v, name)
		}
	}
	switch {
	case c.DefaultProvider != "":
		if _, ok := c.Providers[c.DefaultProvider]; !ok {
			v.errorf("default_provider", "there is no provider named %q", c.DefaultProvider)
		}
	case len(c.Providers) > 1:
		v.errorf("default_provider", "not set; choose one of %s", strings.Join(names, ", "))
	}

	switch c.TUI {
	case "", TUIFullscreen, TUIClassic:
	default:
		v.errorf("tui", "unknown UI %q (use fullscreen or classic)", c.TUI)
	}
	if n := c.AutoCompactTokens; n != nil && *n != 0 && *n < MinAutoCompactTokens {
		v.errorf("auto_compact_tokens", "%d is too small: use 0 to turn auto-compaction off, or at least %d", *n, MinAutoCompactTokens)
	}

	switch c.ApprovalMode {
	case ApprovalPlan, ApprovalDefault, ApprovalEditor, ApprovalAutoEdit:
	case ApprovalYolo:
		v.errorf("approval_mode", "yolo can't be stored in the config; start with --yolo or switch with /mode yolo, which both ask for confirmation")
	default:
		v.errorf("approval_mode", "unknown mode %q (use plan, default, editor or auto-edit)", c.ApprovalMode)
	}

	if c.Subagents.Provider != "" {
		if _, ok := c.Providers[c.Subagents.Provider]; !ok {
			v.errorf("subagents.provider", "there is no provider named %q", c.Subagents.Provider)
		}
	}
	if c.Subagents.MaxParallel < 1 {
		v.errorf("subagents.max_parallel", "must be at least 1")
	}
	if c.Subagents.MaxTurns < 1 {
		v.errorf("subagents.max_turns", "must be at least 1")
	}
	if c.MaxTurns < 0 {
		v.errorf("max_turns", "must be at least 1, or left out for 100")
	}
	for model, p := range c.Prices {
		if strings.TrimSpace(model) == "" {
			v.errorf("prices", "a price needs a model ID as its key")
		}
		for _, f := range []struct {
			name  string
			value float64
		}{{"input", p.Input}, {"output", p.Output}, {"cache_read", p.CacheRead}, {"cache_write", p.CacheWrite},
			{"long_input", p.LongInput}, {"long_output", p.LongOutput}, {"long_cache_read", p.LongCacheRead}, {"long_cache_write", p.LongCacheWrite}} {
			if f.value < 0 {
				v.errorf("prices."+model+"."+f.name, "must not be negative")
			}
		}
		if p.LongFrom < 0 {
			v.errorf("prices."+model+".long_from", "must not be negative")
		}
	}

	switch p := c.Shell.Program; p {
	case "", "auto", "bash", "sh", "powershell", "pwsh", "cmd":
	default:
		if !filepath.IsAbs(p) {
			v.errorf("shell.program", "unknown shell %q (use auto, bash, sh, powershell, pwsh, cmd or a full path)", p)
		}
	}
	if c.Shell.TimeoutSeconds < 1 || c.Shell.TimeoutSeconds > MaxShellTimeoutSeconds {
		v.errorf("shell.timeout_seconds", "must be between 1 and %d", MaxShellTimeoutSeconds)
	}
	for i, name := range c.Shell.StripEnv {
		if !envNameRE.MatchString(name) {
			v.errorf(fmt.Sprintf("shell.strip_env[%d]", i), "%q is not an environment variable name", name)
		}
	}

	for _, file := range sortedKeys(c.TrustedInstructionFiles) {
		path := fmt.Sprintf("trusted_instruction_files[%q]", file)
		if !filepath.IsAbs(file) {
			v.errorf(path, "the path must be absolute")
		}
		if !sha256HexRE.MatchString(c.TrustedInstructionFiles[file]) {
			v.errorf(path, "the value must be a SHA-256 hash (64 hex digits)")
		}
	}

	c.Web.validate(v)
	c.validateMCP(v)
}

func (p *Provider) validate(v *validator, name string) {
	path := "providers." + name
	switch p.Adapter {
	case AdapterOpenAI, AdapterAnthropic:
	case "":
		v.errorf(path+".adapter", "missing (use openai or anthropic)")
	default:
		v.errorf(path+".adapter", "unknown adapter %q (use openai or anthropic)", p.Adapter)
	}

	u, err := checkBaseURL(p.BaseURL)
	if err != nil {
		v.errorf(path+".base_url", "%v", err)
	}
	if p.Model == "" {
		v.errorf(path+".model", "missing")
	}
	if p.ContextWindow < 0 {
		v.errorf(path+".context_window", "must not be negative")
	}
	if p.MaxOutputTokens < 0 {
		v.errorf(path+".max_output_tokens", "must not be negative")
	} else if p.ContextWindow > 0 && p.MaxOutputTokens >= p.ContextWindow {
		v.errorf(path+".max_output_tokens", "must be smaller than context_window (%d)", p.ContextWindow)
	}
	if n := p.AutoCompactTokens; n != nil && *n != 0 && *n < MinAutoCompactTokens {
		v.errorf(path+".auto_compact_tokens", "%d is too small: use 0 to turn auto-compaction off, or at least %d", *n, MinAutoCompactTokens)
	}

	if p.APIKey != "" && p.APIKeyEnv != "" {
		v.errorf(path, "set api_key_env or api_key, not both")
	}
	if p.APIKeyEnv != "" && !envNameRE.MatchString(p.APIKeyEnv) {
		v.errorf(path+".api_key_env", "%q is not an environment variable name", p.APIKeyEnv)
	}
	if p.APIKey != "" && len(p.APIKey) < MinSecretLength {
		v.warnf("%s.api_key is shorter than %d characters, too short to redact reliably from output", path, MinSecretLength)
	}
	if u != nil && u.Scheme == "http" && (p.APIKey != "" || p.APIKeyEnv != "") {
		v.warnf("provider %q uses plain http, so its API key travels unencrypted", name)
	}

	if p.TLSPinSHA256 != "" {
		if _, err := netguard.ParsePin(p.TLSPinSHA256); err != nil {
			v.errorf(path+".tls_pin_sha256", "%v", err)
		} else if u != nil && u.Scheme != "https" {
			v.errorf(path+".tls_pin_sha256", "a certificate pin needs an https base_url")
		}
	}
	if p.HeaderTimeoutSeconds < 0 {
		v.errorf(path+".header_timeout_seconds", "must not be negative")
	}
	if p.StreamIdleSeconds < 0 {
		v.errorf(path+".stream_idle_seconds", "must not be negative")
	}

	if len(p.Options.ChatTemplateKwargs) > 0 && p.Adapter == AdapterAnthropic {
		v.errorf(path+".options.chat_template_kwargs", "only applies to the openai adapter")
	}
	for _, key := range sortedKeys(p.Options.ExtraBody) {
		if slices.Contains(reservedBodyKeys[p.Adapter], key) {
			v.errorf(joinPath(path+".options.extra_body", key), "%s sets this field itself%s", app.Name, reservedBodyHints[key])
		}
	}

	if p.ReasoningEffort != "" {
		if _, ok := effort.Parse(p.ReasoningEffort); !ok {
			v.errorf(path+".reasoning_effort", "unknown level %q (use none, minimal, low, medium, high, xhigh or max)", p.ReasoningEffort)
		}
		if _, set := p.Options.ChatTemplateKwargs["reasoning_effort"]; set {
			v.errorf(path+".options.chat_template_kwargs.reasoning_effort",
				"conflicts with reasoning_effort, which the server applies instead; keep only one")
		}
	}
	switch {
	case p.ReasoningBudgetTokens < 0:
		v.errorf(path+".reasoning_budget_tokens", "must not be negative")
	case p.ReasoningBudgetTokens > 0 && p.Adapter == AdapterAnthropic:
		v.errorf(path+".reasoning_budget_tokens", "only applies to the openai adapter; Claude's thinking follows reasoning_effort")
	}
}

// checkBaseURL checks a provider URL and returns it parsed. Messages never
// repeat the URL, which could hold credentials.
func checkBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("missing")
	}
	if !strings.Contains(raw, "://") {
		return nil, errors.New("missing scheme: start with http:// or https://")
	}
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("not a valid URL: %v", err)
	}
	if u.User != nil {
		return nil, errors.New("credentials in the URL are not supported; use api_key_env")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("must not contain a query (?) or fragment (#)")
	}
	for _, r := range u.Hostname() {
		if r > 0x7f {
			return nil, errors.New("write the host name in its ASCII form (xn--...)")
		}
	}
	if _, err := netguard.EndpointFromURL(raw, ""); err != nil {
		return nil, err
	}
	return u, nil
}

func (w *Web) validate(v *validator) {
	switch w.Mode {
	case WebOff, WebSafe:
	case WebYolo:
		v.errorf("web.mode", "yolo can't be stored in the config; it lasts one session (--web yolo or /web yolo)")
	default:
		v.errorf("web.mode", "unknown mode %q (use off or safe)", w.Mode)
	}

	switch w.SearchEngine {
	case EngineDuckDuckGo, EngineBrave, EngineGoogle:
		if w.SearchURL != "" {
			v.warnf("web.search_url is ignored with search engine %q", w.SearchEngine)
		}
	case EngineSearXNG, EngineCustom:
		if err := checkSearchURL(w.SearchURL); err != nil {
			v.errorf("web.search_url", "%v", err)
		}
	default:
		v.errorf("web.search_engine", "unknown engine %q (use duckduckgo, brave, google, searxng or custom)", w.SearchEngine)
	}

	checkHosts(v, "web.allow_domains", w.AllowDomains)
	checkHosts(v, "web.block_domains", w.BlockDomains)
	checkHosts(v, "web.allow_private_hosts", w.AllowPrivateHosts)
}

func checkSearchURL(s string) error {
	if s == "" {
		return errors.New("required for this search engine, for example https://searx.lan/search?q={q}")
	}
	if !strings.Contains(s, "{q}") {
		return errors.New("must contain {q} where the query goes")
	}
	u, err := url.Parse(strings.ReplaceAll(s, "{q}", "q"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http or https URL")
	}
	return nil
}

func checkHosts(v *validator, path string, hosts []string) {
	for i, h := range hosts {
		if !validHost(h) {
			v.errorf(fmt.Sprintf("%s[%d]", path, i), "%q is not a host name or IP address", h)
		}
	}
}

func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}
