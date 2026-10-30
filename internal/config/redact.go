package config

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
)

// Redacted replaces every secret in output.
const Redacted = "[redacted]"

// MinSecretLength is the shortest value a Redactor scrubs. Shorter values
// are too common in ordinary text to scrub meaningfully.
const MinSecretLength = 4

// Redactor replaces known secret values with [redacted]. Error messages, log
// lines and tool output pass through one before anyone sees them. A nil
// *Redactor redacts nothing. It is safe for concurrent use.
type Redactor struct {
	mu    sync.Mutex               // serializes Add
	forms atomic.Pointer[[][]byte] // every form of every secret, longest first
}

// NewRedactor returns a Redactor for the given secret values. Besides each
// value as is, it catches the value JSON-escaped and URL-encoded, the forms
// in which keys usually show up in error messages and logs.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add makes the redactor scrub more secrets, such as a key the user has
// just entered.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	forms := slices.Clone(r.current())
	seen := map[string]bool{}
	for _, f := range forms {
		seen[string(f)] = true
	}
	for _, s := range secrets {
		if len(s) < MinSecretLength {
			continue
		}
		for _, f := range secretForms(s) {
			if !seen[f] {
				seen[f] = true
				forms = append(forms, []byte(f))
			}
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	slices.SortStableFunc(forms, func(a, b []byte) int { return len(b) - len(a) })
	r.forms.Store(&forms)
}

// current returns the forms to look for.
func (r *Redactor) current() [][]byte {
	if r == nil {
		return nil
	}
	if f := r.forms.Load(); f != nil {
		return *f
	}
	return nil
}

func secretForms(s string) []string {
	forms := []string{s, url.QueryEscape(s), url.PathEscape(s)}
	if b, err := json.Marshal(s); err == nil {
		forms = append(forms, string(b[1:len(b)-1])) // escapes <, > and &
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err == nil {
		quoted := bytes.TrimSpace(buf.Bytes())
		forms = append(forms, string(quoted[1:len(quoted)-1]))
	}
	return forms
}

// String returns s with every secret replaced.
func (r *Redactor) String(s string) string {
	if len(r.current()) == 0 {
		return s
	}
	out, _ := r.redact([]byte(s), true)
	return string(out)
}

// Error returns err with its message redacted. errors.Is and errors.As still
// see the original error chain.
func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if red := r.String(msg); red != msg {
		return &redactedError{msg: red, err: err}
	}
	return err
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redact returns buf with secrets replaced. Unless final, it stops where a
// secret may begin but buf ends before it could be matched, and consumed
// tells how far it got; the rest must be passed again with more data.
func (r *Redactor) redact(buf []byte, final bool) (out []byte, consumed int) {
	forms := r.current()
	if len(forms) == 0 {
		return buf, len(buf)
	}
	plain := 0 // start of the text not yet copied to out
	i := 0
	for i < len(buf) {
		if !final && partialAt(forms, buf[i:]) {
			break
		}
		if n := matchAt(forms, buf[i:]); n > 0 {
			out = append(out, buf[plain:i]...)
			out = append(out, Redacted...)
			i += n
			plain = i
			continue
		}
		i++
	}
	return append(out, buf[plain:i]...), i
}

// matchAt returns the length of the longest secret form at the start of b,
// or 0.
func matchAt(forms [][]byte, b []byte) int {
	for _, f := range forms {
		if bytes.HasPrefix(b, f) {
			return len(f)
		}
	}
	return 0
}

// partialAt reports whether b, which runs to the end of the data seen so
// far, is the beginning of a secret form that more data could complete.
func partialAt(forms [][]byte, b []byte) bool {
	for _, f := range forms {
		if len(b) < len(f) && bytes.HasPrefix(f, b) {
			return true
		}
	}
	return false
}

// Writer returns a writer that redacts everything written through it before
// passing it on to w. Text that might be the start of a secret is held back
// until a later write settles it; Flush writes out what is still held.
func (r *Redactor) Writer(w io.Writer) *Writer {
	return &Writer{r: r, w: w}
}

// Writer is a redacting io.Writer, safe for concurrent use.
type Writer struct {
	r       *Redactor
	w       io.Writer
	mu      sync.Mutex
	pending []byte
}

func (rw *Writer) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	buf := append(rw.pending, p...)
	out, n := rw.r.redact(buf, false)
	rw.pending = append(rw.pending[:0], buf[n:]...)
	if len(out) > 0 {
		if _, err := rw.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes out any held-back text.
func (rw *Writer) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) == 0 {
		return nil
	}
	out, _ := rw.r.redact(rw.pending, true)
	rw.pending = rw.pending[:0]
	_, err := rw.w.Write(out)
	return err
}

// WriteComplete writes what is held back and p, redacted, in one write: for
// output complete in itself, such as a frame of the fullscreen UI, whose
// end mustn't wait for a later write that may not come.
func (rw *Writer) WriteComplete(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	out, _ := rw.r.redact(append(rw.pending, p...), true)
	rw.pending = rw.pending[:0]
	if _, err := rw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Pieces redacts text that arrives in pieces, such as a streamed reply, for
// a caller that keeps the text instead of writing it. Like a Writer, it
// holds back the end of what came so far while that might be the start of
// a secret, so no part of one is ever shown.
type Pieces struct {
	r       *Redactor
	pending []byte
}

// Pieces starts redacting one stream of text.
func (r *Redactor) Pieces() *Pieces { return &Pieces{r: r} }

// Add takes the next piece and returns the redacted text now settled.
func (p *Pieces) Add(s string) string {
	buf := append(p.pending, s...)
	out, n := p.r.redact(buf, false)
	settled := string(out)
	p.pending = append(p.pending[:0], buf[n:]...)
	return settled
}

// End returns what is still held back, redacted.
func (p *Pieces) End() string {
	out, _ := p.r.redact(p.pending, true)
	p.pending = p.pending[:0]
	return string(out)
}
