package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"
)

// CertInfo describes the certificate a server presented, for the
// trust-on-first-use prompt.
type CertInfo struct {
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	DNSNames    []string
	IPAddresses []string
	// Fingerprint is the certificate's SHA-256 as colon-separated upper-case
	// hex, the form shown to the user.
	Fingerprint string
	// Pin is the same fingerprint in the form stored in the config.
	Pin string
	// Verified is true when the certificate verifies against the system roots
	// for this host. Such a certificate needs no pin.
	Verified bool
	// VerifyError says why verification failed when Verified is false.
	VerifyError string
}

// ProbeCertificate connects to ep, performs a TLS handshake that records the
// server's certificate, and closes the connection. It makes no HTTP request,
// so nothing beyond the handshake is sent, and in particular no API key.
// The handshake verifies the certificate as usual; one that fails
// verification is still returned, with Verified false, so the user can decide
// whether to pin it.
func ProbeCertificate(ctx context.Context, ep Endpoint, opts Options) (*CertInfo, error) {
	return probeCertificate(ctx, ep, opts, defaultTimeouts)
}

func probeCertificate(ctx context.Context, ep Endpoint, opts Options, tm timeouts) (*CertInfo, error) {
	t, err := newTarget(ep)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: tm.dial}
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	hsCtx, cancel := context.WithTimeout(ctx, tm.tlsHandshake)
	defer cancel()
	tc := tls.Client(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    opts.RootCAs,
		ServerName: t.name,
	})
	info := &CertInfo{}
	var chain []*x509.Certificate
	var verr *tls.CertificateVerificationError
	switch err := tc.HandshakeContext(hsCtx); {
	case err == nil:
		chain = tc.ConnectionState().PeerCertificates
		info.Verified = true
	case errors.As(err, &verr):
		chain = verr.UnverifiedCertificates
		info.VerifyError = verr.Err.Error()
	default:
		return nil, fmt.Errorf("TLS handshake with %s: %w", t.addr, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("%s presented no certificate", t.addr)
	}

	leaf := chain[0]
	sum := Fingerprint(leaf.Raw)
	info.Subject = leaf.Subject.String()
	info.Issuer = leaf.Issuer.String()
	info.NotBefore = leaf.NotBefore
	info.NotAfter = leaf.NotAfter
	info.DNSNames = leaf.DNSNames
	for _, ip := range leaf.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	info.Fingerprint = FormatFingerprint(sum)
	info.Pin = hex.EncodeToString(sum)
	return info, nil
}
