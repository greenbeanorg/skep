// Package localapi is skep-cluster's local API on its Unix socket: who may
// connect is decided by the kernel-reported peer credentials, and every
// operation names which of the two allowed callers, root and the skep
// account (skep-api), may call it (ADR 0013 B12).
//
// The service account itself is not an allowed peer, and no other account
// is.
package localapi

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

// Listener accepts connections from allowed peers only: the shared
// listener of package socklisten (step 8d-1, D1).
type Listener = socklisten.Listener

// Listen creates the socket with SocketMode and the group the kernel
// gives it (socklisten.Listen).
func Listen(o ListenOptions) (*Listener, error) {
	return socklisten.Listen(socklisten.Options{Name: "localapi", LogName: "cluster socket", Path: o.Path,
		Mode: SocketMode, GID: -1, AllowedUIDs: o.AllowedUIDs, MaxConnections: o.MaxConnections, Logger: o.Logger})
}

// ConnContext is the http.Server.ConnContext hook that hands the verified
// peer of a connection to the handlers.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return socklisten.ConnContext(ctx, c)
}

// WithPeer returns a context that carries a verified peer. The listener
// does this for real connections; tests of the handler use it directly.
func WithPeer(ctx context.Context, cred peercred.Cred) context.Context {
	return socklisten.WithPeer(ctx, cred)
}

// PeerFrom returns the kernel-verified peer of a request's connection.
func PeerFrom(ctx context.Context) (peercred.Cred, bool) { return socklisten.PeerFrom(ctx) }
