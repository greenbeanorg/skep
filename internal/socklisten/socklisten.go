// Package socklisten is the Unix-socket listener the agent's internal API
// and skep-cluster's local API share (step 8d-1, D1): the socket is bound
// under a temporary name, given its mode (and optionally its group), and
// only then renamed to its real name; every connection's peer is read
// from the kernel and checked against an allow-list before a byte is
// read; the number of open connections is bounded; and the verified peer
// is handed to the HTTP handlers through the request context.
package socklisten

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

// MaxSocketPath is the longest socket path accepted. sun_path holds 104
// bytes on FreeBSD, including the terminating NUL; the temporary name used
// while the socket is created needs some of them.
const MaxSocketPath = 88

// Options configures a socket.
type Options struct {
	// Name prefixes errors (for example "internalapi").
	Name string
	// LogName prefixes log lines (for example "agent socket").
	LogName string
	// Path is the socket file. Its directory must already exist.
	Path string
	// Mode is the file mode of the socket.
	Mode fs.FileMode
	// GID is the group that owns the socket, or -1 to leave the group as
	// the kernel set it.
	GID int
	// AllowedUIDs lists the peers that may connect. Everyone else is
	// disconnected before a single byte is read.
	AllowedUIDs []uint32
	// MaxConnections bounds the number of open connections.
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
	logName string
	log     *slog.Logger
}

// Listen creates the socket and starts listening on it. A stale socket
// left by a previous run is replaced; any other kind of file at Path is an
// error.
func Listen(o Options) (*Listener, error) {
	if o.Path == "" {
		return nil, fmt.Errorf("%s: socket path must not be empty", o.Name)
	}
	if len(o.Path) > MaxSocketPath {
		return nil, fmt.Errorf("%s: socket path %q is longer than %d bytes", o.Name, o.Path, MaxSocketPath)
	}
	if len(o.AllowedUIDs) == 0 {
		return nil, fmt.Errorf("%s: the peer allow-list is empty; nobody could connect", o.Name)
	}
	if o.MaxConnections < 1 {
		return nil, fmt.Errorf("%s: max connections must be positive", o.Name)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if info, err := os.Lstat(o.Path); err == nil {
		if info.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s: %s exists and is not a socket", o.Name, o.Path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", o.Name, err)
	}
	tmp := o.Path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("%s: listen in %s: %w", o.Name, filepath.Dir(o.Path), err)
	}
	// The listener would unlink the temporary name on Close; the socket
	// has been renamed by then.
	ln.SetUnlinkOnClose(false)
	fail := func(err error) (*Listener, error) {
		ln.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("%s: prepare socket %s: %w", o.Name, o.Path, err)
	}
	if err := os.Chmod(tmp, o.Mode.Perm()); err != nil {
		return fail(err)
	}
	if o.GID >= 0 {
		if err := os.Chown(tmp, -1, o.GID); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(tmp, o.Path); err != nil {
		return fail(err)
	}
	return &Listener{ln: ln, path: o.Path, allowed: append([]uint32(nil), o.AllowedUIDs...),
		slots: make(chan struct{}, o.MaxConnections), logName: o.LogName, log: o.Logger}, nil
}

// Accept returns the next connection from an allowed peer. Connections from
// anyone else are closed and logged.
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
			// Unknown peer: deny.
			l.log.Error(l.logName+": cannot read peer credentials; connection refused", "error", err)
			uc.Close()
			<-l.slots
			continue
		}
		if !peercred.Allowed(cred, l.allowed) {
			l.log.Warn(l.logName+": connection refused, peer is not on the allow-list",
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

// conn is an accepted connection together with its verified peer.
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

// ConnContext is the http.Server.ConnContext hook that makes the verified
// peer of a connection available to the handlers.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if pc, ok := c.(*conn); ok {
		return context.WithValue(ctx, peerKey{}, pc.cred)
	}
	return ctx
}

// WithPeer returns a context that carries a verified peer. The listener
// does this for real connections; tests of the handlers use it directly.
func WithPeer(ctx context.Context, cred peercred.Cred) context.Context {
	return context.WithValue(ctx, peerKey{}, cred)
}

// PeerFrom returns the kernel-verified peer of the connection a request
// arrived on.
func PeerFrom(ctx context.Context) (peercred.Cred, bool) {
	cred, ok := ctx.Value(peerKey{}).(peercred.Cred)
	return cred, ok
}
