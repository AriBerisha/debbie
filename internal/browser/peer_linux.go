package browser

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// peerCheck accepts only a process of this user (SO_PEERCRED).
func peerCheck(conn net.Conn) error {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("not a socket")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("the peer runs as user %d", cred.Uid)
	}
	return nil
}
