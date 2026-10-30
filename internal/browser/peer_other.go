//go:build !windows && !linux

package browser

import "net"

// peerCheck has nothing to check with here: the standard library can't
// read a peer's credentials on macOS (LOCAL_PEERCRED). The socket's
// directory is the user's alone, and the token proves the peer could read
// it.
func peerCheck(net.Conn) error { return nil }
