//go:build windows

package ui

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var procReadConsoleInputW = kernel32.NewProc("ReadConsoleInputW")

// inputRecord is an INPUT_RECORD: an event type and its record.
type inputRecord struct {
	eventType uint16
	_         uint16
	data      [16]byte
}

const (
	keyEventType    = 0x1
	mouseEventType  = 0x2
	resizeEventType = 0x4
)

// keyRecord is a KEY_EVENT_RECORD.
type keyRecord struct {
	keyDown         int32
	repeatCount     uint16
	virtualKeyCode  uint16
	virtualScanCode uint16
	unicodeChar     uint16
	controlKeyState uint32
}

// mouseRecord is a MOUSE_EVENT_RECORD.
type mouseRecord struct {
	x, y            int16
	buttonState     uint32
	controlKeyState uint32
	eventFlags      uint32
}

// winKeys maps the virtual-key codes of the keys that aren't characters.
var winKeys = map[uint16]key{
	0x08: keyBackspace, 0x09: keyTab, 0x0d: keyEnter, 0x1b: keyEsc,
	0x21: keyPgUp, 0x22: keyPgDn, 0x23: keyEnd, 0x24: keyHome,
	0x25: keyLeft, 0x26: keyUp, 0x27: keyRight, 0x28: keyDown,
	0x2d: keyInsert, 0x2e: keyDelete,
	0x70: keyF1, 0x71: keyF1 + 1, 0x72: keyF1 + 2, 0x73: keyF1 + 3, 0x74: keyF1 + 4, 0x75: keyF1 + 5,
	0x76: keyF1 + 6, 0x77: keyF1 + 7, 0x78: keyF1 + 8, 0x79: keyF1 + 9, 0x7a: keyF1 + 10, 0x7b: keyF1 + 11,
}

type winInput struct {
	h       syscall.Handle
	mode    uint32 // the console's input mode before
	ch      chan event
	done    chan struct{}
	exited  chan struct{}
	buttons uint32 // the mouse buttons held, as last reported
	high    uint16 // the first half of a surrogate pair
}

// openKeyboard switches the console's input out of line mode and reads its
// input records: keys, and with opts.mouse the mouse (Quick Edit, which
// would select text instead, goes off) and window size changes. Ctrl+C
// stays the interrupt signal unless opts.ctrlC.
func openKeyboard(in *os.File, opts inputOptions) (keyboard, error) {
	h := syscall.Handle(in.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	const (
		processedInput = 0x1
		lineInput      = 0x2
		echoInput      = 0x4
		windowInput    = 0x8
		mouseInput     = 0x10
		quickEdit      = 0x40
		extendedFlags  = 0x80
		vtInput        = 0x200
	)
	raw := mode&^(lineInput|echoInput|vtInput) | processedInput
	if opts.ctrlC {
		raw &^= processedInput
	}
	if opts.mouse {
		raw = raw&^quickEdit | mouseInput | windowInput | extendedFlags
	}
	if r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(raw)); r == 0 {
		return nil, err
	}
	kb := &winInput{h: h, mode: mode, ch: make(chan event, 64), done: make(chan struct{}), exited: make(chan struct{})}
	go kb.run()
	return kb, nil
}

func (kb *winInput) events() <-chan event { return kb.ch }

// run passes on input events. It waits for input a tenth of a second at a
// time, so it notices close without another event.
func (kb *winInput) run() {
	defer close(kb.exited)
	buf := make([]inputRecord, 128)
	for {
		select {
		case <-kb.done:
			return
		default:
		}
		if ev, err := syscall.WaitForSingleObject(kb.h, 100); err != nil {
			return
		} else if ev != syscall.WAIT_OBJECT_0 {
			continue
		}
		recs, err := gather(kb.read, kb.wait, buf)
		for _, ev := range kb.translate(recs) {
			select {
			case kb.ch <- ev:
			case <-kb.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (kb *winInput) read(buf []inputRecord) (int, error) {
	var n uint32
	if r, _, err := procReadConsoleInputW.Call(uintptr(kb.h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n))); r == 0 {
		return 0, err
	}
	return int(n), nil
}

// wait reports whether input comes within ms milliseconds.
func (kb *winInput) wait(ms uint32) bool {
	ev, err := syscall.WaitForSingleObject(kb.h, ms)
	return err == nil && ev == syscall.WAIT_OBJECT_0
}

// pasteGap is how long a paste may pause between its pieces. A paste is
// more records than one read takes, two for each character, and a terminal
// writes it to a pseudo-console in pieces: VS Code 50 characters every 5
// ms, which Windows' timers can stretch to 16.
const pasteGap = 50

// gather reads input records: what one read takes and, when they hold a
// paste, the rest of it, while more characters come within pasteGap. One
// paste is then one event, which the UI can show as a chip, and none of its
// newlines sends the message half pasted.
func gather(read func([]inputRecord) (int, error), wait func(ms uint32) bool, buf []inputRecord) ([]inputRecord, error) {
	n, err := read(buf)
	if err != nil {
		return nil, err
	}
	recs := append([]inputRecord(nil), buf[:n]...)
	if countChars(recs) < 2 {
		return recs, nil
	}
	for wait(pasteGap) {
		n, err := read(buf)
		if err != nil {
			return recs, err
		}
		recs = append(recs, buf[:n]...)
		if countChars(buf[:n]) == 0 { // the mouse, say: the paste is over
			break
		}
	}
	return recs, nil
}

// countChars counts the characters typed or pasted in records: the key
// presses that are characters, Enter and Tab.
func countChars(recs []inputRecord) int {
	chars := 0
	for i := range recs {
		if recs[i].eventType == keyEventType {
			k := (*keyRecord)(unsafe.Pointer(&recs[i].data[0]))
			if k.keyDown != 0 && (k.unicodeChar >= 0x20 || k.virtualKeyCode == 0x0d || k.virtualKeyCode == 0x09) {
				chars++
			}
		}
	}
	return chars
}

// translate turns input records into events. Pasted text arrives as a
// burst of key records, which a person typing can't produce: two or more
// characters read at once become one paste, so a newline in it doesn't
// send the message.
func (kb *winInput) translate(recs []inputRecord) []event {
	var events []event
	var paste strings.Builder
	chars := countChars(recs)
	flush := func() {
		if paste.Len() > 0 {
			events = append(events, burstEvents(paste.String())...)
			paste.Reset()
		}
	}
	for i := range recs {
		switch recs[i].eventType {
		case keyEventType:
			k := (*keyRecord)(unsafe.Pointer(&recs[i].data[0]))
			if k.keyDown == 0 {
				continue
			}
			p, ok := kb.key(k)
			if !ok {
				continue
			}
			if chars >= 2 && k.controlKeyState&0x0c == 0 { // no Ctrl: part of the paste
				switch {
				case p.k == keyRune:
					paste.WriteRune(p.r)
					continue
				case p.k == keyEnter:
					paste.WriteByte('\n')
					continue
				case p.k == keyTab:
					paste.WriteByte('\t')
					continue
				}
			}
			flush()
			for range max(1, int(k.repeatCount)) {
				events = append(events, keyEvent(p))
			}
		case mouseEventType:
			flush()
			if ev, ok := kb.mouse((*mouseRecord)(unsafe.Pointer(&recs[i].data[0]))); ok {
				events = append(events, ev)
			}
		case resizeEventType:
			flush()
			events = append(events, event{kind: evResize})
		}
	}
	flush()
	return events
}

func winModifiers(state uint32) modifiers {
	var mod modifiers
	if state&0x10 != 0 {
		mod |= modShift
	}
	if state&0x3 != 0 {
		mod |= modAlt
	}
	if state&0xc != 0 {
		mod |= modCtrl
	}
	return mod
}

// key translates a key record.
func (kb *winInput) key(k *keyRecord) (keyPress, bool) {
	mod := winModifiers(k.controlKeyState)
	if special, ok := winKeys[k.virtualKeyCode]; ok {
		if special == keyTab && mod&modShift != 0 {
			return keyPress{k: keyBackTab, mod: mod}, true
		}
		return keyPress{k: special, mod: mod}, true
	}
	c := k.unicodeChar
	switch {
	case c == 0:
		return keyPress{}, false // Shift, Ctrl and other keys without a character
	case c >= 0xd800 && c < 0xdc00:
		kb.high = c
		return keyPress{}, false
	case c >= 0xdc00 && c < 0xe000:
		r := rune(kb.high-0xd800)<<10 | rune(c-0xdc00) + 0x10000
		kb.high = 0
		return keyPress{k: keyRune, r: r, mod: mod &^ modShift}, true
	case c >= 1 && c <= 26:
		return keyPress{k: keyRune, r: rune('a' + c - 1), mod: mod | modCtrl}, true
	case c < 0x20:
		return keyPress{}, false
	}
	if mod&modCtrl != 0 && mod&modAlt != 0 {
		mod &^= modCtrl | modAlt // AltGr typing a character
	}
	return keyPress{k: keyRune, r: rune(c), mod: mod &^ modShift}, true
}

// mouse translates a mouse record: a press or release (from the change in
// the buttons held), a drag, or a turn of the wheel.
func (kb *winInput) mouse(m *mouseRecord) (event, bool) {
	const (
		moved   = 0x1
		double  = 0x2
		wheeled = 0x4
	)
	ev := mouseEvent{x: int(m.x), y: int(m.y), mod: winModifiers(m.controlKeyState)}
	switch {
	case m.eventFlags&wheeled != 0:
		ev.action = mouseWheelUp
		if int16(m.buttonState>>16) < 0 {
			ev.action = mouseWheelDown
		}
	case m.eventFlags&moved != 0:
		if m.buttonState&0x1 == 0 {
			return event{}, false // movement with no button held
		}
		ev.action = mouseDrag
	default:
		changed := m.buttonState ^ kb.buttons
		if m.eventFlags&double != 0 {
			changed = m.buttonState & 0x7
		}
		pressed, released := m.buttonState&changed, kb.buttons&changed
		kb.buttons = m.buttonState
		switch {
		case pressed&0x1 != 0:
			ev.action, ev.button = mousePress, 0
		case pressed&0x2 != 0:
			ev.action, ev.button = mousePress, 2
		case pressed&0x4 != 0:
			ev.action, ev.button = mousePress, 1
		case released&0x1 != 0:
			ev.action, ev.button = mouseRelease, 0
		case released != 0:
			ev.action, ev.button = mouseRelease, 2
		default:
			return event{}, false
		}
	}
	return event{kind: evMouse, mouse: ev}, true
}

func (kb *winInput) close() {
	close(kb.done)
	<-kb.exited
	procSetConsoleMode.Call(uintptr(kb.h), uintptr(kb.mode))
}
