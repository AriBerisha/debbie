// Package config loads, validates and saves the configuration file.
//
// The file holds provider definitions and settings. API keys normally stay
// in environment variables; one is stored in the file only when the user
// chooses that in the setup wizard. The file is private to the user (mode
// 0600 in a 0700 directory, or an owner-only ACL on Windows), writes are
// atomic, and Load warns when other users can read or change the file.
package config

import (
	"encoding/json"
	"maps"
	"runtime"
	"slices"
	"strings"
	"time"
)

// CurrentVersion is the schema version this build reads and writes.
const CurrentVersion = 1

// Adapters.
const (
	AdapterOpenAI    = "openai"
	AdapterAnthropic = "anthropic"
)

// Approval modes (SPEC §9).
const (
	ApprovalPlan     = "plan"
	ApprovalDefault  = "default"
	ApprovalEditor   = "editor" // edits without asking, runs no commands
	ApprovalAutoEdit = "auto-edit"
	ApprovalYolo     = "yolo"
)

// Web modes (SPEC §19).
const (
	WebOff  = "off"
	WebSafe = "safe"
	WebYolo = "yolo"
)

// Search engines (SPEC §20.4).
const (
	EngineDuckDuckGo = "duckduckgo"
	EngineBrave      = "brave"
	EngineGoogle     = "google"
	EngineSearXNG    = "searxng"
	EngineCustom     = "custom"
)

// Defaults for settings the file leaves out.
const (
	DefaultHeaderTimeoutSeconds = 600
	DefaultStreamIdleSeconds    = 180
	DefaultShellTimeoutSeconds  = 120
	MaxShellTimeoutSeconds      = 600
	DefaultSubagentMaxParallel  = 1
	DefaultSubagentMaxTurns     = 30
)

// DefaultStripEnv lists the variables removed from child processes when the
// config doesn't name its own.
func DefaultStripEnv() []string {
	return []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}
}

// Config is the content of config.json.
type Config struct {
	Version                 int                  `json:"version"`
	DefaultProvider         string               `json:"default_provider"`
	Providers               map[string]*Provider `json:"providers"`
	ApprovalMode            string               `json:"approval_mode"`
	Subagents               Subagents            `json:"subagents"`
	Shell                   Shell                `json:"shell"`
	TrustedInstructionFiles map[string]string    `json:"trusted_instruction_files"`
	Web                     Web                  `json:"web"`
	// TUI chooses the interactive UI: "fullscreen", the default, or
	// "classic", line mode.
	TUI string `json:"tui,omitempty"`
	// AutoCompactTokens is the context size, in tokens, at which the
	// conversation is summarized to make room; 0 turns that off, and
	// leaving it out means 180000.
	AutoCompactTokens *int `json:"auto_compact_tokens,omitempty"`
	// MCPServers are the MCP servers whose tools the model can use, by
	// name (mcp.go).
	MCPServers map[string]*MCPServer `json:"mcp_servers,omitempty"`
	// MaxTurns is how many model requests one message may lead to before
	// the turn stops (SPEC §7); leaving it out means 100.
	MaxTurns int `json:"max_turns,omitempty"`
	// Prices are the user's prices per million tokens, by model ID (or the
	// start of one), over the built-in list prices for Claude
	// (internal/cost). /cost sets them.
	Prices map[string]Price `json:"prices,omitempty"`
}

// Price is what a model costs, in USD per million tokens. A model priced
// by its prompt's length (Haiku 5.5) pays the Long prices for a request
// whose input tokens, cache reads and writes included, exceed LongFrom.
type Price struct {
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cache_read,omitempty"`
	CacheWrite     float64 `json:"cache_write,omitempty"`
	LongFrom       int     `json:"long_from,omitempty"`
	LongInput      float64 `json:"long_input,omitempty"`
	LongOutput     float64 `json:"long_output,omitempty"`
	LongCacheRead  float64 `json:"long_cache_read,omitempty"`
	LongCacheWrite float64 `json:"long_cache_write,omitempty"`
}

// MinAutoCompactTokens is the smallest auto_compact_tokens besides 0.
const MinAutoCompactTokens = 1000

// The interactive UIs.
const (
	TUIFullscreen = "fullscreen"
	TUIClassic    = "classic"
)

// Provider is one model endpoint.
type Provider struct {
	Preset               string          `json:"preset,omitempty"`
	Adapter              string          `json:"adapter"`
	BaseURL              string          `json:"base_url"`
	Model                string          `json:"model"`
	ContextWindow        int             `json:"context_window,omitempty"`
	MaxOutputTokens      int             `json:"max_output_tokens,omitempty"`
	ReasoningEffort      string          `json:"reasoning_effort,omitempty"`
	APIKeyEnv            string          `json:"api_key_env,omitempty"`
	APIKey               string          `json:"api_key,omitempty"`
	TLSPinSHA256         string          `json:"tls_pin_sha256,omitempty"`
	HeaderTimeoutSeconds int             `json:"header_timeout_seconds,omitempty"`
	StreamIdleSeconds    int             `json:"stream_idle_seconds,omitempty"`
	Options              ProviderOptions `json:"options"`
	Vision               bool            `json:"vision"`
	SystemPromptAppend   string          `json:"system_prompt_append,omitempty"`
	// AutoCompactTokens is the context size, in tokens, at which a
	// conversation with this provider is summarized to make room; 0 turns
	// that off, and leaving it out means the config's auto_compact_tokens.
	AutoCompactTokens *int `json:"auto_compact_tokens,omitempty"`
}

// ProviderOptions tune requests to one provider.
type ProviderOptions struct {
	// StreamUsage asks the server for token usage in streamed responses.
	StreamUsage bool `json:"stream_usage"`
	// ChatTemplateKwargs is sent as chat_template_kwargs: variables for the
	// server's Jinja chat template (llama-server, vLLM), such as
	// {"enable_thinking": false}.
	ChatTemplateKwargs map[string]json.RawMessage `json:"chat_template_kwargs,omitempty"`
	// ExtraBody adds top-level request fields for server-specific
	// parameters, such as top_k or reasoning_format. Fields the adapter sets
	// itself can't be overridden.
	ExtraBody map[string]json.RawMessage `json:"extra_body,omitempty"`
	// SendReasoning sends the model's reasoning back with the tool-call
	// steps of the current turn, for chat templates that keep it, such as
	// Qwen's (SPEC §4.3). Off by default.
	SendReasoning bool `json:"send_reasoning,omitempty"`
	// WorkspaceID is the Claude Console workspace that requests run in, for
	// an API key that isn't scoped to one; it goes in the
	// anthropic-workspace-id header (SPEC §4.4).
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Subagents configures the task tool (SPEC §10).
type Subagents struct {
	Provider    string `json:"provider"`
	MaxParallel int    `json:"max_parallel"`
	MaxTurns    int    `json:"max_turns"`
}

// Shell configures the shell tool (SPEC §3.5, §8, §21).
type Shell struct {
	// Program runs the commands: "auto" (Git Bash when installed, else
	// Windows PowerShell; sh on Unix), "bash", "sh", "powershell", "pwsh",
	// "cmd", or a full path.
	Program        string   `json:"program,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	StripEnv       []string `json:"strip_env"`
}

// Web configures web access (SPEC §19, §20).
type Web struct {
	Mode              string   `json:"mode"`
	SearchEngine      string   `json:"search_engine"`
	SearchURL         string   `json:"search_url"`
	AllowDomains      []string `json:"allow_domains"`
	BlockDomains      []string `json:"block_domains"`
	AllowPrivateHosts []string `json:"allow_private_hosts"`
}

// New returns an empty config with every default filled in.
func New() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

// applyDefaults fills in settings the file left out.
func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = CurrentVersion
	}
	if c.Providers == nil {
		c.Providers = map[string]*Provider{}
	}
	for _, p := range c.Providers {
		if p == nil {
			continue // reported by Validate
		}
		if p.HeaderTimeoutSeconds == 0 {
			p.HeaderTimeoutSeconds = DefaultHeaderTimeoutSeconds
		}
		if p.StreamIdleSeconds == 0 {
			p.StreamIdleSeconds = DefaultStreamIdleSeconds
		}
	}
	if c.ApprovalMode == "" {
		c.ApprovalMode = ApprovalDefault
	}
	if c.Subagents.MaxParallel == 0 {
		c.Subagents.MaxParallel = DefaultSubagentMaxParallel
	}
	if c.Subagents.MaxTurns == 0 {
		c.Subagents.MaxTurns = DefaultSubagentMaxTurns
	}
	if c.Shell.TimeoutSeconds == 0 {
		c.Shell.TimeoutSeconds = DefaultShellTimeoutSeconds
	}
	if c.Shell.StripEnv == nil {
		c.Shell.StripEnv = DefaultStripEnv()
	}
	if c.TrustedInstructionFiles == nil {
		c.TrustedInstructionFiles = map[string]string{}
	}
	if c.Web.Mode == "" {
		c.Web.Mode = WebOff
	}
	if c.Web.SearchEngine == "" {
		c.Web.SearchEngine = EngineDuckDuckGo
	}
	for _, list := range []*[]string{&c.Web.AllowDomains, &c.Web.BlockDomains, &c.Web.AllowPrivateHosts} {
		if *list == nil {
			*list = []string{}
		}
	}
}

// ActiveProvider returns the provider used when none is chosen explicitly:
// default_provider, or the only provider when there is exactly one.
func (c *Config) ActiveProvider() (string, *Provider, bool) {
	name := c.DefaultProvider
	if name == "" && len(c.Providers) == 1 {
		for n := range c.Providers {
			name = n
		}
	}
	p, ok := c.Providers[name]
	return name, p, ok && p != nil
}

// HeaderTimeout is how long to wait for response headers.
func (p *Provider) HeaderTimeout() time.Duration {
	return seconds(p.HeaderTimeoutSeconds, DefaultHeaderTimeoutSeconds)
}

// StreamIdle is how long a streamed response may go without a chunk.
func (p *Provider) StreamIdle() time.Duration {
	return seconds(p.StreamIdleSeconds, DefaultStreamIdleSeconds)
}

func seconds(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

// SecretEnvNames returns the environment variables that hold secrets: every
// provider's api_key_env, the names in shell.strip_env and every MCP
// server's bearer_token_env. They are removed
// from the environment of child processes, and their values are redacted
// from output.
func (c *Config) SecretEnvNames() []string {
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		key := name
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(name) // Windows environment names ignore case
		}
		if name != "" && !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	for _, name := range sortedKeys(c.Providers) {
		if p := c.Providers[name]; p != nil {
			add(p.APIKeyEnv)
		}
	}
	for _, name := range c.Shell.StripEnv {
		add(name)
	}
	for _, name := range sortedKeys(c.MCPServers) {
		if s := c.MCPServers[name]; s != nil {
			add(s.BearerTokenEnv)
		}
	}
	return names
}

// Secrets returns every secret value the program knows of: api_key values
// stored in the file and the current values of SecretEnvNames, read with
// getenv.
func (c *Config) Secrets(getenv func(string) string) []string {
	var secrets []string
	for _, name := range sortedKeys(c.Providers) {
		if p := c.Providers[name]; p != nil && p.APIKey != "" {
			secrets = append(secrets, p.APIKey)
		}
	}
	for _, name := range c.SecretEnvNames() {
		if v := getenv(name); v != "" {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
