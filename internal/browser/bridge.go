package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"debbie/internal/app"
	"debbie/internal/config"
)

// protocolVersion is the version of the messages between the session, the
// host and the extension (SPEC §20.3).
const protocolVersion = 1

// Call timeouts (SPEC §20.3). Variables, so tests can shorten them.
var (
	callTimeout     = 30 * time.Second
	navigateTimeout = 60 * time.Second
	helloTimeout    = 5 * time.Second
	// askTimeout is the wait once the extension says it is asking the user
	// whether the agent may open a site (SPEC §20.0): the extension gives
	// them two minutes, and this is the fallback.
	askTimeout = 150 * time.Second
)

// maxSocketPath is the size of a Unix socket address's path, with its
// terminating zero: 108 bytes on Windows and Linux; macOS has 104, and the
// smaller is the limit.
const maxSocketPath = 104

// The bridge's files in its private directory.
const (
	socketFile = "browser.sock"
	tokenFile  = "token"
	ownerFile  = "owner.json"
)

// Owner is the session that holds the browser.
type Owner struct {
	PID       int    `json:"pid"`
	Workspace string `json:"workspace"`
}

// ErrBusy is returned while another session holds the browser.
type ErrBusy struct{ Owner Owner }

func (e *ErrBusy) Error() string {
	if e.Owner.PID == 0 {
		return "another " + app.Name + " session is using the browser"
	}
	return fmt.Sprintf("another %s session (process %d, in %s) is using the browser", app.Name, e.Owner.PID, e.Owner.Workspace)
}

// ErrNotConnected is returned while no extension is connected.
var ErrNotConnected = errors.New("the Chrome extension isn't connected: install it with \"" + app.Name +
	" browser install\", load it in Chrome, and see /web browser")

// Bridge is the session's end of the browser bridge (SPEC §20.3): a Unix
// socket in a private directory, which the native host Chrome starts
// connects to with the token written next to it.
type Bridge struct {
	dir       string
	workspace string

	mu      sync.Mutex
	ln      net.Listener
	token   string
	host    *hostConn // the host connected now; nil for none
	pending map[int64]*pendingCall
	nextID  int64
	ext     Extension // what the extension said when it connected
	asking  string    // the site the extension is asking the user about; "" for none
}

// pendingCall is a call the extension hasn't answered.
type pendingCall struct {
	ch     chan reply
	asking chan string // the site the extension asks the user about, which lengthens the wait
}

// Extension describes the connected extension.
type Extension struct {
	Version string   `json:"version"`
	Build   string   `json:"build"` // its manifest's version_name, see VersionName
	Sites   []string `json:"sites"`
	Name    string   `json:"name"` // the browser's name, which the user gives it in the popup
}

// Label names the browser for messages: "Chrome “Work laptop”", or
// "Chrome" when it has no name.
func (e Extension) Label() string {
	if e.Name == "" {
		return "Chrome"
	}
	return "Chrome “" + e.Name + "”"
}

// Current reports whether Chrome runs the extension this binary installs.
func (e Extension) Current() bool { return e.Build == VersionName() }

type hostConn struct {
	conn    net.Conn
	writeMu sync.Mutex
}

type reply struct {
	result json.RawMessage
	err    string
}

// NewBridge returns a bridge in dir for a session in workspace.
func NewBridge(dir, workspace string) *Bridge {
	return &Bridge{dir: dir, workspace: workspace, pending: map[int64]*pendingCall{}}
}

// Start takes the browser for this session, unless another session holds
// it (*ErrBusy). Starting a started bridge does nothing.
func (b *Bridge) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ln != nil {
		return nil
	}
	if err := config.PrivateDir(b.dir); err != nil {
		return fmt.Errorf("the bridge's directory: %w", err)
	}
	sock := filepath.Join(b.dir, socketFile)
	if len(sock) >= maxSocketPath {
		return fmt.Errorf("the bridge's socket, %s, has a path of %d characters, and a Unix socket's may have %d at most", sock, len(sock), maxSocketPath-1)
	}
	if _, err := os.Lstat(sock); err == nil {
		// A live session answers; a dead one left its files behind.
		if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			c.Close()
			return &ErrBusy{Owner: readOwner(b.dir)}
		}
		for _, f := range []string{socketFile, tokenFile, ownerFile} {
			os.Remove(filepath.Join(b.dir, f))
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		if _, statErr := os.Lstat(sock); statErr == nil {
			return &ErrBusy{Owner: readOwner(b.dir)} // another session was quicker
		}
		return fmt.Errorf("listening on %s: %w", sock, err)
	}
	var raw [32]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	owner, _ := json.Marshal(Owner{PID: os.Getpid(), Workspace: b.workspace})
	if err := config.WritePrivate(filepath.Join(b.dir, tokenFile), []byte(token)); err != nil {
		ln.Close()
		return err
	}
	if err := config.WritePrivate(filepath.Join(b.dir, ownerFile), owner); err != nil {
		ln.Close()
		return err
	}
	b.ln, b.token = ln, token
	go b.accept(ln)
	return nil
}

// Stop gives the browser up: the host is disconnected and the bridge's
// files go.
func (b *Bridge) Stop() {
	b.mu.Lock()
	ln, host := b.ln, b.host
	b.ln, b.host, b.token = nil, nil, ""
	b.mu.Unlock()
	if ln == nil {
		return
	}
	ln.Close() // removes the socket
	if host != nil {
		host.conn.Close()
	}
	for _, f := range []string{socketFile, tokenFile, ownerFile} {
		os.Remove(filepath.Join(b.dir, f))
	}
}

// Status describes the bridge, for /web browser.
type Status struct {
	Started   bool
	Connected bool
	Extension Extension
	Socket    string
	// Asking is the site the extension is asking the user about, in a
	// window of Chrome's, while a call waits for the answer (SPEC §20.0);
	// "" when none is.
	Asking string
}

func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Started: b.ln != nil, Connected: b.host != nil, Extension: b.ext, Socket: filepath.Join(b.dir, socketFile), Asking: b.asking}
}

// accept takes connections from hosts until the listener closes.
func (b *Bridge) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go b.greet(conn)
	}
}

// hello is the host's first message, and the session's answer.
type hello struct {
	V         int    `json:"v"`
	Token     string `json:"token,omitempty"`
	Origin    string `json:"origin,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	Error     string `json:"error,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// greet checks a new connection: a process of this user (peerCheck) that
// sends the token first. One host at a time is connected.
func (b *Bridge) greet(conn net.Conn) {
	refuse := func(msg string) {
		data, _ := json.Marshal(hello{V: protocolVersion, Error: msg})
		writeFrame(conn, data, maxToChrome)
		conn.Close()
	}
	if err := peerCheck(conn); err != nil {
		conn.Close()
		return
	}
	conn.SetDeadline(time.Now().Add(helloTimeout))
	data, err := readFrame(conn, 4096)
	if err != nil {
		conn.Close()
		return
	}
	var h hello
	b.mu.Lock()
	token := b.token
	b.mu.Unlock()
	switch {
	case json.Unmarshal(data, &h) != nil:
		conn.Close()
		return
	case subtle.ConstantTimeCompare([]byte(h.Token), []byte(token)) != 1:
		conn.Close()
		return
	case h.V != protocolVersion:
		refuse(fmt.Sprintf("this session speaks version %d of the bridge protocol and the host %d: run %s browser install again", protocolVersion, h.V, app.Name))
		return
	}
	hc := &hostConn{conn: conn}
	b.mu.Lock()
	if b.host != nil || b.ln == nil {
		b.mu.Unlock()
		refuse("another browser is already connected to this session")
		return
	}
	b.host = hc
	b.mu.Unlock()
	conn.SetDeadline(time.Time{})
	ok, _ := json.Marshal(hello{V: protocolVersion, OK: true, PID: os.Getpid(), Workspace: b.workspace})
	if err := writeFrame(conn, ok, maxToChrome); err != nil {
		b.drop(hc)
		return
	}
	go b.read(hc)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), helloTimeout)
		defer cancel()
		res, err := b.Call(ctx, "hello", nil)
		var ext Extension
		if err == nil && json.Unmarshal(res, &ext) == nil {
			b.mu.Lock()
			b.ext = ext
			b.mu.Unlock()
		}
	}()
}

// message is anything that comes from the extension: a reply to a call,
// or an event.
type message struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *string         `json:"error"`
	Event  string          `json:"event"`
	Name   string          `json:"name"`  // of the "name" event: the browser's new name
	Sites  []string        `json:"sites"` // of the "sites" event: the sites allowed now
	Call   *int64          `json:"call"`  // of the "asking" event: the call it is for
	Site   string          `json:"site"`  // of the "asking" event: the site asked about
}

// read takes the extension's messages until the host goes.
func (b *Bridge) read(hc *hostConn) {
	defer b.drop(hc)
	for {
		data, err := readFrame(hc.conn, maxFromChrome)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID == nil {
			b.event(m)
			continue
		}
		b.mu.Lock()
		c := b.pending[*m.ID]
		delete(b.pending, *m.ID)
		b.mu.Unlock()
		if c == nil {
			continue // its caller gave up
		}
		r := reply{result: m.Result}
		if m.Error != nil {
			r.err = *m.Error
		} else if len(bytes.TrimSpace(m.Result)) == 0 {
			r.result = json.RawMessage("null")
		}
		c.ch <- r
	}
}

// event takes what the extension says of its own accord: the user renamed
// the browser in the popup, the sites changed (the options page, or an
// answer to the window that asks about a site), or the extension is
// asking the user whether the agent may open a site, which lengthens the
// wait of the call it is for.
func (b *Bridge) event(m message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch m.Event {
	case "name":
		b.ext.Name = m.Name
	case "sites":
		b.ext.Sites = m.Sites
	case "asking":
		if m.Call == nil || m.Site == "" {
			return
		}
		if c := b.pending[*m.Call]; c != nil {
			select {
			case c.asking <- m.Site:
			default:
			}
		}
	}
}

// drop forgets a host that went, failing the calls it didn't answer.
func (b *Bridge) drop(hc *hostConn) {
	hc.conn.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.host != hc {
		return
	}
	b.host, b.ext = nil, Extension{}
	for id, c := range b.pending {
		c.ch <- reply{err: "the browser disconnected"}
		delete(b.pending, id)
	}
}

// Call asks the extension to do something and waits for its answer: 30
// seconds at most, or 60 for a navigation, or 150 once the extension says
// it is asking the user whether the agent may open a site. A call given
// up on, for the context or the time, is cancelled in the extension, which
// closes the window asking about a site.
func (b *Bridge) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	hc := b.host
	if hc == nil {
		started := b.ln != nil
		b.mu.Unlock()
		if !started {
			return nil, errors.New("web access is off")
		}
		return nil, ErrNotConnected
	}
	b.nextID++
	id := b.nextID
	c := &pendingCall{ch: make(chan reply, 1), asking: make(chan string, 1)}
	b.pending[id] = c
	b.mu.Unlock()
	asked := ""
	done := func() {
		b.mu.Lock()
		delete(b.pending, id)
		if asked != "" {
			b.asking = ""
		}
		b.mu.Unlock()
	}
	giveUp := func() {
		done()
		data, _ := json.Marshal(map[string]any{"v": protocolVersion, "event": "cancel", "call": id})
		hc.writeMu.Lock()
		writeFrame(hc.conn, data, maxToChrome)
		hc.writeMu.Unlock()
	}

	data, err := json.Marshal(map[string]any{"v": protocolVersion, "id": id, "method": method, "params": params})
	if err != nil {
		done()
		return nil, err
	}
	hc.writeMu.Lock()
	err = writeFrame(hc.conn, data, maxToChrome)
	hc.writeMu.Unlock()
	if err != nil {
		done()
		return nil, fmt.Errorf("sending to the browser: %w", err)
	}
	timeout := callTimeout
	if method == "navigate" {
		timeout = navigateTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case r := <-c.ch:
			done()
			if r.err != "" {
				return nil, errors.New(r.err)
			}
			return r.result, nil
		case site := <-c.asking:
			// The user has the extension's two minutes to answer.
			asked, timeout = site, askTimeout
			b.mu.Lock()
			b.asking = site
			b.mu.Unlock()
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(timeout)
		case <-ctx.Done():
			giveUp()
			return nil, ctx.Err()
		case <-timer.C:
			giveUp()
			if asked != "" {
				return nil, fmt.Errorf("refused: nobody answered, within %s, whether the agent may open %s", timeout, asked)
			}
			return nil, fmt.Errorf("the browser didn't answer within %s", timeout)
		}
	}
}

// readOwner returns the session holding the browser, as far as its file
// says.
func readOwner(dir string) Owner {
	var o Owner
	if data, err := os.ReadFile(filepath.Join(dir, ownerFile)); err == nil {
		json.Unmarshal(data, &o)
	}
	return o
}

// HolderOf returns the session that holds the browser now, if one does: a
// session that ended without cleaning up left files, but nothing answers.
func HolderOf(dir string) (Owner, bool) {
	c, err := net.DialTimeout("unix", filepath.Join(dir, socketFile), time.Second)
	if err != nil {
		return Owner{}, false
	}
	c.Close()
	o := readOwner(dir)
	return o, o.PID != 0
}
