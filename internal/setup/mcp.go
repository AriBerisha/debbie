package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"debbie/internal/config"
)

// MCP servers are managed with commands, "debbie mcp ..." and /mcp in
// the chat, as with Claude Code's "claude mcp" (SPEC §22); config.json can
// still be edited by hand.

// MCPUsage lists the mcp commands.
const MCPUsage = `  add [options] <name> -- <command> [args...]   add a local server, which the agent starts
  add [options] <name> <url>                     add a remote server
  add-json <name> '<json>'                       add a server described in JSON
  list                                           list the servers, checking each
  get <name>                                     show a server and check it
  remove <name>                                  remove a server
  enable <name>, disable <name>                  switch a server on or off
options for add:
  -e, --env KEY=VALUE      a variable for a local server (repeat for more)
  --cwd DIR                where a local server runs (default: the workspace)
  --bearer-token-env NAME  the variable holding a remote server's bearer token
  --pin SHA256             a remote server's certificate fingerprint
  --timeout SECONDS        how long each tool call may take (default 120)
  --disabled               add it switched off`

// ParseMCPAdd reads the arguments of "mcp add": options, the server's
// name, then its URL, or its command and arguments, taken as they are,
// after an optional "--". Options may also come after the name, or after
// a URL.
func ParseMCPAdd(args []string) (name string, s *config.MCPServer, err error) {
	s = &config.MCPServer{}
	transport := ""
	isOption := func(a string) bool { return strings.HasPrefix(a, "-") && a != "-" && a != "--" }
	// options reads the options from args[i] on, returning where they end.
	options := func(i int) (int, error) {
		for i < len(args) && isOption(args[i]) {
			arg := args[i]
			key, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
			if key == "disabled" {
				s.Disabled = true
				i++
				continue
			}
			if !inline {
				if i+1 >= len(args) {
					return 0, fmt.Errorf("%s needs a value", arg)
				}
				value = args[i+1]
				i++
			}
			i++
			switch key {
			case "e", "env":
				k, v, ok := strings.Cut(value, "=")
				if !ok || k == "" {
					return 0, fmt.Errorf("%s %q: give KEY=VALUE", arg, value)
				}
				if s.Env == nil {
					s.Env = map[string]string{}
				}
				s.Env[k] = v
			case "cwd":
				s.Cwd = value
			case "bearer-token-env":
				s.BearerTokenEnv = value
			case "pin", "tls-pin":
				s.TLSPinSHA256 = value
			case "timeout":
				n, err := strconv.Atoi(value)
				if err != nil {
					return 0, fmt.Errorf("%s %q: give a number of seconds", arg, value)
				}
				s.TimeoutSeconds = n
			case "t", "transport":
				transport = value
			default:
				return 0, fmt.Errorf("unknown option %s", arg)
			}
		}
		return i, nil
	}
	i, err := options(0)
	if err != nil {
		return "", nil, err
	}
	if i >= len(args) {
		return "", nil, errors.New("name the server")
	}
	name = args[i]
	if i, err = options(i + 1); err != nil {
		return "", nil, err
	}
	rest := args[i:]
	switch {
	case len(rest) > 0 && rest[0] == "--":
		rest = rest[1:]
	case len(rest) > 0 && (strings.HasPrefix(rest[0], "http://") || strings.HasPrefix(rest[0], "https://")):
		s.URL = rest[0]
		end, err := options(i + 1)
		if err != nil {
			return "", nil, err
		}
		if end < len(args) {
			return "", nil, fmt.Errorf("unexpected %q after the URL", args[end])
		}
		rest = nil
	}
	if s.URL == "" {
		if len(rest) == 0 {
			return "", nil, errors.New("give the server's command after --, or its URL")
		}
		s.Command, s.Args = rest[0], slices.Clone(rest[1:])
	}
	switch transport {
	case "", "stdio", "http", "sse", "streamable-http":
	default:
		return "", nil, fmt.Errorf("unknown transport %q: use stdio for a command, http for a URL", transport)
	}
	if transport == "stdio" && s.URL != "" || (transport == "http" || transport == "sse" || transport == "streamable-http") && s.Command != "" {
		return "", nil, fmt.Errorf("the %s transport doesn't fit %q", transport, describeMCP(s))
	}
	return name, s, nil
}

// ParseMCPJSON reads a server described in JSON as config.json holds it.
// The "type" key of Claude Code's form is accepted; its "headers" are not,
// since tokens belong in environment variables.
func ParseMCPJSON(text string) (*config.MCPServer, error) {
	var raw struct {
		config.MCPServer
		Type    string            `json:"type"`
		Headers map[string]string `json:"headers"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("the JSON: %v", err)
	}
	if len(raw.Headers) > 0 {
		return nil, errors.New(`"headers" aren't supported: put the token in an environment variable and name it in "bearer_token_env"`)
	}
	switch raw.Type {
	case "", "stdio", "http", "sse", "streamable-http":
	default:
		return nil, fmt.Errorf("unknown type %q", raw.Type)
	}
	s := raw.MCPServer
	return &s, nil
}

// DescribeMCP says briefly what a server is: its command line or URL, and
// the names of the variables it gets, never their values.
func DescribeMCP(s *config.MCPServer) string {
	text := describeMCP(s)
	if len(s.Env) > 0 {
		var keys []string
		for k := range s.Env {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		text += " · env " + strings.Join(keys, ", ")
	}
	if s.BearerTokenEnv != "" {
		text += " · token from $" + s.BearerTokenEnv
	}
	if s.Disabled {
		text += " · disabled"
	}
	return text
}

func describeMCP(s *config.MCPServer) string {
	if s.URL != "" {
		return s.URL
	}
	parts := []string{s.Command}
	for _, a := range s.Args {
		if a == "" || strings.ContainsAny(a, " \t\"") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// SplitCommandLine splits a line typed in the chat into arguments, as a
// shell would for words, "double" and 'single' quotes.
func SplitCommandLine(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("a %c quote isn't closed", quote)
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// MCPServer returns a configured server, or nil.
func (m *Manager) MCPServer(name string) *config.MCPServer { return m.Config.MCPServers[name] }

// AddMCPServer saves a new server; a name already taken is refused.
func (m *Manager) AddMCPServer(name string, s *config.MCPServer) error {
	for existing := range m.Config.MCPServers {
		if strings.EqualFold(existing, name) {
			return fmt.Errorf("there is already an MCP server named %q; remove it first", existing)
		}
	}
	return m.change(func(cfg *config.Config) {
		if cfg.MCPServers == nil {
			cfg.MCPServers = map[string]*config.MCPServer{}
		}
		cfg.MCPServers[name] = s
	})
}

// RemoveMCPServer takes a server out of the config.
func (m *Manager) RemoveMCPServer(name string) error {
	if m.Config.MCPServers[name] == nil {
		return fmt.Errorf("there is no MCP server named %q", name)
	}
	return m.change(func(cfg *config.Config) { delete(cfg.MCPServers, name) })
}

// SetMCPServerDisabled switches a server off or on in the config and
// returns it as saved.
func (m *Manager) SetMCPServerDisabled(name string, disabled bool) (*config.MCPServer, error) {
	s := m.Config.MCPServers[name]
	if s == nil {
		return nil, fmt.Errorf("there is no MCP server named %q", name)
	}
	next := *s
	next.Disabled = disabled
	if err := m.change(func(cfg *config.Config) { cfg.MCPServers[name] = &next }); err != nil {
		return nil, err
	}
	return &next, nil
}
