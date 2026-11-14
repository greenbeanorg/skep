package api

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// ConsoleDialer opens a VM's serial console on the agent, for the caller
// in ctx. The agent client implements it.
type ConsoleDialer interface {
	DialConsole(ctx context.Context, vmID string) (*websocket.Conn, error)
}

// Console timing (ADR 0009 decision 8b).
const (
	// DefaultConsoleSessionCheck is how often an open console re-checks
	// the session: a console never outlives it by more than this.
	DefaultConsoleSessionCheck = time.Minute
	// consoleActivityInterval is how often typing counts as use of the
	// session, the same throttle every request has.
	consoleActivityInterval = time.Minute
	consoleWriteTimeout     = 10 * time.Second
	maxConsoleMessage       = 64 << 10
	// statusSessionEnded is the close code a client sees when the session
	// behind its console has ended, expired, or been revoked.
	statusSessionEnded websocket.StatusCode = 4001
)

// Audit actions of the console.
const (
	actionConsoleAttach = "vm.console.attach"
	actionConsoleDetach = "vm.console.detach"
)

// vmConsole relays a VM's serial console between the browser (or any
// WebSocket client) and the agent. The route's permission, the session,
// and the origin were checked before it runs.
func (h *handler) vmConsole(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	dialer, ok := h.vms.(ConsoleDialer)
	if !ok {
		httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound, "no console is available")
		return
	}
	token, kind, _ := sessionToken(r)

	// The agent answers first, so a refusal (not running, busy) is an
	// ordinary error response, not a WebSocket that closes at once.
	agentConn, err := dialer.DialConsole(r.Context(), id)
	if err != nil {
		h.auditVM(r, actionConsoleAttach, id, nil, err)
		h.fail(w, r, err)
		return
	}
	defer agentConn.CloseNow()

	// The server's per-request deadlines do not apply to a stream.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	client, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.log.Warn("console: the WebSocket handshake failed", "vm_id", id, "error", err)
		return
	}
	defer client.CloseNow()
	h.auditVM(r, actionConsoleAttach, id, nil, nil)
	start := time.Now()

	// The stream must not end with the handler's request context alone;
	// it ends when either side closes or the session ends.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	client.SetReadLimit(maxConsoleMessage)
	agentConn.SetReadLimit(maxConsoleMessage)
	closeCode, closeReason := websocket.StatusNormalClosure, ""

	ended := make(chan string, 2)
	// Browser to guest. Typing is activity: at most once a minute it
	// refreshes the session, which also re-checks it.
	go func() {
		lastTouch := time.Now()
		for {
			_, data, err := client.Read(ctx)
			if err != nil {
				ended <- "client"
				return
			}
			if time.Since(lastTouch) >= consoleActivityInterval {
				if _, err := h.auth.Authenticate(ctx, token, kind); err != nil {
					ended <- "session"
					return
				}
				lastTouch = time.Now()
			}
			wctx, wcancel := context.WithTimeout(ctx, consoleWriteTimeout)
			err = agentConn.Write(wctx, websocket.MessageBinary, data)
			wcancel()
			if err != nil {
				ended <- "agent"
				return
			}
		}
	}()
	// Guest to browser. Output is not activity.
	go func() {
		for {
			_, data, err := agentConn.Read(ctx)
			if err != nil {
				ended <- "agent"
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, consoleWriteTimeout)
			err = client.Write(wctx, websocket.MessageBinary, data)
			wcancel()
			if err != nil {
				ended <- "client"
				return
			}
		}
	}()

	check := time.NewTicker(h.consoleCheck)
	defer check.Stop()
	why := ""
loop:
	for {
		select {
		case why = <-ended:
			break loop
		case <-check.C:
			// The session is checked without counting as use: an open
			// console on an unattended screen idles out like any page.
			if _, err := h.auth.AuthenticateBackground(ctx, token, kind); err != nil {
				why = "session"
				break loop
			}
		}
	}
	switch why {
	case "session":
		closeCode, closeReason = statusSessionEnded, "the session has ended"
	case "agent":
		closeReason = "the console closed"
	}
	_ = client.Close(closeCode, closeReason)
	_ = agentConn.Close(websocket.StatusNormalClosure, "")
	cancel()

	who, _ := identityFrom(r.Context())
	h.writeAudit(r, audit.AccessEntry{
		ActorID: who.User.ID, ActorName: who.User.Username, Action: actionConsoleDetach,
		ResourceType: models.VMResource, ResourceID: id, Outcome: audit.Success,
		Details: map[string]any{
			"duration_seconds": int64(time.Since(start) / time.Second),
			"ended_by":         why,
		},
	})
}
