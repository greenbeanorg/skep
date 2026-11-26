package internalapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net"

	"github.com/greenbeanorg/skep/internal/peercred"
	"github.com/greenbeanorg/skep/internal/socklisten"
)

// MaxSocketPath is the longest socket path accepted (socklisten's).
const MaxSocketPath = socklisten.MaxSocketPath

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

// Listener accepts connections on the agent socket from allowed peers
// only. It is the shared listener of package socklisten (step 8d-1, D1).
type Listener = socklisten.Listener

// Listen creates the agent's socket (socklisten.Listen): bound under a
// temporary name, given its owner and mode, and only then renamed, so it
// is never reachable with the permissions of the process's umask.
func Listen(opts ListenOptions) (*Listener, error) {
	return socklisten.Listen(socklisten.Options{Name: "internalapi", LogName: "agent socket", Path: opts.Path,
		Mode: opts.Mode, GID: opts.GID, AllowedUIDs: opts.AllowedUIDs, MaxConnections: opts.MaxConnections, Logger: opts.Logger})
}

// ConnContext is the http.Server.ConnContext hook that makes the verified
// peer of a connection available to the handlers.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return socklisten.ConnContext(ctx, c)
}

// WithPeer returns a context that carries a verified peer (for tests of
// the handler).
func WithPeer(ctx context.Context, cred peercred.Cred) context.Context {
	return socklisten.WithPeer(ctx, cred)
}

// PeerFrom returns the kernel-verified peer of the connection a request
// arrived on.
func PeerFrom(ctx context.Context) (peercred.Cred, bool) { return socklisten.PeerFrom(ctx) }
