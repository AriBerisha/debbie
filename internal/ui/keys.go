package ui

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// key is a key press the interactive parts of the UI react to.
type key int

const (
	keyRune key = iota // a character, in keyPress.r
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyPgUp
	keyPgDn
	keyEnter
	keyEsc
	keyTab
	keyBackTab // Shift+Tab
	keyBackspace
	keyDelete
	keyInsert
	keyF1 // F1 to F12 follow in order
)

// modifiers are the Shift, Alt and Ctrl keys held with a key or click.
type modifiers uint8

const (
	modShift modifiers = 1 << iota
	modAlt
	modCtrl
)

type keyPress struct {
	k   key
	r   rune // for keyRune; Ctrl+A is 'a' with modCtrl
	mod modifiers
}

// ctrl reports whether p is Ctrl and the letter r.
func (p keyPress) ctrl(r rune) bool { return p.k == keyRune && p.mod&modCtrl != 0 && p.r == r }

type mouseAction uint8

const (
	mousePress mouseAction = iota
	mouseRelease
	mouseDrag
	mouseWheelUp
	mouseWheelDown
)

type mouseEvent struct {
	x, y   int // the cell, from 0
	action mouseAction
	button int // 0 left, 1 middle, 2 right
	mod    modifiers
}

type eventKind uint8

const (
	evKey eventKind = iota
	evMouse
	evPaste  // text pasted at once, newlines included
	evResize // the window changed size
)

// event is an input event: a key, a mouse action, a paste or a resize.
type event struct {
	kind  eventKind
	key   keyPress
	mouse mouseEvent
	text  string // for evPaste
}

func keyEvent(p keyPress) event { return event{kind: evKey, key: p} }

// keyboard reads input in raw mode, without waiting for Enter and without
// echo, until closed.
type keyboard interface {
	events() <-chan event
	// close stops reading and puts the terminal back in line mode.
	close()
}

// inputOptions choose what a keyboard reads besides keys.
type inputOptions struct {
	mouse bool // mouse clicks, drags and the wheel
	// ctrlC makes Ctrl+C a key press; otherwise it stays the interrupt
	// signal.
	ctrlC bool
}

// csiKeys maps the final byte of a CSI sequence to its key.
var csiKeys = map[byte]key{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft, 'H': keyHome, 'F': keyEnd, 'Z': keyBackTab,
	'P': keyF1, 'Q': keyF1 + 1, 'R': keyF1 + 2, 'S': keyF1 + 3}

// tildeKeys maps the number of a CSI ... ~ sequence to its key.
var tildeKeys = map[int]key{1: keyHome, 2: keyInsert, 3: keyDelete, 4: keyEnd, 5: keyPgUp, 6: keyPgDn, 7: keyHome, 8: keyEnd,
	11: keyF1, 12: keyF1 + 1, 13: keyF1 + 2, 14: keyF1 + 3, 15: keyF1 + 4, 17: keyF1 + 5, 18: keyF1 + 6, 19: keyF1 + 7,
	20: keyF1 + 8, 21: keyF1 + 9, 23: keyF1 + 10, 24: keyF1 + 11}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// parseInput turns bytes read from a terminal in raw mode into events:
// keys, SGR mouse reports and bracketed pastes. It returns the bytes it
// couldn't use yet: the start of an escape sequence, a character or a
// paste that may continue in the next read. When no more bytes follow, a
// lone ESC is the Esc key; final says so.
func parseInput(b []byte, final bool) (events []event, rest []byte) {
	for len(b) > 0 {
		if strings.HasPrefix(string(b), pasteStart) {
			end := strings.Index(string(b), pasteEnd)
			if end < 0 {
				if final {
					return append(events, event{kind: evPaste, text: string(b[len(pasteStart):])}), nil
				}
				return events, b
			}
			events = append(events, event{kind: evPaste, text: string(b[len(pasteStart):end])})
			b = b[end+len(pasteEnd):]
			continue
		}
		c := b[0]
		switch {
		case c == 0x1b:
			if len(b) == 1 {
				if final {
					return append(events, keyEvent(keyPress{k: keyEsc})), nil
				}
				return events, b
			}
			n, ev, ok, complete := escapeSequence(b)
			if !complete {
				if final {
					return append(events, keyEvent(keyPress{k: keyEsc})), nil
				}
				return events, b
			}
			if ok {
				events = append(events, ev)
			}
			b = b[n:]
		case c == '\r':
			events = append(events, keyEvent(keyPress{k: keyEnter}))
			b = b[1:]
		case c == '\n':
			events = append(events, keyEvent(keyPress{k: keyRune, r: 'j', mod: modCtrl}))
			b = b[1:]
		case c == '\t':
			events = append(events, keyEvent(keyPress{k: keyTab}))
			b = b[1:]
		case c == 0x7f || c == 0x08:
			events = append(events, keyEvent(keyPress{k: keyBackspace}))
			b = b[1:]
		case c >= 1 && c <= 26:
			events = append(events, keyEvent(keyPress{k: keyRune, r: rune('a' + c - 1), mod: modCtrl}))
			b = b[1:]
		case c < 0x20:
			b = b[1:] // other control bytes mean nothing here
		default:
			if !utf8.FullRune(b) && !final {
				return events, b
			}
			r, n := utf8.DecodeRune(b)
			events = append(events, keyEvent(keyPress{k: keyRune, r: r}))
			b = b[n:]
		}
	}
	return events, nil
}

// escapeSequence reads the escape sequence at the start of b: CSI (ESC [
// parameters final byte), SS3 (ESC O letter), or ESC with another key,
// which is that key with Alt. It returns the sequence's length, its event
// and ok false for one the UI ignores, and whether b holds all of it.
func escapeSequence(b []byte) (n int, ev event, ok, complete bool) {
	switch b[1] {
	case '[':
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				ev, ok := csiEvent(string(b[2:i]), b[i])
				return i + 1, ev, ok, true
			}
		}
		return 0, event{}, false, false
	case 'O':
		if len(b) < 3 {
			return 0, event{}, false, false
		}
		k, ok := csiKeys[b[2]]
		return 3, keyEvent(keyPress{k: k}), ok, true
	}
	// Alt with a character or control key.
	if !utf8.FullRune(b[1:]) {
		return 0, event{}, false, false
	}
	_, size := utf8.DecodeRune(b[1:])
	evs, _ := parseInput(b[1:1+size], true)
	if len(evs) != 1 || evs[0].kind != evKey {
		return 1 + size, event{}, false, true
	}
	ev = evs[0]
	ev.key.mod |= modAlt
	return 1 + size, ev, true, true
}

// csiEvent decodes a CSI sequence's parameters and final byte: a key with
// modifiers ("1;5C" is Ctrl+Right) or an SGR mouse report ("<0;10;5M").
func csiEvent(params string, final byte) (event, bool) {
	if strings.HasPrefix(params, "<") && (final == 'M' || final == 'm') {
		return sgrMouse(params[1:], final == 'm')
	}
	nums := strings.Split(params, ";")
	num := func(i int) int {
		if i >= len(nums) {
			return 0
		}
		n, _ := strconv.Atoi(nums[i])
		return n
	}
	var mod modifiers
	if m := num(1); m > 1 {
		mod = modifiers(m - 1)
	}
	if final == '~' {
		k, ok := tildeKeys[num(0)]
		return keyEvent(keyPress{k: k, mod: mod}), ok
	}
	k, ok := csiKeys[final]
	if k == keyBackTab {
		mod |= modShift
	}
	return keyEvent(keyPress{k: k, mod: mod}), ok
}

// sgrMouse decodes the parameters of an SGR mouse report: button and
// modifier bits, then the column and row from 1.
func sgrMouse(params string, release bool) (event, bool) {
	parts := strings.Split(params, ";")
	if len(parts) != 3 {
		return event{}, false
	}
	b, err1 := strconv.Atoi(parts[0])
	x, err2 := strconv.Atoi(parts[1])
	y, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return event{}, false
	}
	m := mouseEvent{x: x - 1, y: y - 1, button: b & 3}
	if b&4 != 0 {
		m.mod |= modShift
	}
	if b&8 != 0 {
		m.mod |= modAlt
	}
	if b&16 != 0 {
		m.mod |= modCtrl
	}
	switch {
	case b&64 != 0:
		m.action, m.button = mouseWheelUp, 0
		if b&1 != 0 {
			m.action = mouseWheelDown
		}
	case b&32 != 0:
		if b&3 == 3 {
			return event{}, false // movement with no button held
		}
		m.action = mouseDrag
	case release:
		m.action = mouseRelease
	default:
		m.action = mousePress
	}
	return event{kind: evMouse, mouse: m}, true
}

// burstEvents turns characters that arrived together into events. A burst
// is a paste, whose line breaks stay in the text, unless its only line
// break is the last character: then it is typing that arrived at once,
// while the agent was busy, and the Enter that ends it sends the message.
func burstEvents(text string) []event {
	body, enter := strings.CutSuffix(text, "\n")
	if !enter || strings.Contains(body, "\n") {
		return []event{{kind: evPaste, text: text}}
	}
	var out []event
	if body != "" {
		out = append(out, event{kind: evPaste, text: body})
	}
	return append(out, keyEvent(keyPress{k: keyEnter}))
}
