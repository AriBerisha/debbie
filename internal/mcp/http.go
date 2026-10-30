package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"debbie/internal/netguard"
)

// httpTransport talks to a remote server over Streamable HTTP: each message
// is a POST, answered with one JSON message or an event stream that ends
// with the response. The client comes from netguard, so it reaches only
// the server's host and port, never through a proxy, never following a
// redirect, and with a certificate pin when the config has one.
type httpTransport struct {
	url    string
	client *http.Client
	token  string // sent as "Authorization: Bearer"

	mu      sync.Mutex
	version string // the MCP-Protocol-Version header; "" for none
	legacy  bool
	session string // the handshake era's Mcp-Session-Id
}

func newHTTP(s Server) (*httpTransport, error) {
	ep, err := netguard.EndpointFromURL(s.URL, s.Pin)
	if err != nil {
		return nil, err
	}
	client, err := netguard.NewClient(ep, netguard.Options{
		HeaderTimeout: s.Timeout,
		TrustHint:     fmt.Sprintf("update tls_pin_sha256 of the MCP server %q in the config", s.Name),
	})
	if err != nil {
		return nil, err
	}
	return &httpTransport{url: s.URL, client: client, token: s.Token}, nil
}

func (t *httpTransport) setVersion(version string, legacy bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.version, t.legacy = version, legacy
	if !legacy {
		t.session = ""
	}
}

func (t *httpTransport) state() (version string, legacy bool, session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version, t.legacy, t.session
}

// post sends one message with the headers its era calls for.
func (t *httpTransport) post(ctx context.Context, body []byte, r *request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	version, legacy, session := t.state()
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		h.Set("MCP-Protocol-Version", version)
	}
	if legacy {
		if session != "" {
			h.Set("Mcp-Session-Id", session)
		}
	} else if r != nil {
		h.Set("Mcp-Method", r.method)
		if r.name != "" {
			h.Set("Mcp-Name", headerValue(r.name))
		}
		for name, value := range r.headers {
			h.Set("Mcp-Param-"+name, value)
		}
	}
	if t.token != "" {
		h.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, unwrapURLError(err)
	}
	return resp, nil
}

// unwrapURLError drops the method and URL that net/http puts in its
// errors.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// roundTrip sends a request and reads the response. Over the stateless
// revision, abandoning the response stream is the cancellation; a
// handshake-era server is also told with notifications/cancelled.
func (t *httpTransport) roundTrip(ctx context.Context, r *request) (*message, error) {
	m, err := t.exchange(ctx, r)
	if err != nil && ctx.Err() != nil {
		if _, legacy, _ := t.state(); legacy && !r.noCancel {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			t.notify(cctx, "notifications/cancelled", map[string]any{"requestId": r.id, "reason": "the user or a timeout stopped it"})
			cancel()
		}
		return nil, ctx.Err()
	}
	return m, err
}

func (t *httpTransport) exchange(ctx context.Context, r *request) (*message, error) {
	resp, err := t.post(ctx, r.body(), r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, legacy, session := t.state()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the server refused access (HTTP %s); if it needs a token, set bearer_token_env for it in the config", resp.Status)
	case legacy && session != "" && resp.StatusCode == http.StatusNotFound:
		return nil, errSessionExpired
	case resp.StatusCode >= 300:
		se := &statusError{status: resp.Status, code: resp.StatusCode}
		var m message
		if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); json.Unmarshal(b, &m) == nil && m.Error != nil {
			se.rpc = m.Error
		}
		return nil, se
	}
	if legacy && r.method == "initialize" {
		if id := resp.Header.Get("Mcp-Session-Id"); visibleASCII(id) {
			t.mu.Lock()
			t.session = id
			t.mu.Unlock()
		}
	}
	id := strconv.FormatInt(r.id, 10)
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ct {
	case "application/json":
		var m message
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(&m); err != nil {
			return nil, fmt.Errorf("the server's answer isn't valid JSON: %v", err)
		}
		if !m.isResponse() || !sameID(m.ID, id) {
			return nil, errors.New("the server's answer isn't the response to the request")
		}
		return &m, nil
	case "text/event-stream":
		return t.stream(ctx, resp.Body, id)
	}
	return nil, fmt.Errorf("the server answered with content of type %q", printable(ct, 60))
}

// stream reads an event stream until the response to the request with id
// arrives. Notifications are ignored; requests from a server of the
// handshake era are answered.
func (t *httpTransport) stream(ctx context.Context, body io.Reader, id string) (*message, error) {
	var resp *message
	err := readEvents(body, func(data []byte) bool {
		var m message
		if json.Unmarshal(data, &m) != nil {
			return false
		}
		switch {
		case m.isRequest():
			t.answer(ctx, &m)
		case m.isResponse() && sameID(m.ID, id):
			resp = &m
			return true
		}
		return false
	})
	if resp != nil {
		return resp, nil
	}
	if err == nil || err == io.EOF {
		err = errors.New("the server ended the event stream without a response")
	}
	return nil, err
}

// answer replies to a server's request with a POST of its own.
func (t *httpTransport) answer(ctx context.Context, m *message) {
	resp, err := t.post(ctx, reply(m), nil)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
}

func (t *httpTransport) notify(ctx context.Context, method string, params map[string]any) error {
	resp, err := t.post(ctx, notification(method, params), nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the server refused %s (HTTP %s)", method, resp.Status)
	}
	return nil
}

// close ends a handshake-era session, if the server gave one.
func (t *httpTransport) close() {
	_, legacy, session := t.state()
	if !legacy || session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// readEvents reads server-sent events and passes each event's data to
// each until it returns true or the stream ends.
func readEvents(r io.Reader, each func(data []byte) bool) error {
	return readNamedEvents(r, func(_ string, data []byte) bool { return each(data) })
}

func sameID(raw json.RawMessage, id string) bool {
	s := string(raw)
	if u, err := strconv.Unquote(s); err == nil {
		s = u
	}
	return s == id
}

func visibleASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// headerValue writes a value into a header as the specification says: as
// it is when it's plain ASCII without whitespace at either end, and in the
// =?base64?...?= form otherwise, or when it looks like that form.
func headerValue(s string) string {
	plain := !strings.HasPrefix(s, "=?base64?") || !strings.HasSuffix(s, "?=")
	if plain && s != "" && (s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		plain = false
	}
	for i := 0; plain && i < len(s); i++ {
		if c := s[i]; (c < 0x20 || c > 0x7e) && c != '\t' {
			plain = false
		}
	}
	if plain {
		return s
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}
