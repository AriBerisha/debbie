package ui

import (
	"bufio"
	"io"
	"strings"
)

// lineReader reads lines in the background, so the chat can react to
// Ctrl+C while a read is pending. It reads a line only when asked, once
// the prompt is drawn: the Windows console remembers where a read started
// and redraws the line being typed from there, for editing and history.
type lineReader struct {
	ask     chan struct{}
	results chan string // closed when the input ends
	pending bool        // a line was asked for and hasn't been received
	// interruptEndsRead is true where Ctrl+C ends a pending read, in the
	// Windows console; elsewhere the read carries on.
	interruptEndsRead bool
}

// newLineReader starts reading r. interrupted, if not nil, is asked whether
// an end of input was really a Ctrl+C: on Windows, Ctrl+C ends a pending
// console read as if the input had ended.
func newLineReader(r io.Reader, interrupted func() bool) *lineReader {
	lr := &lineReader{ask: make(chan struct{}, 1), results: make(chan string), interruptEndsRead: interrupted != nil}
	go func() {
		defer close(lr.results)
		br := bufio.NewReader(r)
		for range lr.ask {
			s, err := br.ReadString('\n')
			if s != "" {
				lr.results <- strings.TrimRight(s, "\r\n")
				continue
			}
			if err == io.EOF && interrupted != nil && interrupted() {
				continue // the reader asks again once it has redrawn its prompt
			}
			if err != nil {
				return
			}
		}
	}()
	return lr
}

// next asks for a line, unless one is already on its way, and returns the
// channel it arrives on. Whoever receives from it calls received.
func (lr *lineReader) next() <-chan string {
	if !lr.pending {
		lr.pending = true
		select {
		case lr.ask <- struct{}{}:
		default: // already asked
		}
	}
	return lr.results
}

// received notes that the line asked for has arrived.
func (lr *lineReader) received() { lr.pending = false }

// interrupted notes a Ctrl+C, which ends a pending read in the Windows
// console.
func (lr *lineReader) interrupted() {
	if lr.interruptEndsRead {
		lr.pending = false
	}
}

// assembler joins input lines into messages. A line ending in a backslash
// continues on the next line, and the lines between two lines of """ form
// one message as they are.
type assembler struct {
	lines []string
	block bool
}

// add takes the next line and returns the message once it is complete.
func (a *assembler) add(line string) (msg string, done bool) {
	isQuotes := strings.TrimSpace(line) == `"""`
	switch {
	case a.block && isQuotes:
		a.block = false
		return a.take(), true
	case a.block:
		a.lines = append(a.lines, line)
		return "", false
	case isQuotes && len(a.lines) == 0:
		a.block = true
		return "", false
	case strings.HasSuffix(line, `\`):
		a.lines = append(a.lines, strings.TrimSuffix(line, `\`))
		return "", false
	}
	a.lines = append(a.lines, line)
	return a.take(), true
}

// continuing reports whether a message has been started but not finished.
func (a *assembler) continuing() bool { return a.block || len(a.lines) > 0 }

func (a *assembler) reset() {
	a.lines, a.block = a.lines[:0], false
}

func (a *assembler) take() string {
	msg := strings.Join(a.lines, "\n")
	a.lines = a.lines[:0]
	return msg
}
