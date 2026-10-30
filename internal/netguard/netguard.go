// Package netguard is the only place that builds HTTP clients, dialers and
// TLS configurations.
//
// A provider client reaches exactly one host and port, ignores proxy
// settings, refuses redirects and, when the provider has a certificate pin,
// accepts only the pinned certificate.
package netguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Timeouts for every provider connection.
const (
	DialTimeout         = 10 * time.Second
	TLSHandshakeTimeout = 10 * time.Second
	// DefaultHeaderTimeout is long because a local server can spend minutes
	// processing a long prompt before it sends the response headers.
	DefaultHeaderTimeout = 600 * time.Second
)

// Endpoint is the single host and port a provider client may connect to.
type Endpoint struct {
	Host      string // hostname or IP, as written in the provider URL
	Port      string // explicit; default 443 for https, 80 for http
	PinSHA256 string // optional, hex SHA-256 of the leaf certificate DER
}

// Options tune a provider client.
type Options struct {
	// HeaderTimeout bounds the wait for response headers once a request has
	// been sent. Zero means DefaultHeaderTimeout.
	HeaderTimeout time.Duration
	// ProviderName appears in pin-mismatch errors, which tell the user to run
	// "/provider trust <name>".
	ProviderName string
	// TrustHint, if set, replaces that advice, for a client that isn't a
	// provider's, such as an MCP server's.
	TrustHint string
	// RootCAs replaces the system roots when an unpinned certificate is
	// verified. Nil means the system roots. Tests use it to stand in for a
	// public CA.
	RootCAs *x509.CertPool
}

// ErrRedirect is wrapped by the error a provider client returns when the
// server answers with a redirect.
var ErrRedirect = errors.New("provider endpoints must not redirect")

// BlockedError reports an attempt to connect to an address other than the
// client's endpoint.
type BlockedError struct {
	Addr string
}

func (e *BlockedError) Error() string {
	return "blocked connection to " + e.Addr + ": not a configured provider"
}

// EndpointFromURL returns the endpoint a provider URL points at, with the
// default port filled in. pin is copied into the endpoint and may be empty.
func EndpointFromURL(rawURL, pin string) (Endpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// url.Error repeats the whole URL, which may hold credentials.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Endpoint{}, fmt.Errorf("invalid URL: %v", err)
	}
	var port string
	switch u.Scheme {
	case "https":
		port = "443"
	case "http":
		port = "80"
		if pin != "" {
			return Endpoint{}, errors.New("a certificate pin needs an https URL")
		}
	default:
		return Endpoint{}, fmt.Errorf("unsupported URL scheme %q: use http or https", u.Scheme)
	}
	if u.User != nil {
		return Endpoint{}, errors.New("credentials in the URL are not supported")
	}
	if u.Hostname() == "" {
		return Endpoint{}, errors.New("the URL has no host")
	}
	if p := u.Port(); p != "" {
		port = p
	}
	ep := Endpoint{Host: u.Hostname(), Port: port, PinSHA256: pin}
	if _, err := newTarget(ep); err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

// target is an endpoint in the form the dial guard compares against.
type target struct {
	name string     // host as written, without IPv6 brackets
	host string     // lower-case hostname; empty when ip is set
	ip   netip.Addr // the host, when it is an IP address
	port int
	addr string // name and port joined, for dialing and messages
}

func newTarget(ep Endpoint) (target, error) {
	name := ep.Host
	if strings.HasPrefix(name, "[") && strings.HasSuffix(name, "]") {
		name = name[1 : len(name)-1]
	}
	if name == "" {
		return target{}, errors.New("endpoint has no host")
	}
	port, err := parsePort(ep.Port)
	if err != nil {
		return target{}, err
	}
	t := target{name: name, port: port, addr: net.JoinHostPort(name, strconv.Itoa(port))}
	if ip, err := netip.ParseAddr(name); err == nil {
		t.ip = ip.Unmap()
	} else {
		t.host = strings.ToLower(name)
	}
	return t, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

// allows reports whether dialing addr reaches the endpoint. IP addresses
// compare by value, hostnames case-insensitively.
func (t target) allows(network, addr string) bool {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return false
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if port, err := parsePort(portStr); err != nil || port != t.port {
		return false
	}
	if t.ip.IsValid() {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Unmap() == t.ip
	}
	return strings.EqualFold(host, t.host)
}

// timeouts are the fixed connection timeouts. Tests shorten them.
type timeouts struct {
	dial, tlsHandshake time.Duration
}

var defaultTimeouts = timeouts{dial: DialTimeout, tlsHandshake: TLSHandshakeTimeout}

// NewClient returns an HTTP client that can only reach ep. It never uses a
// proxy and never follows a redirect. With ep.PinSHA256 set, the TLS
// handshake accepts only the certificate with that fingerprint; without it,
// certificates are verified against the system roots as usual.
func NewClient(ep Endpoint, opts Options) (*http.Client, error) {
	return newClient(ep, opts, defaultTimeouts)
}

func newClient(ep Endpoint, opts Options, tm timeouts) (*http.Client, error) {
	t, err := newTarget(ep)
	if err != nil {
		return nil, err
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: opts.RootCAs}
	if ep.PinSHA256 != "" {
		pin, err := ParsePin(ep.PinSHA256)
		if err != nil {
			return nil, err
		}
		// The pin replaces CA and hostname verification, which a self-signed
		// certificate would fail. VerifyConnection runs inside the handshake
		// and aborts it on a mismatch, before any request byte is sent.
		tlsConf.InsecureSkipVerify = true
		tlsConf.VerifyConnection = verifyPin(pin, t.addr, opts.ProviderName, opts.TrustHint)
	}
	headerTimeout := opts.HeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultHeaderTimeout
	}
	dialer := &net.Dialer{Timeout: tm.dial, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // HTTP_PROXY, HTTPS_PROXY and ALL_PROXY are ignored
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !t.allows(network, addr) {
				return nil, &BlockedError{Addr: addr}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   tm.tlsHandshake,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport, CheckRedirect: refuseRedirect}, nil
}

func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("server redirected to %s: %w", req.URL.Redacted(), ErrRedirect)
}
