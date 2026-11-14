package agentclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	"github.com/greenbeanorg/skep/internal/agentproto"
)

// DialConsole opens the serial console of a VM on the agent, for the
// caller in ctx. A refusal by the agent (the VM is not running, the
// console is busy) comes back as the service error the agent sent.
func (c *Client) DialConsole(ctx context.Context, vmID string) (*websocket.Conn, error) {
	caller, ok := agentproto.CallerFrom(ctx)
	if !ok {
		return nil, errors.New("agentclient: the request context carries no caller")
	}
	if err := caller.Validate(); err != nil {
		return nil, fmt.Errorf("agentclient: %w", err)
	}
	hdr := http.Header{}
	caller.SetHeaders(hdr)
	// The host is a placeholder: the transport always dials the socket.
	u := "ws://agent" + agentproto.Prefix + "/vms/" + url.PathEscape(vmID) + "/console/serial"
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: hdr})
	if err == nil {
		return conn, nil
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
			resp.Body.Close()
		}
		return nil, responseError(resp.StatusCode, body)
	}
	if isDialError(err) {
		return nil, fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	return nil, fmt.Errorf("agentclient: open the console: %w", err)
}
