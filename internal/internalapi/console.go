package internalapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/console"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/internal/vm"
)

// ConsoleBroker is what the console route needs from the agent's broker.
type ConsoleBroker interface {
	Attach(ctx context.Context, vmID string, running bool, who console.Who) (*console.Session, error)
}

// consoleWriteTimeout bounds one write to the client.
const consoleWriteTimeout = 10 * time.Second

// maxConsoleInput bounds one input message.
const maxConsoleInput = 64 << 10

// isConsole reports whether a request is for a console stream, which
// lives as long as the client keeps it open.
func isConsole(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, agentproto.Prefix+"/vms/") && strings.HasSuffix(r.URL.Path, "/console/serial")
}

// vmConsole relays a VM's serial console over a WebSocket on the agent
// socket. skep-api is the only client; it has checked the user's session
// and permission, and the actor headers name the user.
func (h *handler) vmConsole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid VM ID")
		return
	}
	found, err := h.vms.VM(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	sub := submission(r)
	sess, err := h.console.Attach(r.Context(), id, vm.Running(found.Observed.State), console.Who{
		Actor: sub.Actor, PeerUID: sub.PeerUID, RequestID: sub.RequestID, SourceIP: sub.SourceIP,
	})
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	defer sess.Detach(context.WithoutCancel(r.Context()))

	// The server's per-request deadlines do not apply to a stream.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.log.Warn("console: the WebSocket handshake failed", "vm_id", id, "error", err)
		return
	}
	defer conn.CloseNow()
	Relay(r.Context(), conn, sess)
}

// Relay copies a console session to a WebSocket and the WebSocket's
// messages to the session, until either ends.
func Relay(ctx context.Context, conn *websocket.Conn, sess *console.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn.SetReadLimit(maxConsoleInput)

	write := func(p []byte) error {
		wctx, wcancel := context.WithTimeout(ctx, consoleWriteTimeout)
		defer wcancel()
		if err := conn.Write(wctx, websocket.MessageBinary, p); err != nil {
			return err
		}
		sess.CountOutput(len(p))
		return nil
	}
	if len(sess.Scrollback) > 0 {
		if err := write(sess.Scrollback); err != nil {
			return
		}
	}
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if _, err := sess.Write(data); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Ended():
			_ = conn.Close(websocket.StatusNormalClosure, closeReason(sess.Reason()))
			return
		case chunk := <-sess.Output():
			if err := write(chunk); err != nil {
				return
			}
		}
	}
}

// closeReason fits a reason into a close frame (at most 123 bytes).
func closeReason(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
