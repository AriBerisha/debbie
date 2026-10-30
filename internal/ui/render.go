package ui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"debbie/internal/agent"
	"debbie/internal/provider"
)

const borderWidth = 40

// renderer writes a streamed reply to the terminal: reasoning in gray, the
// answer as it arrives, and code blocks framed by a border that names the
// language.
type renderer struct {
	out      io.Writer
	st       style
	decorate bool

	mu  sync.Mutex // serializes writes by the spinner and the reply
	buf bytes.Buffer

	spinMu   sync.Mutex // guards the spinner: subagents' calls start and stop it from their goroutines
	spinStop chan struct{}
	spinDone chan struct{}

	// The reply in progress.
	phase   phase
	col0    bool   // output is at the start of a line
	holding bool   // pending holds the start of a line that may be a fence
	pending []byte // the held text
	fence   fence
	deltas  int       // stands in for the token count when usage is missing
	first   time.Time // when the first delta arrived
}

type phase int

const (
	phaseWaiting phase = iota
	phaseReasoning
	phaseText
)

// fence describes the open code block, if any.
type fence struct {
	open  bool
	char  byte
	count int
}

// begin starts a reply.
func (r *renderer) begin() {
	r.phase, r.col0, r.holding, r.fence = phaseWaiting, true, false, fence{}
	r.pending, r.deltas, r.first = r.pending[:0], 0, time.Time{}
	r.startSpinner("waiting for the model")
}

// event renders one streamed event.
func (r *renderer) event(ev provider.Event) {
	var text string
	var reasoning bool
	switch e := ev.(type) {
	case provider.TextDelta:
		text = e.Text
	case provider.ReasoningDelta:
		text, reasoning = e.Text, true
	default:
		return
	}
	r.stopSpinner()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.first.IsZero() {
		r.first = time.Now()
	}
	r.deltas++
	if reasoning {
		r.reasoning(text)
	} else {
		r.text(text)
	}
	r.flush()
}

// end finishes the reply: it writes out held text, closes a code block the
// model left open and moves to the start of a line.
func (r *renderer) end() {
	r.stopSpinner()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holding {
		r.releaseHeld()
	}
	if r.fence.open {
		r.newline()
		r.buf.WriteString(r.st.gray(bottomBorder()) + "\n")
		r.fence, r.col0 = fence{}, true
	}
	r.newline()
	r.flush()
}

// println writes a line outside a reply.
func (r *renderer) println(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	io.WriteString(r.out, s+"\n")
}

// printAbove prints a line where the spinner is, if one spins; the spinner
// draws again below it on its next tick.
func (r *renderer) printAbove(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.decorate {
		s = "\r\x1b[K" + s
	}
	io.WriteString(r.out, s+"\n")
}

func (r *renderer) newline() {
	if !r.col0 {
		r.buf.WriteByte('\n')
		r.col0 = true
	}
}

func (r *renderer) flush() {
	if r.buf.Len() > 0 {
		r.out.Write(r.buf.Bytes())
		r.buf.Reset()
	}
}

func (r *renderer) reasoning(s string) {
	if r.phase == phaseWaiting {
		r.buf.WriteString(r.st.gray("thinking: "))
	}
	r.phase = phaseReasoning
	r.buf.WriteString(r.st.gray(s))
	r.col0 = strings.HasSuffix(s, "\n")
}

func (r *renderer) text(s string) {
	if r.phase == phaseReasoning {
		r.newline()
		r.buf.WriteByte('\n') // a blank line between reasoning and answer
	}
	r.phase = phaseText
	if !r.decorate {
		r.buf.WriteString(s)
		if s != "" {
			r.col0 = s[len(s)-1] == '\n'
		}
		return
	}
	for i := 0; i < len(s); i++ {
		r.textByte(s[i])
	}
}

// textByte writes one byte of the answer. A line that starts like a code
// fence is held back until it is clear whether it is one.
func (r *renderer) textByte(c byte) {
	if r.holding {
		r.pending = append(r.pending, c)
		switch {
		case c == '\n':
			r.endHeldLine()
		case !maybeFence(r.pending):
			r.releaseHeld()
		}
		return
	}
	if r.col0 {
		if c == ' ' || c == '`' || c == '~' {
			r.holding = true
			r.pending = append(r.pending[:0], c)
			return
		}
		r.gutter()
	}
	r.buf.WriteByte(c)
	r.col0 = c == '\n'
}

// gutter starts a line, marking it as code inside a code block.
func (r *renderer) gutter() {
	if r.fence.open {
		r.buf.WriteString(r.st.gray("│ "))
	}
	r.col0 = false
}

// releaseHeld writes held text that turned out not to be a fence.
func (r *renderer) releaseHeld() {
	r.holding = false
	r.gutter()
	r.buf.Write(r.pending)
}

// endHeldLine handles a complete held line: a fence opens or closes a code
// block, anything else is written as it is.
func (r *renderer) endHeldLine() {
	r.holding = false
	if ch, n, info, ok := parseFence(r.pending); ok {
		switch {
		case !r.fence.open:
			r.fence = fence{open: true, char: ch, count: n}
			r.buf.WriteString(r.st.gray(topBorder(info)) + "\n")
			r.col0 = true
			return
		case ch == r.fence.char && n >= r.fence.count && info == "":
			r.fence = fence{}
			r.buf.WriteString(r.st.gray(bottomBorder()) + "\n")
			r.col0 = true
			return
		}
	}
	r.gutter()
	r.buf.Write(r.pending)
	r.col0 = true
}

// parseFence reports whether line, newline included, is a code fence: up
// to three spaces, three or more backticks or tildes, and an optional info
// string whose first word names the language.
func parseFence(line []byte) (ch byte, n int, info string, ok bool) {
	s := strings.TrimRight(string(line), "\r\n")
	rest := strings.TrimLeft(s, " ")
	if len(s)-len(rest) > 3 || rest == "" {
		return 0, 0, "", false
	}
	ch = rest[0]
	if ch != '`' && ch != '~' {
		return 0, 0, "", false
	}
	n = len(rest) - len(strings.TrimLeft(rest, string(ch)))
	info = strings.TrimSpace(rest[n:])
	if n < 3 || ch == '`' && strings.Contains(info, "`") {
		return 0, 0, "", false
	}
	return ch, n, info, true
}

// maybeFence reports whether the start of a line, without its newline,
// could still turn out to be a code fence.
func maybeFence(b []byte) bool {
	i := 0
	for i < len(b) && b[i] == ' ' {
		i++
	}
	if i > 3 {
		return false
	}
	if i == len(b) {
		return true
	}
	ch := b[i]
	if ch != '`' && ch != '~' {
		return false
	}
	j := i
	for j < len(b) && b[j] == ch {
		j++
	}
	switch {
	case j == len(b):
		return true
	case j-i < 3:
		return false
	}
	return ch != '`' || bytes.IndexByte(b[j:], '`') < 0
}

func topBorder(info string) string {
	lang, _, _ := strings.Cut(info, " ")
	if lang == "" {
		return "┌" + strings.Repeat("─", borderWidth-1)
	}
	return "┌─ " + lang + " " + strings.Repeat("─", max(3, borderWidth-4-utf8.RuneCountInString(lang)))
}

func bottomBorder() string {
	return "└" + strings.Repeat("─", borderWidth-1)
}

// startSpinner shows what is being waited for and for how long, until
// stopSpinner: the model until the first part of its reply arrives, or a
// tool until it is done.
func (r *renderer) startSpinner(label string) {
	if !r.decorate {
		return
	}
	r.spinMu.Lock()
	defer r.spinMu.Unlock()
	r.stopSpinnerLocked() // one at a time
	stop, done := make(chan struct{}), make(chan struct{})
	r.spinStop, r.spinDone = stop, done
	start := time.Now()
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		drawn := false
		for i := 0; ; i++ {
			select {
			case <-stop:
				if drawn {
					r.mu.Lock()
					io.WriteString(r.out, "\r\x1b[K")
					r.mu.Unlock()
				}
				return
			case <-tick.C:
			}
			if i < 2 {
				continue // no flicker for quick replies
			}
			r.mu.Lock()
			frame := fmt.Sprintf("%c %s  %.1fs", `|/-\`[i%4], label, time.Since(start).Seconds())
			io.WriteString(r.out, "\r"+r.st.gray(frame)+"\x1b[K")
			r.mu.Unlock()
			drawn = true
		}
	}()
}

func (r *renderer) stopSpinner() {
	r.spinMu.Lock()
	defer r.spinMu.Unlock()
	r.stopSpinnerLocked()
}

func (r *renderer) stopSpinnerLocked() {
	if r.spinStop == nil {
		return
	}
	close(r.spinStop)
	<-r.spinDone
	r.spinStop, r.spinDone = nil, nil
}

// StatusInfo is what the status line shows about the session.
type StatusInfo struct {
	Provider      string
	Model         string
	Mode          string
	ContextWindow int    // 0 when unknown
	Effort        string // the reasoning effort sent; empty for the server's default
}

// status describes the turn that just ended: context use, speed and time.
// The mode is the third part.
func (r *renderer) status(info StatusInfo, res agent.Result, elapsed time.Duration) []string {
	parts := []string{info.Provider, info.Model, info.Mode}
	if info.Effort != "" {
		parts = append(parts, "effort "+info.Effort)
	}
	output := r.deltas
	if u := res.Usage; u != nil {
		used := u.Input + u.Output
		if info.ContextWindow > 0 {
			parts = append(parts, fmt.Sprintf("ctx %d%% (%s/%s)", used*100/info.ContextWindow, kTokens(used), kTokens(info.ContextWindow)))
		} else {
			parts = append(parts, fmt.Sprintf("ctx %s tokens", kTokens(used)))
		}
		if u.Output > 0 {
			output = u.Output
		}
	}
	if !r.first.IsZero() && output > 1 {
		if gen := time.Since(r.first).Seconds(); gen >= minSpeedSeconds {
			parts = append(parts, fmt.Sprintf("%.1f tok/s", float64(output)/gen))
		}
	}
	return append(parts, fmt.Sprintf("%.1fs", elapsed.Seconds()))
}

// kTokens writes a token count briefly: 950, 8.1k, 262k.
func kTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
	return fmt.Sprintf("%dk", n/1000)
}

// minSpeedSeconds is the shortest generation whose speed the status line
// shows; a reply that arrives all at once has none worth showing.
const minSpeedSeconds = 0.25
