package mcp

import (
	"maps"
	"slices"
	"time"

	"debbie/internal/config"
)

// FromConfig turns a configured server into one the manager runs. A local
// server gets env, the environment commands get (without API keys), plus
// its own variables, and runs in workspace unless it names a directory; a
// remote one gets its bearer token through getenv.
func FromConfig(name string, s *config.MCPServer, env []string, workspace string, getenv func(string) string) Server {
	timeout := s.TimeoutSeconds
	if timeout == 0 {
		timeout = config.DefaultMCPTimeoutSeconds
	}
	srv := Server{Name: name, Command: s.Command, Args: s.Args, URL: s.URL, Pin: s.TLSPinSHA256,
		Timeout: time.Duration(timeout) * time.Second, Disabled: s.Disabled, Dir: s.Cwd}
	if s.Command != "" {
		srv.Env = slices.Clone(env)
		for _, key := range slices.Sorted(maps.Keys(s.Env)) {
			srv.Env = append(srv.Env, key+"="+s.Env[key])
		}
		if srv.Dir == "" {
			srv.Dir = workspace
		}
	}
	if s.BearerTokenEnv != "" {
		srv.Token = getenv(s.BearerTokenEnv)
	}
	return srv
}

// ServersFromConfig returns the config's servers by name.
func ServersFromConfig(cfg *config.Config, env []string, workspace string, getenv func(string) string) []Server {
	var out []Server
	for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
		if s := cfg.MCPServers[name]; s != nil {
			out = append(out, FromConfig(name, s, env, workspace, getenv))
		}
	}
	return out
}
