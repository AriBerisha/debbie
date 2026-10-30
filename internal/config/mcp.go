package config

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"debbie/internal/netguard"
)

// MCPServer is an MCP server whose tools the model can use (SPEC §22):
// a program the agent starts and talks to over its standard streams, or a
// server at a URL. Only servers in the user's config.json are used; a
// repository's own server lists are never read.
type MCPServer struct {
	// Command and Args start a local server; Env adds to the environment
	// it gets, which never holds API keys (SPEC §3.5); Cwd is where it
	// runs, the workspace when empty.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	// URL reaches a remote server (Streamable HTTP). TLSPinSHA256 pins its
	// certificate as for providers; BearerTokenEnv names the environment
	// variable holding a token to send as "Authorization: Bearer".
	URL            string `json:"url,omitempty"`
	TLSPinSHA256   string `json:"tls_pin_sha256,omitempty"`
	BearerTokenEnv string `json:"bearer_token_env,omitempty"`
	// TimeoutSeconds bounds each tool call; 0 means 120.
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
	Disabled       bool `json:"disabled,omitempty"`
}

// MCP defaults and limits.
const (
	DefaultMCPTimeoutSeconds = 120
	MaxMCPTimeoutSeconds     = 3600
)

// mcpNameRE limits server names to what can go into tool names, which
// become mcp__<server>__<tool>.
var mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

func (c *Config) validateMCP(v *validator) {
	secretNames := map[string]bool{}
	for _, name := range c.SecretEnvNames() {
		secretNames[envFold(name)] = true
	}
	byLower := map[string]string{}
	for _, name := range sortedKeys(c.MCPServers) {
		path := "mcp_servers." + name
		if !mcpNameRE.MatchString(name) || strings.Contains(name, "__") {
			v.errorf(path, "invalid name: use up to 32 letters, digits, '_' and '-', without \"__\"")
		}
		if other, dup := byLower[strings.ToLower(name)]; dup {
			v.errorf(path, "same name as %q apart from case", other)
		}
		byLower[strings.ToLower(name)] = name
		s := c.MCPServers[name]
		if s == nil {
			v.errorf(path, "must be an object")
			continue
		}
		switch {
		case s.Command != "" && s.URL != "":
			v.errorf(path, "set command or url, not both")
		case s.Command == "" && s.URL == "":
			v.errorf(path, "set command, for a local server, or url, for a remote one")
		case s.Command != "":
			if s.TLSPinSHA256 != "" || s.BearerTokenEnv != "" {
				v.errorf(path, "tls_pin_sha256 and bearer_token_env are for servers with a url")
			}
			for _, key := range sortedKeys(s.Env) {
				switch {
				case !envNameRE.MatchString(key):
					v.errorf(joinPath(path+".env", key), "%q is not an environment variable name", key)
				case secretNames[envFold(key)]:
					v.errorf(joinPath(path+".env", key), "%s holds an API key, which programs the agent starts never get", key)
				}
			}
			if s.Cwd != "" && !filepath.IsAbs(s.Cwd) {
				v.errorf(path+".cwd", "the path must be absolute")
			}
		default:
			if len(s.Args) > 0 || len(s.Env) > 0 || s.Cwd != "" {
				v.errorf(path, "args, env and cwd are for servers with a command")
			}
			u, err := checkBaseURL(s.URL)
			if err != nil {
				v.errorf(path+".url", "%v", err)
			}
			if s.TLSPinSHA256 != "" {
				if _, err := netguard.ParsePin(s.TLSPinSHA256); err != nil {
					v.errorf(path+".tls_pin_sha256", "%v", err)
				} else if u != nil && u.Scheme != "https" {
					v.errorf(path+".tls_pin_sha256", "a certificate pin needs an https url")
				}
			}
			if s.BearerTokenEnv != "" && !envNameRE.MatchString(s.BearerTokenEnv) {
				v.errorf(path+".bearer_token_env", "%q is not an environment variable name", s.BearerTokenEnv)
			}
			if u != nil && u.Scheme == "http" && s.BearerTokenEnv != "" {
				v.warnf("MCP server %q uses plain http, so its token travels unencrypted", name)
			}
		}
		if s.TimeoutSeconds < 0 || s.TimeoutSeconds > MaxMCPTimeoutSeconds {
			v.errorf(path+".timeout_seconds", "must be between 1 and %d, or 0 for %d", MaxMCPTimeoutSeconds, DefaultMCPTimeoutSeconds)
		}
	}
}

// envFold is an environment variable's name as the OS compares it.
func envFold(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
