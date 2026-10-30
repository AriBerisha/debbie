package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Prompts and resources (SPEC §22): a server's prompts become slash
// commands the user runs, and its resources can be attached to a message
// with @<server>:<uri>. Both happen only when the user asks; the model
// doesn't reach them by itself.

// maxResources bounds the resources kept per server, for completion.
const maxResources = 500

// capabilities are the features a server announced.
type capabilities struct {
	prompts, resources bool
}

func parseCapabilities(raw json.RawMessage) capabilities {
	var c map[string]json.RawMessage
	json.Unmarshal(raw, &c)
	_, prompts := c["prompts"]
	_, resources := c["resources"]
	return capabilities{prompts: prompts, resources: resources}
}

// Prompt is a prompt a server offers, run by the user as a command.
type Prompt struct {
	Server      string
	Name        string
	Title       string
	Description string
	Arguments   []PromptArgument
	Command     string // /mcp__<server>__<name>
}

// PromptArgument is one of a prompt's arguments.
type PromptArgument struct {
	Name, Description string
	Required          bool
}

// Resource is something a server offers to attach to a message.
type Resource struct {
	Server      string
	URI         string
	Name        string
	Description string
	MimeType    string
}

// list pages through a list method, calling each with each page's raw
// result.
func (c *Client) list(ctx context.Context, method string, each func(json.RawMessage) error) error {
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.send(ctx, &request{method: method, params: params})
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		if err := each(res); err != nil {
			return fmt.Errorf("%s: the answer isn't valid: %v", method, err)
		}
		var next struct {
			NextCursor string `json:"nextCursor"`
		}
		json.Unmarshal(res, &next)
		if next.NextCursor == "" || next.NextCursor == cursor {
			return nil
		}
		cursor = next.NextCursor
	}
	return nil
}

// ListPrompts lists the server's prompts.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	var out []Prompt
	err := c.list(ctx, "prompts/list", func(res json.RawMessage) error {
		var r struct {
			Prompts []struct {
				Name        string `json:"name"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Arguments   []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Required    bool   `json:"required"`
				} `json:"arguments"`
			} `json:"prompts"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return err
		}
		for _, p := range r.Prompts {
			if p.Name == "" {
				continue
			}
			prompt := Prompt{Name: p.Name, Title: printable(p.Title, 100), Description: printable(p.Description, 300)}
			for _, a := range p.Arguments {
				prompt.Arguments = append(prompt.Arguments, PromptArgument{Name: printable(a.Name, 64), Description: printable(a.Description, 200), Required: a.Required})
			}
			out = append(out, prompt)
		}
		return nil
	})
	return out, err
}

// GetPrompt gets a prompt with arguments, as the text of its messages.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	res, err := c.send(ctx, &request{method: "prompts/get", name: name, params: params})
	if err != nil {
		return "", err
	}
	var r struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("the prompt isn't valid: %v", err)
	}
	var parts []string
	for _, m := range r.Messages {
		res, err := resultText(json.RawMessage(`{"content":[` + string(m.Content) + `]}`))
		if err != nil || strings.TrimSpace(res.Text) == "" {
			continue
		}
		parts = append(parts, res.Text)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("the prompt %s has no text", name)
	}
	return strings.Join(parts, "\n\n"), nil
}

// ListResources lists the server's resources, at most maxResources.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	err := c.list(ctx, "resources/list", func(res json.RawMessage) error {
		var r struct {
			Resources []struct {
				URI         string `json:"uri"`
				Name        string `json:"name"`
				Title       string `json:"title"`
				Description string `json:"description"`
				MimeType    string `json:"mimeType"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return err
		}
		for _, x := range r.Resources {
			if x.URI == "" || len(out) >= maxResources || strings.ContainsAny(x.URI, " \t\n") {
				continue // a URI with spaces can't be written in a mention
			}
			name := x.Title
			if name == "" {
				name = x.Name
			}
			out = append(out, Resource{URI: x.URI, Name: printable(name, 100), Description: printable(x.Description, 200), MimeType: printable(x.MimeType, 60)})
		}
		return nil
	})
	return out, err
}

// ReadResource reads a resource as text: text contents as they are,
// binary ones described.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	res, err := c.send(ctx, &request{method: "resources/read", name: uri, params: map[string]any{"uri": uri}})
	if err != nil {
		return "", err
	}
	var r struct {
		Contents []struct {
			URI      string  `json:"uri"`
			MimeType string  `json:"mimeType"`
			Text     *string `json:"text"`
			Blob     string  `json:"blob"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fmt.Errorf("the resource isn't valid: %v", err)
	}
	var parts []string
	for _, x := range r.Contents {
		switch {
		case x.Text != nil && len(r.Contents) == 1:
			parts = append(parts, *x.Text)
		case x.Text != nil:
			parts = append(parts, fmt.Sprintf("[%s]\n%s", printable(x.URI, 300), *x.Text))
		default:
			parts = append(parts, fmt.Sprintf("[%s, %s, %s: binary, not shown]", printable(x.URI, 300), printable(x.MimeType, 60), size(len(x.Blob)*3/4)))
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// The manager's side.

// Prompts lists the prompts of the servers that are ready.
func (m *Manager) Prompts() []Prompt {
	var out []Prompt
	for _, c := range m.list() {
		c.mu.Lock()
		out = append(out, c.prompts...)
		c.mu.Unlock()
	}
	return out
}

// Resources lists the resources of the servers that are ready.
func (m *Manager) Resources() []Resource {
	var out []Resource
	for _, c := range m.list() {
		c.mu.Lock()
		out = append(out, c.resources...)
		c.mu.Unlock()
	}
	return out
}

// Servers names the configured servers.
func (m *Manager) Servers() map[string]bool {
	out := map[string]bool{}
	for _, c := range m.list() {
		out[c.cfg.Name] = true
	}
	return out
}

func (m *Manager) conn(name string) (*conn, error) {
	for _, c := range m.list() {
		if c.cfg.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("there is no MCP server named %q", name)
}

// RunPrompt gets the prompt a command names with the arguments the user
// typed: name=value pairs, or values in the order the prompt lists its
// arguments, the last taking the rest of the line.
func (m *Manager) RunPrompt(ctx context.Context, command, argText string) (string, error) {
	var p *Prompt
	for _, x := range m.Prompts() {
		if x.Command == command {
			p = &x
			break
		}
	}
	if p == nil {
		return "", fmt.Errorf("there is no MCP prompt %s", command)
	}
	args, err := promptArgs(*p, argText)
	if err != nil {
		return "", err
	}
	c, err := m.conn(p.Server)
	if err != nil {
		return "", err
	}
	client, err := c.current()
	if err != nil {
		return "", err
	}
	text, err := client.GetPrompt(ctx, p.Name, args)
	if err != nil {
		return "", fmt.Errorf("the MCP server %s: %s", p.Server, m.redact(err.Error()))
	}
	return m.redact(text), nil
}

// namedArg is a name=value pair in a prompt command.
var namedArg = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=`)

// promptArgs reads a prompt's arguments from what the user typed.
func promptArgs(p Prompt, text string) (map[string]string, error) {
	args := map[string]string{}
	text = strings.TrimSpace(text)
	// name=value pairs when the first word is one; values in order else.
	fields := strings.Fields(text)
	named := len(fields) > 0 && namedArg.MatchString(fields[0])
	if named {
		for _, field := range strings.Fields(text) {
			k, v, ok := strings.Cut(field, "=")
			if !ok || !slices.ContainsFunc(p.Arguments, func(a PromptArgument) bool { return a.Name == k }) {
				return nil, fmt.Errorf("%q isn't one of the prompt's arguments (%s)", field, argNames(p))
			}
			args[k] = v
		}
	} else if text != "" {
		for i, a := range p.Arguments {
			if i >= len(fields) {
				break
			}
			if i == len(p.Arguments)-1 {
				args[a.Name] = strings.Join(fields[i:], " ")
				break
			}
			args[a.Name] = fields[i]
		}
		if len(p.Arguments) == 0 {
			return nil, fmt.Errorf("the prompt %s takes no arguments", p.Command)
		}
	}
	var missing []string
	for _, a := range p.Arguments {
		if _, ok := args[a.Name]; a.Required && !ok {
			missing = append(missing, a.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s needs %s (its arguments: %s)", p.Command, strings.Join(missing, " and "), argNames(p))
	}
	return args, nil
}

func argNames(p Prompt) string {
	var names []string
	for _, a := range p.Arguments {
		n := a.Name
		if !a.Required {
			n = "[" + n + "]"
		}
		names = append(names, n)
	}
	return strings.Join(names, " ")
}

// ReadResource reads a resource for an @<server>:<uri> mention, wrapped as
// untrusted content.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (string, error) {
	c, err := m.conn(server)
	if err != nil {
		return "", err
	}
	client, err := c.current()
	if err != nil {
		return "", err
	}
	text, err := client.ReadResource(ctx, uri)
	if err != nil {
		return "", fmt.Errorf("the MCP server %s: %s", server, m.redact(err.Error()))
	}
	return wrapResource(server, uri, m.redact(text)), nil
}

const untrustedResourceTag = "untrusted_mcp_resource"

func wrapResource(server, uri, text string) string {
	text = strings.ReplaceAll(text, "<"+untrustedResourceTag, "&lt;"+untrustedResourceTag)
	text = strings.ReplaceAll(text, "</"+untrustedResourceTag, "&lt;/"+untrustedResourceTag)
	return fmt.Sprintf("<%s server=%q uri=%q>\n%s\n</%s>", untrustedResourceTag, server, printable(uri, 300), text, untrustedResourceTag)
}
