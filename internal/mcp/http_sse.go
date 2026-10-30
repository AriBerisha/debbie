package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"debbie/internal/netguard"
)

// sseTransport talks to a server of the HTTP+SSE transport of 2024-11-05,
// deprecated since but still met: a GET opens an event stream whose first
// event, "endpoint", names where to POST messages; the server's messages,
// responses among them, arrive as "message" events on the stream. It is
// tried only after both Streamable HTTP attempts failed as the
// specification describes for an older server.
type sseTransport struct {
	client   *http.Client
	token    string
	endpoint string
	stop     context.CancelFunc // closes the event stream

	mu      sync.Mutex
	pending map[string]chan *message
	done    chan struct{} // closed when the stream ends
	err     error
}

// openSSE opens the event stream and waits for the endpoint to POST to,
// which must be on the same scheme, host and port.
func openSSE(ctx context.Context, s Server) (*sseTransport, error) {
	base, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
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
	streamCtx, stop := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, s.URL, nil)
	if err != nil {
		stop()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	// The stream outlives ctx, which bounds only the wait for its start.
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := client.Do(req)
		got <- answer{resp, err}
	}()
	var resp *http.Response
	select {
	case a := <-got:
		if a.err != nil {
			stop()
			return nil, unwrapURLError(a.err)
		}
		resp = a.resp
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		resp.Body.Close()
		stop()
		return nil, fmt.Errorf("a GET for an event stream got HTTP %s (%s)", resp.Status, printable(ct, 40))
	}
	t := &sseTransport{client: client, token: s.Token, stop: stop, pending: map[string]chan *message{}, done: make(chan struct{})}
	endpoint := make(chan string, 1)
	go t.read(resp.Body, endpoint)
	select {
	case e := <-endpoint:
		u, err := base.Parse(e)
		if err != nil || u.Scheme != base.Scheme || u.Host != base.Host {
			t.close()
			return nil, errors.New("the server's endpoint for messages isn't on its own host and port")
		}
		t.endpoint = u.String()
		return t, nil
	case <-t.done:
		return nil, t.err
	case <-ctx.Done():
		t.close()
		return nil, ctx.Err()
	}
}

// read dispatches the stream's events until it ends.
func (t *sseTransport) read(body io.ReadCloser, endpoint chan<- string) {
	defer body.Close()
	sent := false
	err := readNamedEvents(body, func(event string, data []byte) bool {
		switch event {
		case "endpoint":
			if !sent {
				endpoint <- string(bytes.TrimSpace(data))
				sent = true
			}
		case "", "message":
			var m message
			if json.Unmarshal(data, &m) != nil {
				return false
			}
			switch {
			case m.isRequest():
				go t.post(context.Background(), reply(&m))
			case m.isResponse():
				id := string(m.ID)
				if s, err := strconv.Unquote(id); err == nil {
					id = s
				}
				t.mu.Lock()
				ch := t.pending[id]
				delete(t.pending, id)
				t.mu.Unlock()
				if ch != nil {
					ch <- &m
				}
			}
		}
		return false
	})
	t.mu.Lock()
	if err == nil || err == io.EOF {
		err = errors.New("the server closed its event stream")
	}
	t.err = err
	t.mu.Unlock()
	close(t.done)
}

// post sends one message to the endpoint; the answer comes on the stream.
func (t *sseTransport) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return unwrapURLError(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &statusError{status: resp.Status, code: resp.StatusCode}
	}
	return nil
}

func (t *sseTransport) roundTrip(ctx context.Context, r *request) (*message, error) {
	id := strconv.FormatInt(r.id, 10)
	ch := make(chan *message, 1)
	t.mu.Lock()
	t.pending[id] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()
	if err := t.post(ctx, r.body()); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	select {
	case m := <-ch:
		return m, nil
	case <-t.done:
		return nil, t.err
	case <-ctx.Done():
		if !r.noCancel {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			t.notify(cctx, "notifications/cancelled", map[string]any{"requestId": r.id, "reason": "the user or a timeout stopped it"})
			cancel()
		}
		return nil, ctx.Err()
	}
}

func (t *sseTransport) notify(ctx context.Context, method string, params map[string]any) error {
	return t.post(ctx, notification(method, params))
}

func (t *sseTransport) setVersion(string, bool) {}

func (t *sseTransport) close() { t.stop() }

// readNamedEvents reads server-sent events, passing each event's name and
// data to each until it returns true or the stream ends.
func readNamedEvents(r io.Reader, each func(event string, data []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []byte
	event, hasData := "", false
	for {
		line, err := readLine(br)
		if err != nil && err != io.EOF {
			return err
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if hasData && each(event, data) {
				return nil
			}
			data, event, hasData = data[:0], "", false
		case line[0] == ':': // a comment, such as a keep-alive
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "data":
				if hasData {
					data = append(data, '\n')
				}
				data, hasData = append(data, value...), true
				if len(data) > maxMessage {
					return fmt.Errorf("an event from the server is larger than %d MB", maxMessage>>20)
				}
			case "event":
				event = string(value)
			}
		}
		if err == io.EOF {
			if hasData && each(event, data) {
				return nil
			}
			return io.EOF
		}
	}
}
