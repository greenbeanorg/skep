// Package localapi is skep-cluster's local API on its Unix socket: who may
// connect is decided by the kernel-reported peer credentials, and every
// operation names which of the two allowed callers, root and the skep
// account (skep-api), may call it (ADR 0013 B12, step 8a).
//
// The service account itself is not an allowed peer, and no other account
// is.
package localapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/greenbeanorg/skep/internal/peercred"
)

// MaxSocketPath is the longest socket path accepted (sun_path is 104
// bytes on FreeBSD; the temporary name needs some of them).
const MaxSocketPath = 88

// SocketMode is the mode of the socket: its owner (the service) and the
// group of its directory (skep, inherited on FreeBSD) may connect.
const SocketMode fs.FileMode = 0o660

// ListenOptions configures the socket.
type ListenOptions struct {
	// Path is the socket file. Its directory must exist.
	Path string
	// AllowedUIDs lists the peers that may connect: root and the API
	// account. Everyone else is disconnected before a byte is read.
	AllowedUIDs []uint32
	// MaxConnections bounds the open connections.
	MaxConnections int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Listener accepts connections from allowed peers only.
type Listener struct {
	ln      *net.UnixListener
	path    string
	allowed []uint32
	slots   chan struct{}
	log     *slog.Logger
}

// Listen creates the socket under a temporary name, sets its mode, and
// renames it, so it is never reachable with the umask's permissions. A
// stale socket is replaced; any other file at Path is an error.
func Listen(o ListenOptions) (*Listener, error) {
	if o.Path == "" || len(o.Path) > MaxSocketPath {
		return nil, fmt.Errorf("localapi: socket path %q is empty or longer than %d bytes", o.Path, MaxSocketPath)
	}
	if len(o.AllowedUIDs) == 0 {
		return nil, errors.New("localapi: the peer allow-list is empty; nobody could connect")
	}
	if o.MaxConnections < 1 {
		return nil, errors.New("localapi: max connections must be positive")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if info, err := os.Lstat(o.Path); err == nil {
		if info.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("localapi: %s exists and is not a socket", o.Path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("localapi: %w", err)
	}
	tmp := o.Path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("localapi: listen in %s: %w", filepath.Dir(o.Path), err)
	}
	ln.SetUnlinkOnClose(false)
	fail := func(err error) (*Listener, error) {
		ln.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("localapi: prepare socket %s: %w", o.Path, err)
	}
	if err := os.Chmod(tmp, SocketMode); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, o.Path); err != nil {
		return fail(err)
	}
	return &Listener{
		ln:      ln,
		path:    o.Path,
		allowed: append([]uint32(nil), o.AllowedUIDs...),
		slots:   make(chan struct{}, o.MaxConnections),
		log:     o.Logger,
	}, nil
}

// Accept returns the next connection from an allowed peer. Any other
// connection is closed and logged.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		l.slots <- struct{}{}
		uc, err := l.ln.AcceptUnix()
		if err != nil {
			<-l.slots
			return nil, err
		}
		cred, err := peercred.FromConn(uc)
		if err != nil {
			l.log.Error("cluster socket: cannot read peer credentials; connection refused", "error", err)
			uc.Close()
			<-l.slots
			continue
		}
		if !peercred.Allowed(cred, l.allowed) {
			l.log.Warn("cluster socket: connection refused, peer is not on the allow-list",
				"peer_uid", cred.UID, "peer_gid", cred.GID)
			uc.Close()
			<-l.slots
			continue
		}
		return &conn{UnixConn: uc, cred: cred, release: func() { <-l.slots }}, nil
	}
}

// Close stops listening and removes the socket file.
func (l *Listener) Close() error {
	err := l.ln.Close()
	if rmErr := os.Remove(l.path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

// Addr returns the socket address.
func (l *Listener) Addr() net.Addr { return &net.UnixAddr{Name: l.path, Net: "unix"} }

type conn struct {
	*net.UnixConn
	cred    peercred.Cred
	once    sync.Once
	release func()
}

func (c *conn) Close() error {
	err := c.UnixConn.Close()
	c.once.Do(c.release)
	return err
}

type peerKey struct{}

// ConnContext is the http.Server.ConnContext hook that hands the verified
// peer of a connection to the handlers.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if pc, ok := c.(*conn); ok {
		return context.WithValue(ctx, peerKey{}, pc.cred)
	}
	return ctx
}

// WithPeer returns a context that carries a verified peer. The listener
// does this for real connections; tests of the handler use it directly.
func WithPeer(ctx context.Context, cred peercred.Cred) context.Context {
	return context.WithValue(ctx, peerKey{}, cred)
}

// PeerFrom returns the kernel-verified peer of a request's connection.
func PeerFrom(ctx context.Context) (peercred.Cred, bool) {
	cred, ok := ctx.Value(peerKey{}).(peercred.Cred)
	return cred, ok
}
