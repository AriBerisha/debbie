//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package ui

import "syscall"

// The ioctl requests that read and set a terminal's settings.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
