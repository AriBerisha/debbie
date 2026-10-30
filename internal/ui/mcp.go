package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"debbie/internal/app"
	"debbie/internal/config"
	"debbie/internal/mcp"
	"debbie/internal/setup"
)

// mcpCommand runs /mcp. Adding, removing, enabling and disabling a server
// change config.json and the running servers at once; anything else is a
// report (mcpReport).
func (c *chat) mcpCommand(arg string) []span {
	args, err := setup.SplitCommandLine(arg)
	if err != nil {
		return []span{inked(err.Error(), inkYellow)}
	}
	if len(args) == 0 {
		return mcpReport(c.opts.MCP, arg, c.term)
	}
	switch args[0] {
	case "add", "add-json", "remove", "rm", "enable", "disable":
	default:
		return mcpReport(c.opts.MCP, arg, c.term)
	}
	cfg := c.opts.Providers
	if cfg == nil {
		return []span{inked("There is no config file to keep MCP servers in.", inkYellow)}
	}
	fail := func(err error) []span { return []span{inked(c.opts.Redactor.String(err.Error()), inkYellow)} }
	usage := func(problem string) []span {
		lines := strings.Split(setup.MCPUsage, "\n")
		for i, l := range lines {
			lines[i] = strings.Replace(l, "  ", "  /mcp ", 1)
		}
		return []span{inked(problem+"\n", inkYellow), grayed(strings.Join(lines, "\n"))}
	}
	start := func(name string, s *config.MCPServer) {
		if c.opts.MCP == nil {
			return
		}
		var env []string
		if c.opts.ChildEnv != nil {
			env = c.opts.ChildEnv()
		}
		c.opts.MCP.Add(mcp.FromConfig(name, s, env, c.opts.Workspace, os.Getenv))
	}
	switch args[0] {
	case "add", "add-json":
		var name string
		var s *config.MCPServer
		if args[0] == "add" {
			if name, s, err = setup.ParseMCPAdd(args[1:]); err != nil {
				return usage("/mcp add: " + err.Error())
			}
		} else {
			if len(args) != 3 {
				return usage("/mcp add-json takes a name and the JSON, in quotes")
			}
			if s, err = setup.ParseMCPJSON(args[2]); err != nil {
				return fail(err)
			}
			name = args[1]
		}
		if err := cfg.AddMCPServer(name, s); err != nil {
			return fail(err)
		}
		start(name, s)
		state := "starting it…"
		if s.Disabled {
			state = "switched off until /mcp enable " + name
		}
		return []span{plain("Added the MCP server "), bolded(name, ink{}), grayed(" · " + c.opts.Redactor.String(setup.DescribeMCP(s)) + "\nSaved to config.json; " + state)}
	case "remove", "rm":
		if len(args) != 2 {
			return usage("/mcp remove takes a server's name")
		}
		if err := cfg.RemoveMCPServer(args[1]); err != nil {
			return fail(err)
		}
		if c.opts.MCP != nil {
			c.opts.MCP.Remove(args[1])
		}
		return []span{plain("Removed the MCP server "), bolded(args[1], ink{}), grayed(" and its tools from config.json and this session.")}
	default: // enable, disable
		if len(args) != 2 {
			return usage("/mcp " + args[0] + " takes a server's name")
		}
		s, err := cfg.SetMCPServerDisabled(args[1], args[0] == "disable")
		if err != nil {
			return fail(err)
		}
		start(args[1], s)
		if s.Disabled {
			return []span{plain("Switched the MCP server "), bolded(args[1], ink{}), plain(" off"), grayed("; its tools are gone until /mcp enable " + args[1] + ".")}
		}
		return []span{plain("Switched the MCP server "), bolded(args[1], ink{}), plain(" on"), grayed("; starting it…")}
	}
}

// mcpReport is what /mcp shows: the servers and how they are, a server's
// tools with /mcp tools <name>, and /mcp reconnect <name> starts one again.
func mcpReport(m *mcp.Manager, arg string, t Terminal) []span {
	statuses := []mcp.Status(nil)
	if m != nil {
		statuses = m.Statuses()
	}
	if len(statuses) == 0 {
		return []span{plain("No MCP servers yet. Add one, and it starts at once:\n"),
			grayed("  /mcp add <name> -- <command> [args...]   a local server, such as\n"),
			plain("  /mcp add github -- npx -y @modelcontextprotocol/server-github\n"),
			grayed("  /mcp add <name> <url>                     a remote server, such as\n"),
			plain("  /mcp add docs https://mcp.example.com/mcp --bearer-token-env DOCS_TOKEN\n"),
			grayed("Options: -e KEY=VALUE, --cwd, --pin, --timeout. From a shell: " + app.Name + " mcp add, list, remove.\n" +
				"Only servers in your own config are used; a repository's are never read.")}
	}
	sub, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	for _, s := range statuses {
		if sub == s.Name { // /mcp <server> is /mcp tools <server>
			sub, name = "tools", s.Name
		}
	}
	switch sub {
	case "reconnect":
		if err := m.Reconnect(name); err != nil {
			return []span{inked(err.Error(), inkYellow)}
		}
		return []span{grayed("Starting the MCP server " + name + " again…")}
	case "tools":
		for _, s := range statuses {
			if s.Name != name {
				continue
			}
			if len(s.Tools)+s.Prompts+s.Resources == 0 {
				return []span{grayed(fmt.Sprintf("The MCP server %s offers nothing (%s).", name, s.State))}
			}
			out := []span{bolded(name, ink{}), grayed(fmt.Sprintf(" · %d tool%s, which the model calls as mcp__%s__<tool>\n", len(s.Tools), plural(len(s.Tools)), name))}
			for _, tool := range s.Tools {
				out = append(out, plain("  "+tool+"\n"))
			}
			if s.Prompts > 0 {
				out = append(out, grayed(fmt.Sprintf("%d prompt%s, which you run as commands:\n", s.Prompts, plural(s.Prompts))))
				for _, p := range m.Prompts() {
					if p.Server == name {
						var args []string
						for _, a := range p.Arguments {
							if a.Required {
								args = append(args, "<"+a.Name+">")
							} else {
								args = append(args, "["+a.Name+"]")
							}
						}
						out = append(out, plain("  "+strings.TrimSpace(p.Command+" "+strings.Join(args, " "))), grayed("  "+firstLine(p.Description, 60)+"\n"))
					}
				}
			}
			if s.Resources > 0 {
				out = append(out, grayed(fmt.Sprintf("%d resource%s, which you attach with @%s:<uri>:\n", s.Resources, plural(s.Resources), name)))
				shown := 0
				for _, r := range m.Resources() {
					if r.Server == name && shown < 20 {
						out = append(out, plain("  @"+name+":"+r.URI), grayed("  "+firstLine(r.Name, 40)+"\n"))
						shown++
					}
				}
				if s.Resources > shown {
					out = append(out, grayed(fmt.Sprintf("  … %d more; type @%s: to search them\n", s.Resources-shown, name)))
				}
			}
			for _, w := range s.Warnings {
				out = append(out, inked("  "+w+"\n", inkYellow))
			}
			return out
		}
		return []span{inked(fmt.Sprintf("There is no MCP server named %q.", name), inkYellow)}
	case "":
	default:
		return []span{inked("Use /mcp, /mcp <server>, or /mcp add, remove, enable, disable or reconnect <server>.", inkYellow)}
	}

	width := 0
	for _, s := range statuses {
		width = max(width, textWidth(s.Name))
	}
	out := []span{bolded("MCP servers", ink{}), grayed(" · from config.json\n")}
	for _, s := range statuses {
		bullet, state := inkGray, string(s.State)
		switch s.State {
		case mcp.Ready:
			bullet = inkGreen
			state = fmt.Sprintf("ready · %d tool%s", len(s.Tools), plural(len(s.Tools)))
			if s.Prompts > 0 {
				state += fmt.Sprintf(" · %d prompt%s", s.Prompts, plural(s.Prompts))
			}
			if s.Resources > 0 {
				state += fmt.Sprintf(" · %d resource%s", s.Resources, plural(s.Resources))
			}
			state += " · MCP " + s.Version
			if s.ServerInfo != "" {
				state += " · " + s.ServerInfo
			}
		case mcp.Starting:
			bullet, state = inkYellow, "starting…"
		case mcp.Failed:
			bullet, state = inkRed, "failed: "+s.Err
		}
		where := []rune(s.Where)
		if room := t.columns() - width - 8; room > 10 && len(where) > room {
			where = append([]rune("…"), where[len(where)-room+1:]...) // the end says most
		}
		out = append(out, styled(t.sym().bullet+" ", look{fg: bullet}), bolded(fmt.Sprintf("%-*s", width, s.Name), ink{}),
			plain("  "+state+"\n"), grayed(fmt.Sprintf("  %*s %s\n", width, "", string(where))))
	}
	return append(out, grayed("/mcp <server> lists what it offers · /mcp add, remove, enable, disable or reconnect <server> · each tool call asks first"))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// mcpNotes collects MCP news for line mode, which shows it before the next
// prompt rather than in the middle of a reply.
type mcpNotes struct {
	mu    sync.Mutex
	notes []mcp.Event
}

func (n *mcpNotes) add(e mcp.Event) {
	n.mu.Lock()
	n.notes = append(n.notes, e)
	n.mu.Unlock()
}

func (n *mcpNotes) take() []mcp.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.notes
	n.notes = nil
	return out
}
