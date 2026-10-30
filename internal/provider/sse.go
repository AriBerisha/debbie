package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"time"
)

// maxEventSize bounds one line and one event of a stream, so a broken or
// hostile server can't make the reader buffer without limit.
const maxEventSize = 8 << 20

var errEventTooLarge = errors.New("server-sent event larger than 8 MiB")

// sseEvent is one dispatched server-sent event.
type sseEvent struct {
	Type string // "message" unless the stream named one
	Data string
}

// sseReader parses a text/event-stream as the WHATWG HTML standard
// specifies (§9.2.6, "Interpreting an event stream"): lines end in CRLF, LF
// or CR, a blank line dispatches the event, a line starting with a colon is
// a comment, and data lines are joined with LF. An event cut off by the end
// of the stream is discarded. The id and retry fields only matter for
// reconnecting, which this client never does, so they are ignored.
type sseReader struct {
	r       *bufio.Reader
	line    []byte
	afterCR bool // the last line ended in CR, so a following LF belongs to it
	typ     string
	data    []byte
	hasData bool
}

func newSSEReader(r io.Reader) *sseReader {
	br := bufio.NewReader(r)
	if bom, err := br.Peek(3); err == nil && bytes.Equal(bom, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	return &sseReader{r: br}
}

// Next returns the next event. At the end of the stream it returns io.EOF.
func (s *sseReader) Next() (sseEvent, error) {
	for {
		line, err := s.readLine()
		if err != nil {
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if !s.hasData {
				s.typ = ""
				continue
			}
			ev := sseEvent{Type: s.typ, Data: string(s.data)}
			if ev.Type == "" {
				ev.Type = "message"
			}
			s.typ, s.data, s.hasData = "", s.data[:0], false
			return ev, nil
		}
		if line[0] == ':' {
			continue // comment; llama-server sends these as keep-alive pings
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			s.typ = string(value)
		case "data":
			if len(s.data)+len(value)+1 > maxEventSize {
				return sseEvent{}, errEventTooLarge
			}
			if s.hasData {
				s.data = append(s.data, '\n')
			}
			s.data = append(s.data, value...)
			s.hasData = true
		}
	}
}

// readLine returns the next line without its terminator. A last line
// without a terminator is incomplete and is not returned.
func (s *sseReader) readLine() ([]byte, error) {
	s.line = s.line[:0]
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch {
		case b == '\n' && s.afterCR:
			s.afterCR = false // the LF of a CRLF pair
			continue
		case b == '\n':
			return s.line, nil
		case b == '\r':
			s.afterCR = true
			return s.line, nil
		}
		s.afterCR = false
		if len(s.line) >= maxEventSize {
			return nil, errEventTooLarge
		}
		s.line = append(s.line, b)
	}
}

// idleReader calls onIdle when no data arrives for d. The timer starts when
// the reader is created and restarts whenever a read returns data.
type idleReader struct {
	r     io.Reader
	d     time.Duration
	timer *time.Timer
}

func newIdleReader(r io.Reader, d time.Duration, onIdle func()) *idleReader {
	return &idleReader{r: r, d: d, timer: time.AfterFunc(d, onIdle)}
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.timer.Reset(ir.d)
	}
	return n, err
}

func (ir *idleReader) stop() { ir.timer.Stop() }
