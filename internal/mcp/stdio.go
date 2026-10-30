package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"debbie/internal/tools"
)

// stdio talks to a server the agent started, one JSON-RPC message per line
// on the server's standard input and output.
type stdio struct {
	child  *tools.Child
	stdin  io.WriteCloser
	stderr *tail

	wmu     sync.Mutex // serializes writes to stdin
	mu      sync.Mutex // guards pending and exit
	pending map[string]chan *message
	done    chan struct{} // closed when the server's output has ended
	exit    error         // why, once done is closed
}

// startStdio starts the server's program. It doesn't wait for anything.
func startStdio(s Server) (*stdio, error) {
	cmd := tools.NewChild(s.Command, s.Args...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("can't start %s: %v", s.Command, cmd.Err)
	}
	cmd.Env, cmd.Dir = s.Env, s.Dir
	t := &stdio{stderr: &tail{max: 4096}, pending: map[string]chan *message{}, done: make(chan struct{})}
	cmd.Stderr = t.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if t.child, err = tools.StartChild(cmd); err != nil {
		return nil, fmt.Errorf("can't start %s: %v", s.Command, err)
	}
	t.stdin = stdin
	go t.read(stdout)
	return t, nil
}

// read dispatches the server's messages until its output ends, then waits
// for it to exit.
func (t *stdio) read(stdout io.Reader) {
	br := bufio.NewReaderSize(stdout, 64<<10)
	var readErr error
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			t.dispatch(line)
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
				t.child.Kill()
			}
			break
		}
	}
	waitErr := t.child.Cmd.Wait()
	t.child.Close()
	t.mu.Lock()
	switch {
	case readErr != nil:
		t.exit = readErr
	case waitErr != nil:
		t.exit = fmt.Errorf("the server exited (%v)", waitErr)
	default:
		t.exit = errors.New("the server exited")
	}
	if last := t.stderr.lastLine(); last != "" {
		t.exit = fmt.Errorf("%w: %s", t.exit, last)
	}
	t.mu.Unlock()
	close(t.done)
}

// readLine reads one line, at most maxMessage bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxMessage {
			return nil, fmt.Errorf("a message from the server is larger than %d MB", maxMessage>>20)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func (t *stdio) dispatch(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		t.stderr.Write(append(line, '\n')) // not a message: a server that logs to stdout
		return
	}
	var m message
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch {
	case m.isRequest():
		t.write(reply(&m))
	case m.isResponse():
		id := string(m.ID)
		if s, err := strconv.Unquote(id); err == nil {
			id = s // an ID echoed as a string
		}
		t.mu.Lock()
		ch := t.pending[id]
		delete(t.pending, id)
		t.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
	// Notifications, such as progress and logging, are ignored.
}

func (t *stdio) write(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err := t.stdin.Write(append(b, '\n'))
	return err
}

func (t *stdio) setVersion(string, bool) {}

func (t *stdio) roundTrip(ctx context.Context, r *request) (*message, error) {
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
	if err := t.write(r.body()); err != nil {
		select {
		case <-t.done:
			return nil, t.exitErr()
		case <-time.After(time.Second):
			return nil, fmt.Errorf("writing to the server: %v", err)
		}
	}
	select {
	case m := <-ch:
		return m, nil
	case <-t.done:
		return nil, t.exitErr()
	case <-ctx.Done():
		if !r.noCancel {
			t.notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": r.id, "reason": "the user or a timeout stopped it"})
		}
		return nil, ctx.Err()
	}
}

func (t *stdio) notify(_ context.Context, method string, params map[string]any) error {
	return t.write(notification(method, params))
}

func (t *stdio) exitErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit
}

// close stops the server: it closes its input, which tells it to exit, and
// ends it, with everything it started, if it hasn't within two seconds.
func (t *stdio) close() {
	t.stdin.Close()
	select {
	case <-t.done:
		return
	case <-time.After(2 * time.Second):
	}
	t.child.Kill()
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}

// alive reports whether the server is still running.
func (t *stdio) alive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// tail keeps the end of what a server writes to stderr, for error messages.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// lastLine is the last line written that isn't blank.
func (t *tail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return printable(strings.TrimSpace(lines[len(lines)-1]), 300)
}
