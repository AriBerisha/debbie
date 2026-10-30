package ui

import (
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// consoleWriter writes UTF-8 to a Windows console with one call to
// WriteConsoleW for each write, however long. os.File cuts writes into
// pieces of 16,000 characters, a limit of consoles before Windows 8, and
// the console can paint between two pieces: a big window's frame would
// show half drawn. A character cut between two writes waits for the rest.
type consoleWriter struct {
	write func(units []uint16) (int, error) // WriteConsoleW: how many it took

	mu      sync.Mutex
	rest    []byte // the start of a character cut off at the end of a write
	chunked bool   // the console refused a long write: send pieces from now on
}

// consoleChunk is the size of the pieces for a console that refuses long
// writes, as os.File sends them.
const consoleChunk = 16000

func (w *consoleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var units []uint16
	units, w.rest = utf16Units(w.rest, p)
	for len(units) > 0 {
		m := len(units)
		if w.chunked && m > consoleChunk {
			m = consoleChunk
			if utf16.IsSurrogate(rune(units[m-1])) && units[m-1] < 0xdc00 {
				m-- // keep a surrogate pair together
			}
		}
		n, err := w.write(units[:m])
		if err != nil {
			if !w.chunked && m > consoleChunk {
				w.chunked = true
				continue
			}
			return 0, err
		}
		units = units[n:]
	}
	return len(p), nil
}

// utf16Units converts rest and p, UTF-8, to UTF-16, returning the start of
// a character cut off at the end to send with the next write.
func utf16Units(rest, p []byte) (units []uint16, cut []byte) {
	b := append(rest, p...)
	end := len(b)
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				end = i
			}
			break
		}
	}
	for _, r := range string(b[:end]) {
		units = utf16.AppendRune(units, r)
	}
	return units, append([]byte(nil), b[end:]...)
}
