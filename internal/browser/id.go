// Package browser connects a session to the user's Chrome (SPEC §20):
// installing the extension and its native messaging host, the host that
// Chrome starts, the bridge between that host and the session, and the
// browser tools.
package browser

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"debbie/internal/app"
)

// HostName is the native messaging host's name, which Chrome allows only
// lowercase letters, digits, dots and underscores.
var HostName = strings.ReplaceAll(app.Name, "-", "_")

// ExtensionID returns the ID Chrome gives the extension whose manifest key
// is pub, a DER-encoded public key: the first 16 bytes of its SHA-256 hash
// in hexadecimal, with the digits 0-f written a-p (Chromium's
// crx_file::id_util::GenerateId).
func ExtensionID(pub []byte) string {
	sum := sha256.Sum256(pub)
	id := make([]byte, 32)
	for i, b := range sum[:16] {
		id[2*i], id[2*i+1] = 'a'+b>>4, 'a'+b&0xf
	}
	return string(id)
}

// Native messaging limits (developer.chrome.com, "Native messaging"): a
// message from the host to Chrome is at most 1 MB, one the other way at
// most 64 MiB.
const (
	maxToChrome   = 1 << 20
	maxFromChrome = 64 << 20
)

// readFrame reads one native messaging frame: a 32-bit length in the
// machine's byte order, then that much JSON. Messages over the bridge's
// socket use the same framing.
func readFrame(r io.Reader, limit int) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.NativeEndian, &n); err != nil {
		return nil, err
	}
	if int64(n) > int64(limit) {
		return nil, fmt.Errorf("a message of %d bytes is over the limit of %d", n, limit)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// writeFrame writes one frame, refusing one over limit.
func writeFrame(w io.Writer, msg []byte, limit int) error {
	if len(msg) > limit {
		return errors.New("the message is too large")
	}
	buf := make([]byte, 4+len(msg))
	binary.NativeEndian.PutUint32(buf, uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	return err
}
