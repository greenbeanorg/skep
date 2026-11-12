package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultPort is the port of skep-api when a server address names none
// (ADR 0006 decision 7).
const DefaultPort = "8443"

// pinRE is the form of a public key pin: "sha256:" and 64 lowercase hex
// digits, the form "skep-api tls fingerprint" prints.
var pinRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidPin reports whether s is a public key pin in the form
// "sha256:<64 lowercase hex digits>".
func ValidPin(s string) bool { return pinRE.MatchString(s) }

// PublicKeyPin returns the pin of a certificate: the SHA-256 of its
// SubjectPublicKeyInfo, as "sha256:<hex>". It is the value
// "skep-api tls fingerprint" prints, and it does not change when skep-api
// renews its certificate, because renewal keeps the key (ADR 0006
// decision 6).
func PublicKeyPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// KeyMismatchError is returned when the server presents a key other than
// the pinned one. Nothing was sent to that server.
type KeyMismatchError struct {
	// Pinned is the pin the client expected.
	Pinned string
	// Presented is the pin of the key the server presented.
	Presented string
}

func (e *KeyMismatchError) Error() string {
	return fmt.Sprintf("the server presented public key %s, but %s is pinned for it", e.Presented, e.Pinned)
}

// NormalizeServer turns a server address into the form the client uses and
// stores pins and sessions under: "https://host:port". It accepts "host",
// "host:port", and "https://host[:port]". Plain HTTP, paths, queries, and
// user information are refused: skep-api has no plaintext listener, and
// the API path is fixed.
func NormalizeServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no server given")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server address: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("invalid server address %q: only https is supported", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid server address %q: give only the host and, optionally, the port", raw)
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return "", fmt.Errorf("invalid server address %q: no host", raw)
	}
	if port == "" {
		port = DefaultPort
	}
	return "https://" + net.JoinHostPort(strings.ToLower(host), port), nil
}

// hostPort returns the "host:port" of a normalized server address.
func hostPort(server string) (string, error) {
	norm, err := NormalizeServer(server)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(norm, "https://"), nil
}

// tlsConfig returns the client TLS configuration. There are two modes, and
// there is no configuration in which neither check runs:
//
//   - Pin only (verifyChain false), for skep-api's self-signed certificate.
//     The connection is accepted if and only if the leaf certificate
//     carries the pinned public key. The chain and host name are not
//     checked: the certificate is self-signed, and the pin names the exact
//     key, which is the stronger check. That is why InsecureSkipVerify is
//     set; VerifyConnection runs regardless of it and refuses every
//     connection whose key does not match.
//   - Chain (verifyChain true), for an operator-supplied certificate. The
//     chain is verified against roots (the system roots when nil) and the
//     host name against the certificate, as any TLS client does, so a
//     renewal with a new key keeps working. A pin is optional here; if one
//     is given, it is checked in addition.
func tlsConfig(host, pin string, verifyChain bool, roots *x509.CertPool) (*tls.Config, error) {
	if pin != "" && !ValidPin(pin) {
		return nil, fmt.Errorf("invalid public key pin %q", pin)
	}
	if !verifyChain && pin == "" {
		return nil, errors.New("a public key pin is required unless the certificate chain is verified")
	}
	checkPin := func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("the server presented no certificate")
		}
		got := PublicKeyPin(cs.PeerCertificates[0])
		if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
			return &KeyMismatchError{Pinned: pin, Presented: got}
		}
		return nil
	}
	if !verifyChain {
		return &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // replaced by the pin check; see the doc comment
			VerifyConnection:   checkPin,
		}, nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: host,
		RootCAs:    roots, // nil: the system roots
	}
	if pin != "" {
		// Runs after the standard verification has succeeded.
		cfg.VerifyConnection = checkPin
	}
	return cfg, nil
}

// LoadCAFile reads a PEM file of CA certificates for Options.RootCAs.
func LoadCAFile(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s contains no PEM certificate", path)
	}
	return pool, nil
}

// IsUntrustedCertificate reports whether err is a failed chain or host
// name verification (Options.VerifyChain), as opposed to a network error.
func IsUntrustedCertificate(err error) bool {
	var (
		ua  x509.UnknownAuthorityError
		he  x509.HostnameError
		ci  x509.CertificateInvalidError
		cve *tls.CertificateVerificationError
	)
	return errors.As(err, &cve) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci)
}

// ProbeTimeout bounds FetchCertificate when the context has no deadline.
const ProbeTimeout = 10 * time.Second

// FetchCertificate connects to a server, completes a TLS 1.3 handshake,
// and returns the certificate it presented, without sending a request.
// It is the first step of trust on first use: the caller shows the pin to
// the user, who compares it with "skep-api tls fingerprint" on the host,
// and only a confirmed pin is used afterwards. The returned certificate
// is not trusted by this call.
func FetchCertificate(ctx context.Context, server string) (*x509.Certificate, error) {
	addr, err := hostPort(server)
	if err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
	}
	var leaf *x509.Certificate
	dialer := &tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Nothing is trusted here and nothing but the handshake is
		// sent; the certificate is only shown to the user.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server presented no certificate")
			}
			leaf = cs.PeerCertificates[0]
			return nil
		},
	}}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	conn.Close()
	return leaf, nil
}

// CheckConnection completes a TLS handshake with the checks opts asks for
// (pin, chain, or both) and closes the connection without sending a
// request. It lets a caller confirm a trust setting before storing it.
func CheckConnection(ctx context.Context, opts Options) error {
	addr, err := hostPort(opts.Server)
	if err != nil {
		return err
	}
	hostname, _, _ := net.SplitHostPort(addr)
	cfg, err := tlsConfig(hostname, opts.Pin, opts.VerifyChain, opts.RootCAs)
	if err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
	}
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", addr)
	if err != nil {
		var km *KeyMismatchError
		if errors.As(err, &km) {
			return km
		}
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	return conn.Close()
}
