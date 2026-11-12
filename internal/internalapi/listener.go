package internalapi

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

// ListenOptions configures the agent's Unix socket.
type ListenOptions struct {
	// Path is the socket file. Its directory must already exist with the
	// right owner and mode; in production the rc.d script creates it.
	Path string
	// Mode is the file mode of the socket, for example 0660.
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

// Listener accepts connections on the agent socket from allowed peers only.
type Listener struct {
	ln      *net.UnixListener
	path    string
	allowed []uint32
	slots   chan struct{}
	log     *slog.Logger
}

// Listen creates the socket and starts listening on it.
//
// The socket is bound under a temporary name, given its owner and mode, and
// only then renamed to its real name, so it is never reachable with the
// permissions the process's umask happens to give it. A stale socket left by
// a previous run is replaced; any other kind of file at Path is an error.
func Listen(opts ListenOptions) (*Listener, error) {
	if opts.Path == "" {
		return nil, errors.New("internalapi: socket path must not be empty")
	}
	if len(opts.Path) > MaxSocketPath {
		return nil, fmt.Errorf("internalapi: socket path %q is longer than %d bytes", opts.Path, MaxSocketPath)
	}
	if len(opts.AllowedUIDs) == 0 {
		return nil, errors.New("internalapi: the peer allow-list is empty; nobody could connect")
	}
	if opts.MaxConnections < 1 {
		return nil, errors.New("internalapi: max connections must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if info, err := os.Lstat(opts.Path); err == nil {
		if info.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("internalapi: %s exists and is not a socket", opts.Path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("internalapi: %w", err)
	}

	tmp := opts.Path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("internalapi: listen on %s: %w", filepath.Dir(opts.Path), err)
	}
	// The listener would unlink the temporary name on Close; the socket
	// has been renamed by then.
	ln.SetUnlinkOnClose(false)
	fail := func(err error) (*Listener, error) {
		ln.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("internalapi: prepare socket %s: %w", opts.Path, err)
	}
	if err := os.Chmod(tmp, opts.Mode.Perm()); err != nil {
		return fail(err)
	}
	if opts.GID >= 0 {
		if err := os.Chown(tmp, -1, opts.GID); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(tmp, opts.Path); err != nil {
		return fail(err)
	}
	return &Listener{
		ln:      ln,
		path:    opts.Path,
		allowed: append([]uint32(nil), opts.AllowedUIDs...),
		slots:   make(chan struct{}, opts.MaxConnections),
		log:     opts.Logger,
	}, nil
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
			l.log.Error("agent socket: cannot read peer credentials; connection refused", "error", err)
			uc.Close()
			<-l.slots
			continue
		}
		if !peercred.Allowed(cred, l.allowed) {
			l.log.Warn("agent socket: connection refused, peer is not on the allow-list",
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

// PeerFrom returns the kernel-verified peer of the connection a request
// arrived on.
func PeerFrom(ctx context.Context) (peercred.Cred, bool) {
	cred, ok := ctx.Value(peerKey{}).(peercred.Cred)
	return cred, ok
}
