// Package sessions keeps conversations so they can be resumed (SPEC §12):
// each is a JSON Lines file, readable only by the user, in a folder of the
// state directory for each workspace. The first line describes the
// session; the messages follow as the conversation grows, and compaction
// writes the conversation that replaces them. API keys never reach the
// files: every line goes through the redactor.
package sessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"debbie/internal/app"
	"debbie/internal/provider"
)

// version is the format of the files.
const version = 1

// Root is the folder of all sessions: %LOCALAPPDATA%\debbie\sessions on
// Windows, $XDG_STATE_HOME/debbie/sessions (or ~/.local/state) elsewhere.
func Root() (string, error) {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" && filepath.IsAbs(d) {
			return filepath.Join(d, app.Name, "sessions"), nil
		}
		return "", errors.New("LOCALAPPDATA isn't set")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, app.Name, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", app.Name, "sessions"), nil
}

// Dir is the folder of a workspace's sessions, named by a hash of its path.
func Dir(root, workspace string) string {
	key := filepath.Clean(workspace)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key) // the same folder however it is spelled
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(root, hex.EncodeToString(sum[:8]))
}

// Meta is a session's first line.
type Meta struct {
	Type      string    `json:"type"` // "session"
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	Created   time.Time `json:"created"`
	Title     string    `json:"title"` // the first message, short
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
}

// A record is a line after the first: a message that joined the
// conversation, the conversation that replaced it all (after
// compaction), the mark that web content joined it, which taints a
// resumed session as it did the first (SPEC §19.5), or a title the user
// gave it (/rename).
type record struct {
	Type     string        `json:"type"` // "message", "replace", "tainted" or "title"
	Message  *diskMessage  `json:"message,omitempty"`
	Messages []diskMessage `json:"messages,omitempty"`
	Title    string        `json:"title,omitempty"`
}

// diskMessage is a message as the files keep it, apart from the provider
// package's types, so that a change there doesn't spoil saved sessions.
// Images are left out, which keeps the files small; a note says so.
type diskMessage struct {
	Role  string     `json:"role"`
	Parts []diskPart `json:"parts"`
}

type diskPart struct {
	Kind    string `json:"kind"` // "text", "reasoning", "call" or "result"
	Text    string `json:"text,omitempty"`
	ID      string `json:"id,omitempty"` // the call's, or the call a result answers
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Images  int    `json:"images,omitempty"` // how many were left out
	Opaque  string `json:"opaque,omitempty"`
}

var kindNames = map[provider.PartKind]string{
	provider.PartText: "text", provider.PartReasoning: "reasoning", provider.PartToolCall: "call", provider.PartToolResult: "result",
}

func toDisk(m provider.Message) diskMessage {
	d := diskMessage{Role: string(m.Role)}
	for _, p := range m.Parts {
		dp := diskPart{Kind: kindNames[p.Kind], Text: p.Text, Opaque: string(p.Opaque)}
		if c := p.ToolCall; c != nil {
			dp.ID, dp.Name, dp.Args = c.ID, c.Name, string(c.Args)
		}
		if r := p.ToolResult; r != nil {
			dp.ID, dp.Content, dp.IsError, dp.Images = r.CallID, r.Content, r.IsError, len(r.Images)
		}
		d.Parts = append(d.Parts, dp)
	}
	return d
}

func fromDisk(d diskMessage) provider.Message {
	m := provider.Message{Role: provider.Role(d.Role)}
	for _, dp := range d.Parts {
		p := provider.Part{Text: dp.Text}
		if dp.Opaque != "" {
			p.Opaque = json.RawMessage(dp.Opaque)
		}
		switch dp.Kind {
		case "reasoning":
			p.Kind = provider.PartReasoning
		case "call":
			p.Kind, p.ToolCall = provider.PartToolCall, &provider.ToolCall{ID: dp.ID, Name: dp.Name, Args: json.RawMessage(dp.Args)}
		case "result":
			content := dp.Content
			if dp.Images > 0 {
				content += fmt.Sprintf("\n[%d image(s) came with this result; the saved session doesn't keep them.]", dp.Images)
			}
			p.Kind, p.ToolResult = provider.PartToolResult, &provider.ToolResult{CallID: dp.ID, Content: content, IsError: dp.IsError}
		default:
			p.Kind = provider.PartText
		}
		m.Parts = append(m.Parts, p)
	}
	return m
}

// Recorder writes a conversation to its session file as it changes. Its
// zero value records nothing. It never fails the conversation: the first
// error stops the recording, and Err reports it.
type Recorder struct {
	dir       string
	workspace string
	redact    func(string) string
	// describe names the provider and the model in use, for a new file.
	describe func() (provider, model string)
	now      func() time.Time

	mu      sync.Mutex
	path    string // "" until the first message is written
	written int    // the history's messages in the file
	tainted bool   // web content joined the conversation
	marked  bool   // the file says so
	title   string // the conversation's title; "" takes the first message's
	err     error
}

// NewRecorder records the conversations of workspace in its folder under
// root (Dir). redact, if not nil, cleans every line of secrets.
func NewRecorder(root, workspace string, redact func(string) string, describe func() (provider, model string)) *Recorder {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	return &Recorder{dir: Dir(root, workspace), workspace: workspace, redact: redact, describe: describe, now: time.Now}
}

// Record learns the conversation after a change. replaced is false when
// history only grew since the last call, and its new messages are added;
// true when it was started over: an empty history (/clear) ends the file,
// and the next message starts another, while compaction's new
// conversation is written whole.
func (r *Recorder) Record(history []provider.Message, replaced bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	switch {
	case replaced && len(history) == 0:
		r.path, r.written, r.tainted, r.marked, r.title = "", 0, false, false, ""
		return
	case replaced && r.path != "":
		msgs := make([]diskMessage, len(history))
		for i, m := range history {
			msgs[i] = toDisk(m)
		}
		r.write(record{Type: "replace", Messages: msgs})
		r.written = len(history)
		return
	case replaced:
		r.written = 0
	}
	if len(history) <= r.written {
		return
	}
	if r.path == "" && !r.start(history) {
		return
	}
	for _, m := range history[r.written:] {
		d := toDisk(m)
		if !r.write(record{Type: "message", Message: &d}) {
			return
		}
	}
	r.written = len(history)
	r.mark()
}

// start creates the file for a conversation whose first message is
// history[0]; it reports whether it could.
func (r *Recorder) start(history []provider.Message) bool {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		r.err = err
		return false
	}
	now := r.now()
	if r.title == "" {
		r.title = Title(history)
	}
	meta := Meta{Type: "session", Version: version, Workspace: r.workspace, Created: now.UTC(), Title: r.title}
	if r.describe != nil {
		meta.Provider, meta.Model = r.describe()
	}
	// Named by the time; a second session started the same millisecond,
	// here or by another debbie in the folder, gets a number after it.
	base := strings.Replace(now.Format("20060102-150405.000"), ".", "-", 1)
	for n := 1; ; n++ {
		name := base + ".jsonl"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.jsonl", base, n)
		}
		f, err := os.OpenFile(filepath.Join(r.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) && n < 1000 {
			continue
		}
		if err != nil {
			r.err = err
			return false
		}
		f.Close()
		r.path = filepath.Join(r.dir, name)
		break
	}
	if !r.write(meta) {
		r.path = ""
		return false
	}
	return true
}

// write appends one line to the file; it reports whether it could.
func (r *Recorder) write(v any) bool {
	line, err := json.Marshal(v)
	if err != nil {
		r.err = err
		return false
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err == nil {
		_, err = f.WriteString(r.redact(string(line)) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		r.err = err
		return false
	}
	return true
}

// Taint notes that web content joined the conversation.
func (r *Recorder) Taint() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tainted = true
	r.mark()
}

func (r *Recorder) mark() {
	if r.tainted && !r.marked && r.path != "" && r.err == nil {
		r.marked = r.write(record{Type: "tainted"})
	}
}

// Resume goes on writing the session in path, whose conversation is
// history.
func (r *Recorder) Resume(path string, history []provider.Message, tainted bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path, r.written, r.tainted, r.marked, r.err = path, len(history), tainted, tainted, nil
	r.title = ""
	if info, err := describe(path); err == nil {
		r.title = info.Title
	}
	r.err = endLine(path)
}

// Rename gives the conversation a title of the user's, which /resume
// lists it by. Before the first message it waits for the file.
func (r *Recorder) Rename(title string) error {
	if r == nil {
		return errors.New("conversations aren't saved in this session")
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return errors.New("give the conversation a name")
	}
	if n := []rune(title); len(n) > 100 {
		title = string(n[:99]) + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.title = title
	if r.path != "" && r.err == nil {
		r.write(record{Type: "title", Title: title})
	}
	return r.err
}

// CurrentTitle is the conversation's title: the user's, else its first
// message's, else "" before the first message.
func (r *Recorder) CurrentTitle() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.title
}

// endLine ends the file with a line break, which a crash in the middle of
// a line leaves out: the next line would join that one, and both be lost.
func endLine(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return err
	}
	if last[0] != '\n' {
		_, err = f.WriteString("\n")
	}
	return err
}

// Path is the file of the conversation, "" before its first message.
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

// Err is what stopped the recording, if anything did.
func (r *Recorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Dir is the folder of the workspace's sessions.
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// Title is how a conversation is listed: its first message's first line,
// without what was attached to it, cut short.
func Title(history []provider.Message) string {
	for _, m := range history {
		if m.Role != provider.RoleUser {
			continue
		}
		text := m.Text()
		if i := strings.Index(text, "<attached_file"); i >= 0 {
			text = text[:i]
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > 100 {
			text = string(r[:99]) + "…"
		}
		return text
	}
	return "(no message)"
}

// Info describes a saved session.
type Info struct {
	Meta
	Path     string
	Updated  time.Time
	Messages int // messages of the conversation as last written
}

// List describes the sessions in dir, the most recent first. A folder
// that doesn't exist yet has none.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := describe(path)
		if err != nil {
			continue // not one of ours, or broken
		}
		if st, err := e.Info(); err == nil {
			info.Updated = st.ModTime()
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b Info) int { return b.Updated.Compare(a.Updated) })
	return out, nil
}

// describe reads a session's first line and counts its messages.
func describe(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	info := Info{Path: path}
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &info.Meta) != nil || info.Type != "session" {
		return Info{}, errors.New("not a session file")
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte(`{"type":"message"`)):
			info.Messages++
		case bytes.HasPrefix(line, []byte(`{"type":"replace"`)):
			var rec record
			if json.Unmarshal(line, &rec) == nil {
				info.Messages = len(rec.Messages)
			}
		case bytes.HasPrefix(line, []byte(`{"type":"title"`)):
			var rec record
			if json.Unmarshal(line, &rec) == nil && rec.Title != "" {
				info.Title = rec.Title
			}
		}
	}
	return info, nil
}

// maxLine is the longest line a session file may hold: a conversation
// written whole after compaction is one line.
const maxLine = 256 << 20

// Load reads a session: its conversation, and whether web content was in
// it. A last line cut short, as a crash leaves it, is skipped.
func Load(path string) (history []provider.Message, tainted bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	var meta Meta
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &meta) != nil || meta.Type != "session" {
		return nil, false, fmt.Errorf("%s isn't a session file", filepath.Base(path))
	}
	if meta.Version > version {
		return nil, false, fmt.Errorf("%s was saved by a newer %s", filepath.Base(path), app.Name)
	}
	for sc.Scan() {
		var rec record
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Type {
		case "message":
			if rec.Message != nil {
				history = append(history, fromDisk(*rec.Message))
			}
		case "replace":
			history = history[:0:0]
			for _, d := range rec.Messages {
				history = append(history, fromDisk(d))
			}
		case "tainted":
			tainted = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return history, tainted, nil
}
