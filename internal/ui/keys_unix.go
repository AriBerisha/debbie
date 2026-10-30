//go:build !windows

package ui

import (
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"
)

type unixInput struct {
	fd     int
	old    syscall.Termios // the terminal's settings before
	ch     chan event
	done   chan struct{}
	exited chan struct{}
	resize chan os.Signal
}

// openKeyboard turns off the terminal's line editing and echo, and reads
// its input. A read returns after a tenth of a second without input (VMIN
// 0, VTIME 1), which tells a lone Esc from the start of an escape sequence
// and lets close stop the reader. Mouse reporting and bracketed paste are
// turned on with escape sequences by whoever draws the screen.
func openKeyboard(in *os.File, opts inputOptions) (keyboard, error) {
	fd := int(in.Fd())
	var old syscall.Termios
	if err := termios(fd, ioctlGetTermios, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.IEXTEN
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	if opts.ctrlC {
		raw.Lflag &^= syscall.ISIG
	}
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := termios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	kb := &unixInput{fd: fd, old: old, ch: make(chan event, 64), done: make(chan struct{}), exited: make(chan struct{}),
		resize: make(chan os.Signal, 1)}
	signal.Notify(kb.resize, syscall.SIGWINCH)
	go kb.run()
	return kb, nil
}

func termios(fd int, req uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}

func (kb *unixInput) events() <-chan event { return kb.ch }

func (kb *unixInput) run() {
	defer close(kb.exited)
	buf := make([]byte, 4096)
	var pending []byte
	send := func(ev event) bool {
		select {
		case kb.ch <- ev:
			return true
		case <-kb.done:
			return false
		}
	}
	for {
		select {
		case <-kb.done:
			return
		case <-kb.resize:
			if !send(event{kind: evResize}) {
				return
			}
		default:
		}
		n, err := syscall.Read(kb.fd, buf)
		switch err {
		case nil, syscall.EINTR:
		case syscall.EAGAIN: // a non-blocking descriptor: VTIME doesn't wait
			time.Sleep(10 * time.Millisecond)
		default:
			return
		}
		// Nothing more within a tenth of a second ends what is pending.
		events, rest := parseInput(append(pending, buf[:max(0, n)]...), n <= 0)
		pending = rest
		for _, ev := range events {
			if !send(ev) {
				return
			}
		}
	}
}

func (kb *unixInput) close() {
	signal.Stop(kb.resize)
	close(kb.done)
	<-kb.exited
	termios(kb.fd, ioctlSetTermios, &kb.old)
}
