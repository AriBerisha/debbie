package ui

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"debbie/internal/config"
	"debbie/internal/setup"
)

// Console is the terminal the program talks through: it reads lines,
// writes redacted output and turns Ctrl+C into interrupts. The setup
// wizard and the chat share one, so no typed input is lost between them.
type Console struct {
	term        Terminal
	out         *config.Writer
	st          style
	r           *renderer
	input       *lineReader
	interrupts  chan struct{} // a single Ctrl+C, for whoever is waiting
	quit        chan struct{} // closed on a second Ctrl+C within a second
	quitOnce    sync.Once
	lastSignal  atomic.Int64 // time of the last Ctrl+C, in Unix nanoseconds
	stopSignals func()
	cmdDone     <-chan struct{} // closed when the running command is cancelled
	// openKeys switches the terminal to single key presses, for the
	// slider; nil when the input isn't a terminal.
	openKeys func(inputOptions) (keyboard, error)
}

// NewConsole starts reading in and watching for Ctrl+C. Everything written
// to out passes through redactor first.
func NewConsole(term Terminal, in io.Reader, out io.Writer, redactor *config.Redactor) *Console {
	w := redactor.Writer(consoleOutput(out))
	c := &Console{
		term:       term,
		out:        w,
		st:         style{color: term.Color},
		r:          &renderer{out: w, st: style{color: term.Color}, decorate: term.Decorate},
		interrupts: make(chan struct{}),
		quit:       make(chan struct{}),
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	c.stopSignals = func() { signal.Stop(sigs) }
	go c.watch(sigs)

	var interrupted func() bool
	if term.Interactive && runtime.GOOS == "windows" {
		interrupted = c.recentlyInterrupted
	}
	c.input = newLineReader(in, interrupted)
	if term.in != nil {
		c.openKeys = func(o inputOptions) (keyboard, error) { return openKeyboard(term.in, o) }
	}
	return c
}

// Close stops watching for Ctrl+C and writes out whatever is held back.
func (c *Console) Close() {
	c.stopSignals()
	c.out.Flush()
}

// watch turns interrupt signals into single interrupts and the quit signal.
func (c *Console) watch(sigs <-chan os.Signal) {
	var last time.Time
	for range sigs {
		now := time.Now()
		c.lastSignal.Store(now.UnixNano())
		if now.Sub(last) < time.Second {
			c.quitOnce.Do(func() { close(c.quit) })
			// Should something ignore the cancellation, don't hang.
			time.AfterFunc(3*time.Second, func() { os.Exit(ExitInterrupted) })
			return
		}
		last = now
		select {
		case c.interrupts <- struct{}{}:
		default:
		}
	}
}

// recentlyInterrupted reports whether Ctrl+C was pressed just now. The
// signal may arrive a moment after the console read it ended, so it waits
// briefly.
func (c *Console) recentlyInterrupted() bool {
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if t := c.lastSignal.Load(); t != 0 && time.Since(time.Unix(0, t)) < time.Second {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Quitting reports whether the user pressed Ctrl+C twice to leave.
func (c *Console) Quitting() bool {
	select {
	case <-c.quit:
		return true
	default:
		return false
	}
}

var (
	errInterrupted = errors.New("interrupted") // Ctrl+C once
	errQuit        = errors.New("quit")        // Ctrl+C twice
)

// readLine waits for a line of input outside a turn: an answer or a
// confirmation. It returns errInterrupted, errQuit or io.EOF instead when
// the user presses Ctrl+C or the input ends.
func (c *Console) readLine(echo bool) (string, error) {
	c.out.Flush()
	select {
	case <-c.quit:
		return "", errQuit
	case <-c.interrupts:
		c.input.interrupted()
		c.r.println("")
		return "", errInterrupted
	case <-c.cmdDone:
		c.input.interrupted()
		c.r.println("")
		return "", errInterrupted
	case line, ok := <-c.input.next():
		c.input.received()
		if !ok {
			return "", io.EOF
		}
		if echo {
			c.echo(line)
		}
		return line, nil
	}
}

// echo shows an answer read from input that isn't a terminal, which
// doesn't show it itself, so the output reads as a transcript.
func (c *Console) echo(line string) {
	if !c.term.Interactive {
		c.r.println(line)
	}
}

// Say shows a line; it is part of setup.Prompter, whose conventions mark
// passed checks, problems and the suggested option. The marks are drawn
// with symbols the terminal's font has.
func (c *Console) Say(text string) {
	switch {
	case strings.HasPrefix(text, "  ✓ "):
		text = "  " + c.st.green(c.term.sym().check) + text[len("  ✓"):]
	case strings.HasPrefix(text, "  ! "):
		text = c.st.yellow(text)
	case strings.HasPrefix(text, " ❯ "):
		text = c.st.cyan(" " + c.term.sym().pointer + text[len(" ❯"):])
	case strings.HasSuffix(text, "?") && !strings.HasPrefix(text, " "):
		text = c.st.bold(text) // a question
	}
	c.r.println(text)
}

// question styles a prompt: the question bold, a default in brackets gray.
func (c *Console) question(prompt string) string {
	body := strings.TrimRight(prompt, " ")
	tail := prompt[len(body):]
	if i := strings.LastIndex(body, " ["); i >= 0 {
		if j := strings.LastIndex(body, "]"); j > i {
			return c.st.bold(body[:i]) + " " + c.st.gray(body[i+1:j+1]) + body[j+1:] + tail
		}
	}
	return c.st.bold(body) + tail
}

// Ask shows prompt and reads the answer. Ctrl+C or the end of the input
// gives setup.ErrCancelled.
func (c *Console) Ask(prompt string) (string, error) {
	io.WriteString(c.out, c.question(prompt))
	line, err := c.readLine(true)
	if err != nil {
		if errors.Is(err, io.EOF) {
			c.r.println("")
		}
		return "", setup.ErrCancelled
	}
	return strings.TrimSpace(line), nil
}

// Secret reads an API key. In a terminal it takes single key presses, so
// the key never shows; otherwise the terminal shows it while it is typed,
// so the line is erased afterwards. The key is never echoed.
func (c *Console) Secret(prompt string) (string, error) {
	if key, ok, err := c.secret(prompt); ok {
		return key, err
	}
	io.WriteString(c.out, prompt)
	line, err := c.readLine(false)
	if err != nil {
		return "", setup.ErrCancelled
	}
	switch {
	case c.term.Interactive && c.term.Decorate:
		io.WriteString(c.out, "\x1b[1A\r\x1b[2K"+prompt+c.st.gray("(hidden)")+"\n")
	case !c.term.Interactive:
		c.r.println(c.st.gray("(hidden)"))
	}
	return strings.TrimSpace(line), nil
}
