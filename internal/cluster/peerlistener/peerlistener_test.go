package peerlistener

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
)

const testID = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"

type pins struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *pins) pinned(pin string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.m[pin]
	return id, ok
}

func newKey(t *testing.T, now time.Time) *nodekey.Key {
	t.Helper()
	k, err := nodekey.Create(t.TempDir(), testID, now)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// start opens a listener on a free loopback port that pins only member.
func start(t *testing.T, server, member *nodekey.Key, now func() time.Time) (*Listener, *pins) {
	t.Helper()
	p := &pins{m: map[string]string{member.Pin(): "member-node"}}
	// Port 0: the tests must not depend on 8444 being free.
	l, err := Listen(Options{
		Address:     netip.MustParseAddrPort("127.0.0.1:0"),
		Certificate: server.Certificate,
		Pinned:      p.pinned,
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, p
}

// dial does a handshake and one read, which is where a TLS 1.3 client
// learns that the server refused its certificate. It returns the error of
// whichever failed, or io.EOF when the server accepted the key and closed.
func dial(l *Listener, cert *tls.Certificate, min, max uint16) error {
	conf := &tls.Config{InsecureSkipVerify: true, MinVersion: min, MaxVersion: max} // test client; the server side is under test
	if cert != nil {
		conf.Certificates = []tls.Certificate{*cert}
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", l.Addr().String(), conf)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	return err
}

func refused(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && strings.Contains(err.Error(), "tls:")
}

func TestPinnedKeyAcceptedThenClosed(t *testing.T) {
	now := time.Now()
	key := newKey(t, now)
	l, _ := start(t, key, key, time.Now)
	// The node's own key is the only one in a one-node membership. It
	// is accepted, and nothing is served: the connection just ends.
	if err := dial(l, key.Certificate(), tls.VersionTLS13, tls.VersionTLS13); !errors.Is(err, io.EOF) {
		t.Fatalf("pinned key: %v, want a clean close", err)
	}
}

func TestUnknownKeyRefused(t *testing.T) {
	now := time.Now()
	key := newKey(t, now)
	l, _ := start(t, key, key, time.Now)
	other := newKey(t, now)
	if err := dial(l, other.Certificate(), tls.VersionTLS13, tls.VersionTLS13); !refused(err) {
		t.Fatalf("unknown key over TLS 1.3: %v, want a TLS alert", err)
	}
}

func TestNoClientCertificateRefused(t *testing.T) {
	key := newKey(t, time.Now())
	l, _ := start(t, key, key, time.Now)
	if err := dial(l, nil, tls.VersionTLS13, tls.VersionTLS13); !refused(err) {
		t.Fatalf("no client certificate: %v, want a TLS alert", err)
	}
}

func TestTLS12Refused(t *testing.T) {
	key := newKey(t, time.Now())
	l, _ := start(t, key, key, time.Now)
	// Even the pinned key is refused over TLS 1.2.
	err := dial(l, key.Certificate(), tls.VersionTLS12, tls.VersionTLS12)
	if !refused(err) || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("TLS 1.2: %v, want a protocol version alert", err)
	}
}

func TestExpiredCertificateRefusedDespitePin(t *testing.T) {
	issued := time.Now()
	key := newKey(t, issued)
	later := func() time.Time { return issued.Add(nodekey.Validity + time.Hour) }
	l, _ := start(t, key, key, later)
	if err := dial(l, key.Certificate(), tls.VersionTLS13, tls.VersionTLS13); !refused(err) {
		t.Fatalf("expired certificate with a pinned key: %v, want a TLS alert", err)
	}
}

func TestPinIsCheckedAtEveryHandshake(t *testing.T) {
	now := time.Now()
	key := newKey(t, now)
	l, p := start(t, key, key, time.Now)
	if err := dial(l, key.Certificate(), tls.VersionTLS13, tls.VersionTLS13); !errors.Is(err, io.EOF) {
		t.Fatalf("before removal: %v", err)
	}
	// The key leaves the membership: the next handshake is refused.
	p.mu.Lock()
	delete(p.m, key.Pin())
	p.mu.Unlock()
	if err := dial(l, key.Certificate(), tls.VersionTLS13, tls.VersionTLS13); !refused(err) {
		t.Fatalf("after removal: %v, want a TLS alert", err)
	}
}

func TestListenRefusesUnspecifiedAddress(t *testing.T) {
	key := newKey(t, time.Now())
	p := &pins{m: map[string]string{}}
	for _, a := range []string{"0.0.0.0:8444", "[::]:8444"} {
		if l, err := Listen(Options{Address: netip.MustParseAddrPort(a), Certificate: key.Certificate, Pinned: p.pinned}); err == nil {
			l.Close()
			t.Fatalf("listened on %s", a)
		}
	}
}

func TestServerConfigIsTLS13Only(t *testing.T) {
	key := newKey(t, time.Now())
	c := TLSConfig(Options{Certificate: key.Certificate, Pinned: (&pins{}).pinned})
	if c.MinVersion != tls.VersionTLS13 || c.ClientAuth != tls.RequireAnyClientCert || !c.SessionTicketsDisabled {
		t.Fatalf("server TLS configuration: min %x, client auth %v", c.MinVersion, c.ClientAuth)
	}
}
