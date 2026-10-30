package browser

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"debbie/internal/app"
)

// IsHostLaunch reports whether Chrome started the binary as the native
// messaging host: its first argument is the calling extension's origin,
// and on Windows a --parent-window argument follows (developer.chrome.com,
// "Native messaging").
func IsHostLaunch(args []string) bool {
	return len(args) > 0 && strings.HasPrefix(args[0], "chrome-extension://")
}

// How often a waiting host looks for a session, and how long it waits
// after one refused it.
var (
	hostPoll  = time.Second
	hostRetry = 5 * time.Second
)

// RunHost is the native messaging host. It relays messages between the
// extension, over in and out, and the session that holds the browser,
// over the bridge's socket in dir; while no session does, it waits for
// one. It returns when Chrome closes in.
func RunHost(origin string, in io.Reader, out io.Writer, dir string) error {
	h := &host{out: out, origin: origin}
	fromChrome := make(chan []byte)
	chromeGone := make(chan struct{})
	go func() {
		defer close(chromeGone)
		for {
			msg, err := readFrame(in, maxFromChrome)
			if err != nil {
				return
			}
			select {
			case fromChrome <- msg:
			case <-time.After(callTimeout):
				// Nobody takes it: the session went while it came.
			}
		}
	}()

	for {
		conn, owner, err := h.dial(dir)
		if err != nil {
			wait := hostPoll
			if err != errNoSession {
				fmt.Fprintf(os.Stderr, "%s host: %v\n", app.Name, err)
				wait = hostRetry
			}
			select {
			case <-chromeGone:
				return nil
			case <-fromChrome: // a late reply for a session that went
			case <-time.After(wait):
			}
			continue
		}
		h.tell(map[string]any{"event": "session", "connected": true, "pid": owner.PID, "workspace": owner.Workspace})
		gone := h.relay(conn, fromChrome, chromeGone)
		conn.Close()
		if gone {
			return nil
		}
		h.tell(map[string]any{"event": "session", "connected": false})
	}
}

type host struct {
	out    io.Writer
	outMu  sync.Mutex
	origin string
}

var errNoSession = fmt.Errorf("no session holds the browser")

// dial connects to the session holding the browser and proves itself with
// the token.
func (h *host) dial(dir string) (net.Conn, Owner, error) {
	sock := filepath.Join(dir, socketFile)
	if _, err := os.Lstat(sock); err != nil {
		return nil, Owner{}, errNoSession
	}
	token, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		return nil, Owner{}, errNoSession // it is starting or going
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, Owner{}, errNoSession
	}
	data, _ := json.Marshal(hello{V: protocolVersion, Token: strings.TrimSpace(string(token)), Origin: h.origin})
	conn.SetDeadline(time.Now().Add(helloTimeout))
	if err := writeFrame(conn, data, maxToChrome); err != nil {
		conn.Close()
		return nil, Owner{}, err
	}
	data, err = readFrame(conn, 4096)
	if err != nil {
		conn.Close()
		return nil, Owner{}, fmt.Errorf("the session closed the connection: %v", err)
	}
	var answer hello
	if err := json.Unmarshal(data, &answer); err != nil || !answer.OK {
		conn.Close()
		if answer.Error != "" {
			return nil, Owner{}, fmt.Errorf("the session refused: %s", answer.Error)
		}
		return nil, Owner{}, fmt.Errorf("the session refused the connection")
	}
	conn.SetDeadline(time.Time{})
	return conn, Owner{PID: answer.PID, Workspace: answer.Workspace}, nil
}

// relay passes messages both ways until the session or Chrome goes,
// reporting whether Chrome went.
func (h *host) relay(conn net.Conn, fromChrome <-chan []byte, chromeGone <-chan struct{}) bool {
	sessionGone := make(chan struct{})
	go func() {
		defer close(sessionGone)
		for {
			msg, err := readFrame(conn, maxToChrome)
			if err != nil {
				return
			}
			h.outMu.Lock()
			err = writeFrame(h.out, msg, maxToChrome)
			h.outMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case msg := <-fromChrome:
			if writeFrame(conn, msg, maxFromChrome) != nil {
				<-sessionGone
				return false
			}
		case <-sessionGone:
			return false
		case <-chromeGone:
			return true
		}
	}
}

// tell sends the extension a message of the host's own.
func (h *host) tell(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.outMu.Lock()
	defer h.outMu.Unlock()
	writeFrame(h.out, data, maxToChrome)
}
