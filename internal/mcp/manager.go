package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"debbie/internal/tools"
)

// Server is how to reach one MCP server.
type Server struct {
	Name string
	// A local server: the program, its arguments, its whole environment
	// and the directory it runs in.
	Command string
	Args    []string
	Env     []string
	Dir     string
	// A remote server: its URL, a certificate pin and a bearer token, both
	// optional.
	URL   string
	Pin   string
	Token string
	// Timeout bounds each tool call.
	Timeout  time.Duration
	Disabled bool
}

// Where describes the server for /mcp: its command line or its URL.
func (s Server) Where() string {
	if s.URL != "" {
		return s.URL
	}
	return strings.Join(append([]string{s.Command}, s.Args...), " ")
}

// Connect starts or reaches the server and agrees on a protocol version.
func Connect(ctx context.Context, s Server) (*Client, error) {
	if s.URL != "" {
		t, err := newHTTP(s)
		if err != nil {
			return nil, err
		}
		c, err := connect(ctx, t, true)
		var se *statusError
		if err == nil || !errors.As(err, &se) || se.code != 400 && se.code != 404 && se.code != 405 || se.rpc != nil && se.rpc.modern() {
			return c, err
		}
		// Refused both ways: maybe a server of the older HTTP+SSE
		// transport, as the specification says to try next.
		st, serr := openSSE(ctx, s)
		if serr != nil {
			return nil, fmt.Errorf("%v (and as an HTTP+SSE server: %v)", err, serr)
		}
		c = &Client{t: st}
		if err := c.initialize(ctx); err != nil {
			st.close()
			return nil, err
		}
		return c, nil
	}
	t, err := startStdio(s)
	if err != nil {
		return nil, err
	}
	return connect(ctx, t, false)
}

const (
	// startTimeout bounds starting a server and listing its tools; npx
	// may download a package first.
	startTimeout = 90 * time.Second
	// maxRestarts is how many times a local server that exited is started
	// again in a session.
	maxRestarts = 3
	// maxDescription is the most of a tool's description the model sees.
	maxDescription = 2000
)

// State is where a server is at.
type State string

const (
	Starting State = "starting"
	Ready    State = "ready"
	Failed   State = "failed"
	Disabled State = "disabled"
)

// Status describes a server for /mcp.
type Status struct {
	Name, Where string
	State       State
	Err         string
	Tools       []string // the tools' names on the server
	Prompts     int
	Resources   int
	Warnings    []string
	Version     string // the protocol version agreed on
	ServerInfo  string // what the server says it is, unverified
}

// Event is news about a server, for the UI.
type Event struct {
	Server string
	Failed bool
	Text   string
}

// Manager runs the configured servers and keeps their tools in a registry,
// named mcp__<server>__<tool>, while they are ready.
type Manager struct {
	reg    *tools.Registry
	redact func(string) string

	mu     sync.Mutex
	conns  []*conn
	events []Event
	notify func(Event)
}

// NewManager returns a manager of servers whose tools go into reg. redact
// removes secrets from what servers say in errors.
func NewManager(servers []Server, reg *tools.Registry, redact func(string) string) *Manager {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	m := &Manager{reg: reg, redact: redact}
	for _, s := range servers {
		m.conns = append(m.conns, &conn{m: m, cfg: s, state: Starting})
	}
	return m
}

// errStopped ends a start whose server was removed or switched off
// meanwhile.
var errStopped = errors.New("the MCP server was stopped")

// list returns the servers, a copy safe to use while servers are added or
// removed.
func (m *Manager) list() []*conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.conns)
}

// Add starts a server added during the session, after the one of that
// name, if any, is stopped and taken out: so it also applies new settings.
func (m *Manager) Add(s Server) {
	m.Remove(s.Name)
	c := &conn{m: m, cfg: s, state: Starting}
	m.mu.Lock()
	m.conns = append(m.conns, c)
	m.mu.Unlock()
	if s.Disabled {
		c.setState(Disabled, "")
		return
	}
	go c.start(false)
}

// Remove stops a server and takes its tools, prompts and resources away,
// reporting whether there was one by that name.
func (m *Manager) Remove(name string) bool {
	m.mu.Lock()
	i := slices.IndexFunc(m.conns, func(c *conn) bool { return c.cfg.Name == name })
	if i < 0 {
		m.mu.Unlock()
		return false
	}
	c := m.conns[i]
	m.conns = slices.Delete(m.conns, i, i+1)
	m.mu.Unlock()
	c.stop()
	return true
}

// Start connects to the servers in the background.
func (m *Manager) Start() {
	for _, c := range m.list() {
		if c.cfg.Disabled {
			c.setState(Disabled, "")
			continue
		}
		go c.start(false)
	}
}

// Subscribe calls fn with each event so far, then with each new one, on
// whatever goroutine it happens.
func (m *Manager) Subscribe(fn func(Event)) {
	m.mu.Lock()
	past := append([]Event(nil), m.events...)
	m.notify = fn
	m.mu.Unlock()
	for _, e := range past {
		fn(e)
	}
}

func (m *Manager) event(e Event) {
	e.Text = m.redact(e.Text)
	m.mu.Lock()
	m.events = append(m.events, e)
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(e)
	}
}

// Statuses describes the servers, in the config's order.
func (m *Manager) Statuses() []Status {
	var out []Status
	for _, c := range m.list() {
		c.mu.Lock()
		s := Status{Name: c.cfg.Name, Where: c.cfg.Where(), State: c.state, Err: c.err, Warnings: c.warnings}
		for _, t := range c.tools {
			s.Tools = append(s.Tools, t.Name)
		}
		s.Prompts, s.Resources = len(c.prompts), len(c.resources)
		if c.client != nil {
			s.Version, s.ServerInfo = c.client.Version(), c.client.ServerInfo
		}
		c.mu.Unlock()
		out = append(out, s)
	}
	return out
}

// Reconnect starts a server again, in the background.
func (m *Manager) Reconnect(name string) error {
	for _, c := range m.list() {
		if c.cfg.Name == name {
			if c.cfg.Disabled {
				return fmt.Errorf("the MCP server %s is disabled in the config", name)
			}
			c.setState(Starting, "")
			go c.start(true)
			return nil
		}
	}
	return fmt.Errorf("there is no MCP server named %q", name)
}

// Close stops every server.
func (m *Manager) Close() {
	var wg sync.WaitGroup
	for _, c := range m.list() {
		c.mu.Lock()
		client := c.client
		c.client = nil
		c.mu.Unlock()
		if client != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				client.Close()
			}()
		}
	}
	wg.Wait()
}

// conn is one server and its state.
type conn struct {
	m   *Manager
	cfg Server

	start1    sync.Mutex // one start at a time
	mu        sync.Mutex // guards the rest
	client    *Client
	state     State
	err       string
	tools     []Tool
	prompts   []Prompt
	resources []Resource
	warnings  []string
	restarts  int
	stopped   bool // removed or switched off: a start under way gives up
}

// stop ends the server for good. A start under way finds it stopped and
// closes what it opened instead of offering its tools.
func (c *conn) stop() {
	c.mu.Lock()
	c.stopped = true
	client := c.client
	c.client, c.tools, c.prompts, c.resources = nil, nil, nil, nil
	c.state, c.err = Disabled, ""
	c.m.reg.Remove(c.prefix())
	c.mu.Unlock()
	if client != nil {
		client.Close()
	}
}

func (c *conn) setState(s State, err string) {
	c.mu.Lock()
	c.state, c.err = s, err
	c.mu.Unlock()
}

// start connects and registers the server's tools, reporting how it went.
func (c *conn) start(again bool) {
	err := c.connect()
	if errors.Is(err, errStopped) {
		return
	}
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	n, prompts, resources, warnings := len(c.tools), len(c.prompts), len(c.resources), c.warnings
	c.mu.Unlock()
	offers := fmt.Sprintf("%d tool%s", n, plural(n))
	if prompts > 0 {
		offers += fmt.Sprintf(" · %d prompt%s", prompts, plural(prompts))
	}
	if resources > 0 {
		offers += fmt.Sprintf(" · %d resource%s", resources, plural(resources))
	}
	text := fmt.Sprintf("MCP server %s is ready · %s", c.cfg.Name, offers)
	if again {
		text = fmt.Sprintf("MCP server %s reconnected · %s", c.cfg.Name, offers)
	}
	c.m.event(Event{Server: c.cfg.Name, Text: text})
	for _, w := range warnings {
		c.m.event(Event{Server: c.cfg.Name, Failed: true, Text: "MCP server " + c.cfg.Name + ": " + w})
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fail records why the server isn't usable and takes its tools away.
func (c *conn) fail(err error) {
	msg := c.m.redact(err.Error())
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.state, c.err = Failed, msg
	c.prompts, c.resources = nil, nil
	c.mu.Unlock()
	c.m.reg.Remove(c.prefix())
	c.m.event(Event{Server: c.cfg.Name, Failed: true, Text: fmt.Sprintf("MCP server %s failed: %s · /mcp", c.cfg.Name, msg)})
}

func (c *conn) prefix() string { return "mcp__" + c.cfg.Name + "__" }

// connect (re)starts the server, lists its tools and registers them.
func (c *conn) connect() error {
	c.start1.Lock()
	defer c.start1.Unlock()
	c.mu.Lock()
	old := c.client
	c.client = nil
	c.mu.Unlock()
	if old != nil {
		old.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	client, err := Connect(ctx, c.cfg)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("it didn't answer within %s", startTimeout)
		}
		return err
	}
	list, warnings, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return err
	}
	// Prompts and resources are extras: a server that fails to list them
	// still offers its tools.
	var prompts []Prompt
	var resources []Resource
	if client.caps.prompts {
		got, err := client.ListPrompts(ctx)
		if err != nil {
			warnings = append(warnings, "its prompts: "+c.m.redact(err.Error()))
		}
		taken := map[string]bool{}
		for _, p := range got {
			p.Server = c.cfg.Name
			p.Command = "/" + exposedName(c.cfg.Name, p.Name, taken)
			taken[p.Command[1:]] = true
			prompts = append(prompts, p)
		}
	}
	if client.caps.resources {
		got, err := client.ListResources(ctx)
		if err != nil {
			warnings = append(warnings, "its resources: "+c.m.redact(err.Error()))
		}
		for _, r := range got {
			r.Server = c.cfg.Name
			resources = append(resources, r)
		}
	}
	// The tools change under c.mu, so a stop can't come between.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		client.Close()
		return errStopped
	}
	c.client, c.tools, c.prompts, c.resources, c.warnings, c.state, c.err = client, list, prompts, resources, warnings, Ready, ""
	c.m.reg.Remove(c.prefix())
	taken := map[string]bool{}
	var adapters []tools.Tool
	for _, t := range list {
		name := exposedName(c.cfg.Name, t.Name, taken)
		taken[name] = true
		adapters = append(adapters, &serverTool{name: name, conn: c, tool: t, schema: modelSchema(t.InputSchema)})
	}
	c.m.reg.Add(adapters...)
	return nil
}

// current returns the client for a call. A local server that exited is
// started again, a few times a session.
func (c *conn) current() (*Client, error) {
	for waited := false; ; waited = true {
		c.mu.Lock()
		client, state, errText := c.client, c.state, c.err
		c.mu.Unlock()
		if client != nil {
			if s, ok := client.t.(*stdio); !ok || s.alive() {
				return client, nil
			}
		}
		switch {
		case state == Starting && !waited:
			c.start1.Lock() // wait for the start under way
			c.start1.Unlock()
			continue
		case state == Failed || state == Disabled:
			return nil, fmt.Errorf("the MCP server %s isn't available: %s", c.cfg.Name, errText)
		}
		break
	}
	c.mu.Lock()
	c.restarts++
	restarts := c.restarts
	c.mu.Unlock()
	if restarts > maxRestarts {
		err := fmt.Errorf("the MCP server %s exited %d times; /mcp reconnect %s starts it again", c.cfg.Name, restarts, c.cfg.Name)
		c.fail(err)
		return nil, err
	}
	c.m.event(Event{Server: c.cfg.Name, Text: "MCP server " + c.cfg.Name + " had exited; starting it again"})
	if err := c.connect(); errors.Is(err, errStopped) {
		return nil, fmt.Errorf("the MCP server %s was stopped", c.cfg.Name)
	} else if err != nil {
		c.fail(err)
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client, nil
}

// call runs a tool call on the server, within the server's timeout.
func (c *conn) call(ctx context.Context, t Tool, args json.RawMessage) (tools.Result, error) {
	client, err := c.current()
	if err != nil {
		return tools.Result{}, err
	}
	if c.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	}
	res, err := client.CallTool(ctx, t, args)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return tools.Result{}, fmt.Errorf("the MCP server %s didn't answer within %s", c.cfg.Name, c.cfg.Timeout)
	case ctx.Err() != nil:
		return tools.Result{}, ctx.Err()
	case err != nil:
		return tools.Result{}, fmt.Errorf("the MCP server %s: %s", c.cfg.Name, c.m.redact(err.Error()))
	}
	display := tools.Lines(strings.Count(res.Text, "\n") + 1)
	if res.Text == "" {
		res.Text, display = "(no output)", "no output"
	}
	if res.IsError {
		display = "the tool reported an error"
	}
	return tools.Result{Output: wrapUntrusted(c.cfg.Name, t.Name, res.Text), Display: display, Warn: res.IsError}, nil
}

// untrustedTag wraps tool results: what MCP servers return is data, never
// instructions (SPEC §19.5).
const untrustedTag = "untrusted_mcp_result"

func wrapUntrusted(server, tool, text string) string {
	text = strings.ReplaceAll(text, "<"+untrustedTag, "&lt;"+untrustedTag)
	text = strings.ReplaceAll(text, "</"+untrustedTag, "&lt;/"+untrustedTag)
	return fmt.Sprintf("<%s server=%q tool=%q>\n%s\n</%s>", untrustedTag, server, printable(tool, 128), text, untrustedTag)
}

// exposedName is the name the model calls a server's tool by:
// mcp__<server>__<tool>, with characters model APIs refuse replaced, at
// most 64 characters, and a short hash added when it had to be cut or is
// taken.
func exposedName(server, tool string, taken map[string]bool) string {
	var b strings.Builder
	b.WriteString("mcp__" + server + "__")
	for _, r := range tool {
		if r == '_' || r == '-' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) <= 64 && !taken[name] {
		return name
	}
	sum := sha256.Sum256([]byte(tool))
	return name[:min(len(name), 64-7)] + "_" + hex.EncodeToString(sum[:3])
}

// modelSchema is a tool's input schema as the model gets it: an object
// schema, without the root's $schema and $id, which some model servers
// refuse. Everything else stays as the server wrote it.
func modelSchema(raw json.RawMessage) json.RawMessage {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	delete(root, "$schema")
	delete(root, "$id")
	if _, ok := root["type"]; !ok {
		root["type"] = json.RawMessage(`"object"`)
	}
	b, err := json.Marshal(root)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

// serverTool offers one of a server's tools to the model. Calls run only
// after the user allows them, unless the approval mode is yolo, since a
// server's own claims about what a tool does can't be trusted.
type serverTool struct {
	name   string
	conn   *conn
	tool   Tool
	schema json.RawMessage
}

func (t *serverTool) Name() string            { return t.name }
func (t *serverTool) Kind() tools.Kind        { return tools.Execute }
func (t *serverTool) Params() []tools.Param   { return nil }
func (t *serverTool) Schema() json.RawMessage { return t.schema }

func (t *serverTool) Description() string {
	d := strings.TrimSpace(t.tool.Description)
	if d == "" {
		d = t.tool.Title
	}
	if len([]rune(d)) > maxDescription {
		d = string([]rune(d)[:maxDescription]) + "…"
	}
	return fmt.Sprintf("(MCP server %s) %s", t.conn.cfg.Name, strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, d))
}

func (t *serverTool) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	raw, err := json.Marshal(map[string]any(args))
	if err != nil {
		return nil, err
	}
	// Short arguments fit on the summary line; longer ones are shown in
	// full, indented, when the call is asked about.
	summary := fmt.Sprintf("%s (%s MCP)", printable(t.tool.Name, 64), t.conn.cfg.Name)
	preview := ""
	if compact := printable(string(raw), 80); compact != "{}" && len(compact) == len(raw) {
		summary += " " + compact
		preview = compact
	} else if compact != "{}" {
		var pretty bytes.Buffer
		json.Indent(&pretty, raw, "", "  ")
		preview = pretty.String()
		if lines := strings.Split(preview, "\n"); len(lines) > 30 {
			preview = strings.Join(lines[:30], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-30)
		}
	}
	return &tools.Call{
		Kind:       tools.Execute,
		Summary:    summary,
		Preview:    preview,
		SessionKey: t.name,
		Run: func(ctx context.Context) (tools.Result, error) {
			return t.conn.call(ctx, t.tool, raw)
		},
	}, nil
}
