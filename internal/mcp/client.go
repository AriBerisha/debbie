// Package mcp is a client for MCP servers (SPEC §22). It starts local
// servers or reaches remote ones, lists their tools and calls them for the
// model.
//
// It speaks the stateless protocol of revision 2026-07-28, where every
// request carries its version and the client's capabilities in _meta, and
// falls back to the initialize handshake of 2025-11-25 and earlier for
// servers that don't, as the specification's backward-compatibility rules
// describe: on stdio it probes with server/discover, and over HTTP it reads
// the body of a 400 before falling back. It offers the server no
// capabilities: no sampling, elicitation or roots.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"debbie/internal/app"
)

// The protocol versions this client speaks.
const (
	ModernVersion = "2026-07-28"
	LegacyVersion = "2025-11-25"
)

// legacyVersions are the handshake-era versions accepted in a server's
// answer to initialize. Their tool messages differ only in fields this
// client reads when present.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes this client looks at.
const (
	codeMethodNotFound     = -32601
	codeHeaderMismatch     = -32020
	codeMissingCapability  = -32021
	codeUnsupportedVersion = -32022
)

// probeTimeout bounds the wait for an answer to server/discover; a server
// of the handshake era may not answer it at all. Tests shorten it.
var probeTimeout = 5 * time.Second

// maxMessage is the largest message accepted from a server.
const maxMessage = 32 << 20

// RPCError is an error a server answered with.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// modern reports whether the error is one that only servers of the
// stateless revision send.
func (e *RPCError) modern() bool {
	switch e.Code {
	case codeHeaderMismatch, codeMissingCapability, codeUnsupportedVersion:
		return true
	}
	return false
}

// message is any JSON-RPC message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *message) isRequest() bool {
	return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null"
}

func (m *message) isResponse() bool { return m.Method == "" && (m.Result != nil || m.Error != nil) }

// request is a request to send.
type request struct {
	id       int64
	method   string
	params   map[string]any
	name     string            // the tool called, for HTTP's Mcp-Name header
	headers  map[string]string // Mcp-Param-* headers, for HTTP
	noCancel bool              // don't send notifications/cancelled when abandoned
}

func (r *request) body() []byte {
	m := map[string]any{"jsonrpc": "2.0", "id": r.id, "method": r.method}
	if r.params != nil {
		m["params"] = r.params
	}
	b, _ := json.Marshal(m)
	return b
}

func notification(method string, params map[string]any) []byte {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return b
}

// reply answers a request a server of the handshake era sent: ping gets an
// empty result; anything else is refused, since this client offers no
// capabilities.
func reply(m *message) []byte {
	out := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if m.Method == "ping" {
		out["result"] = map[string]any{}
	} else {
		out["error"] = map[string]any{"code": codeMethodNotFound, "message": "Method not found"}
	}
	b, _ := json.Marshal(out)
	return b
}

// transport carries messages to one server.
type transport interface {
	// roundTrip sends a request and returns the response to it.
	roundTrip(ctx context.Context, r *request) (*message, error)
	notify(ctx context.Context, method string, params map[string]any) error
	// setVersion sets the protocol version that requests carry, and
	// whether it is of the handshake era.
	setVersion(version string, legacy bool)
	close()
}

// errSessionExpired is returned by a transport whose handshake-era session
// the server ended; the client starts a new one.
var errSessionExpired = errors.New("the server ended the session")

// statusError is an HTTP error status, with the JSON-RPC error in its body
// if there was one.
type statusError struct {
	status string
	code   int
	rpc    *RPCError
}

func (e *statusError) Error() string {
	if e.rpc != nil {
		return fmt.Sprintf("HTTP %s: %v", e.status, e.rpc)
	}
	return "HTTP " + e.status
}

func (e *statusError) Unwrap() error {
	if e.rpc != nil {
		return e.rpc
	}
	return nil
}

// Client talks to one MCP server.
type Client struct {
	t       transport
	http    bool // tools' x-mcp-header parameters are mirrored into headers
	version string
	legacy  bool
	ids     atomic.Int64

	// What the server says about itself, unverified, for /mcp.
	ServerInfo   string
	Instructions string
	caps         capabilities
}

// Version is the protocol version agreed on.
func (c *Client) Version() string { return c.version }

// Close ends the connection; a local server is stopped.
func (c *Client) Close() { c.t.close() }

// connect agrees on a protocol version over t.
func connect(ctx context.Context, t transport, isHTTP bool) (*Client, error) {
	c := &Client{t: t, http: isHTTP}
	if err := c.handshake(ctx); err != nil {
		t.close()
		return nil, err
	}
	return c, nil
}

// handshake finds out which era the server is of: it probes with
// server/discover and falls back to initialize when the answer, or its
// absence, shows a server of the handshake era.
func (c *Client) handshake(ctx context.Context) error {
	c.version, c.legacy = ModernVersion, false
	c.t.setVersion(ModernVersion, false)
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	res, err := c.send(pctx, &request{method: "server/discover", noCancel: true})
	timedOut := errors.Is(pctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err == nil {
		var d struct {
			SupportedVersions []string                   `json:"supportedVersions"`
			Capabilities      json.RawMessage            `json:"capabilities"`
			Instructions      string                     `json:"instructions"`
			Meta              map[string]json.RawMessage `json:"_meta"`
		}
		json.Unmarshal(res, &d)
		if len(d.SupportedVersions) > 0 && !slices.Contains(d.SupportedVersions, ModernVersion) {
			if hasLegacy(d.SupportedVersions) {
				return c.initialize(ctx)
			}
			return unsupported(d.SupportedVersions)
		}
		c.ServerInfo = implementation(d.Meta["io.modelcontextprotocol/serverInfo"])
		c.Instructions = d.Instructions
		c.caps = parseCapabilities(d.Capabilities)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var se *statusError
	var rpc *RPCError
	switch {
	case timedOut:
		return c.initialize(ctx)
	case errors.As(err, &se) && (se.rpc == nil || !se.rpc.modern()):
		if se.code == 400 || se.code == 404 || se.code == 405 {
			return c.initialize(ctx)
		}
		return err
	case errors.As(err, &rpc) && rpc.modern():
		if rpc.Code == codeUnsupportedVersion {
			var data struct {
				Supported []string `json:"supported"`
			}
			json.Unmarshal(rpc.Data, &data)
			if hasLegacy(data.Supported) {
				return c.initialize(ctx)
			}
			return unsupported(data.Supported)
		}
		return fmt.Errorf("the server refused the connection: %w", rpc)
	case errors.As(err, &rpc):
		return c.initialize(ctx) // a handshake-era server, refusing a method it doesn't know
	}
	return err
}

func hasLegacy(versions []string) bool {
	for _, v := range versions {
		if slices.Contains(legacyVersions, v) {
			return true
		}
	}
	return false
}

func unsupported(versions []string) error {
	return fmt.Errorf("the server speaks MCP %s; this client speaks %s, and %s with the older handshake",
		strings.Join(versions, ", "), ModernVersion, LegacyVersion)
}

// initialize runs the handshake of the older revisions.
func (c *Client) initialize(ctx context.Context) error {
	c.legacy = true
	c.t.setVersion("", true)
	res, err := c.send(ctx, &request{method: "initialize", params: map[string]any{
		"protocolVersion": LegacyVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": app.Name, "version": app.Version},
	}})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var r struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
		Instructions    string          `json:"instructions"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return fmt.Errorf("initialize: the answer isn't valid: %v", err)
	}
	if !slices.Contains(legacyVersions, r.ProtocolVersion) {
		return fmt.Errorf("the server answered initialize with MCP version %q, which this client doesn't speak", r.ProtocolVersion)
	}
	c.version = r.ProtocolVersion
	c.t.setVersion(r.ProtocolVersion, true)
	c.ServerInfo, c.Instructions = implementation(r.ServerInfo), r.Instructions
	c.caps = parseCapabilities(r.Capabilities)
	return c.t.notify(ctx, "notifications/initialized", nil)
}

// implementation describes a server's Implementation object: its name and
// version, cleaned up for display.
func implementation(raw json.RawMessage) string {
	var impl struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &impl) != nil || impl.Name == "" {
		return ""
	}
	s := impl.Name
	if impl.Version != "" {
		s += " " + impl.Version
	}
	return printable(s, 80)
}

// meta is the _meta every request of the stateless revision carries.
func (c *Client) meta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    c.version,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": app.Name, "version": app.Version},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
}

// send sends a request and returns its result. A handshake-era session the
// server ended is started again once.
func (c *Client) send(ctx context.Context, r *request) (json.RawMessage, error) {
	if !c.legacy {
		if r.params == nil {
			r.params = map[string]any{}
		}
		r.params["_meta"] = c.meta()
	}
	r.id = c.ids.Add(1)
	m, err := c.t.roundTrip(ctx, r)
	if errors.Is(err, errSessionExpired) && r.method != "initialize" {
		if err := c.initialize(ctx); err != nil {
			return nil, err
		}
		r.id = c.ids.Add(1)
		m, err = c.t.roundTrip(ctx, r)
	}
	if err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, m.Error
	}
	var rt struct {
		ResultType string `json:"resultType"`
	}
	json.Unmarshal(m.Result, &rt)
	switch rt.ResultType {
	case "", "complete":
		return m.Result, nil
	case "input_required":
		return nil, errors.New("the server asked for input (elicitation, sampling or roots), which this client doesn't offer")
	}
	return nil, fmt.Errorf("the server answered with an unknown result type %q", printable(rt.ResultType, 40))
}

// printable keeps the printable characters of s, at most n of them.
func printable(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if n == 0 {
			b.WriteString("…")
			break
		}
		if r == '\n' || r == '\t' {
			r = ' '
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
		n--
	}
	return b.String()
}
