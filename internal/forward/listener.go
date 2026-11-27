package forward

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
)

// The node listener's bounds (D18).
const (
	MaxConns          = 256
	MaxConnsPerRemote = 32
	headerTimeout     = 10 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 60 * time.Second
	maxHeaderBytes    = 16 << 10
)

// limitListener closes, never queues, a connection over the limits: 256
// open in all and 32 per remote address.
type limitListener struct {
	net.Listener
	mu     sync.Mutex
	open   int
	remote map[string]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		l.mu.Lock()
		if l.open >= MaxConns || l.remote[host] >= MaxConnsPerRemote {
			l.mu.Unlock()
			_ = c.Close()
			continue
		}
		l.open++
		l.remote[host]++
		l.mu.Unlock()
		return &countedConn{Conn: c, l: l, host: host}, nil
	}
}

type countedConn struct {
	net.Conn
	l    *limitListener
	host string
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.open--
		if c.l.remote[c.host]--; c.l.remote[c.host] <= 0 {
			delete(c.l.remote, c.host)
		}
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}

// NodeListener is the open node-to-node listener on port 8445 (D16–D19):
// TLS 1.3, HTTP/1.1, a client certificate accepted by its pin alone, and
// only the key of a usable peer's skep-api.
type NodeListener struct {
	srv  *http.Server
	ln   net.Listener
	addr netip.AddrPort
	done chan struct{}
}

// ListenNode binds addr and serves handler. accept is asked at every
// handshake; certificate returns the API node key's certificate.
func ListenNode(addr netip.AddrPort, certificate func() *tls.Certificate, accept func(pin string) bool,
	handler http.Handler, now func() time.Time, log *slog.Logger) (*NodeListener, error) {
	if !addr.IsValid() || addr.Addr().IsUnspecified() {
		return nil, errors.New("forward: the node listener binds one specific address")
	}
	if log == nil {
		log = slog.Default()
	}
	// peerlistener's rules: TLS 1.3, any certificate asked for, no CA,
	// no session tickets, the certificate's validity checked, the pin
	// alone decides.
	conf := peerlistener.TLSConfig(peerlistener.Options{Certificate: certificate, Accept: accept, Now: now})
	conf.NextProtos = []string{"http/1.1"}
	network := "tcp4"
	if addr.Addr().Is6() {
		network = "tcp6"
	}
	raw, err := net.Listen(network, addr.String())
	if err != nil {
		return nil, err
	}
	ln := tls.NewListener(&limitListener{Listener: raw, remote: map[string]int{}}, conf)
	srv := &http.Server{
		Handler: handler,
		// The handshake is bounded by the shortest of the three (F8).
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	// Every forwarded request is a new handshake (D17).
	srv.SetKeepAlivesEnabled(false)
	l := &NodeListener{srv: srv, ln: ln, addr: addr, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		_ = srv.Serve(ln)
	}()
	return l, nil
}

// Addr is the bound address.
func (l *NodeListener) Addr() netip.AddrPort { return l.addr }

// BoundAddr is the bound address as the kernel reports it (port 0 in
// tests).
func (l *NodeListener) BoundAddr() string { return l.ln.Addr().String() }

// Close stops the listener and its connections.
func (l *NodeListener) Close() error {
	err := l.srv.Close()
	<-l.done
	return err
}

// nodeAddr is the node listener's address for a listen address.
func nodeAddr(listen string, port int) (netip.AddrPort, error) {
	a, err := netip.ParseAddr(listen)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.ParseAddrPort(net.JoinHostPort(a.String(), strconv.Itoa(port)))
}
