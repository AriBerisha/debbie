package ui

import "syscall"

// The ioctl requests that read and set a terminal's settings.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
