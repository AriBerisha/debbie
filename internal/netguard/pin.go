package netguard

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ParsePin decodes a certificate pin: the SHA-256 of the leaf certificate's
// DER encoding, written as 64 hex digits in either case, optionally with
// colons between the bytes.
func ParsePin(s string) ([]byte, error) {
	digits := strings.ReplaceAll(strings.TrimSpace(s), ":", "")
	if len(digits) != 2*sha256.Size {
		return nil, fmt.Errorf("invalid certificate pin: want a SHA-256 fingerprint (64 hex digits), got %d characters", len(digits))
	}
	sum, err := hex.DecodeString(digits)
	if err != nil {
		return nil, errors.New("invalid certificate pin: not hexadecimal")
	}
	return sum, nil
}

// CanonicalPin returns a pin in the form stored in the config: 64 lower-case
// hex digits.
func CanonicalPin(s string) (string, error) {
	sum, err := ParsePin(s)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum), nil
}

// Fingerprint returns the SHA-256 fingerprint of a DER-encoded certificate.
func Fingerprint(der []byte) []byte {
	sum := sha256.Sum256(der)
	return sum[:]
}

// FormatFingerprint renders a fingerprint the way it is shown to users:
// upper-case hex bytes separated by colons.
func FormatFingerprint(sum []byte) string {
	var b strings.Builder
	for i, c := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", c)
	}
	return b.String()
}

// PinMismatchError reports a server certificate that doesn't match the
// provider's pin.
type PinMismatchError struct {
	Addr     string
	Expected string // fingerprint from the config, colon-separated hex
	Received string // fingerprint of the certificate the server presented
	Provider string // provider name for the trust hint; may be empty
	Hint     string // what to do instead of /provider trust; may be empty
}

func (e *PinMismatchError) Error() string {
	hint := e.Hint
	if hint == "" {
		name := e.Provider
		if name == "" {
			name = "<name>"
		}
		hint = "run /provider trust " + name
	}
	return fmt.Sprintf("certificate of %s does not match the pinned fingerprint: expected %s, received %s; "+
		"if the server's certificate was replaced on purpose, %s",
		e.Addr, e.Expected, e.Received, hint)
}

// verifyPin returns a tls.Config.VerifyConnection callback that accepts only
// a leaf certificate whose SHA-256 fingerprint equals pin.
func verifyPin(pin []byte, addr, provider, hint string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("%s presented no certificate", addr)
		}
		got := Fingerprint(cs.PeerCertificates[0].Raw)
		if subtle.ConstantTimeCompare(got, pin) != 1 {
			return &PinMismatchError{
				Addr:     addr,
				Expected: FormatFingerprint(pin),
				Received: FormatFingerprint(got),
				Provider: provider,
				Hint:     hint,
			}
		}
		return nil
	}
}
