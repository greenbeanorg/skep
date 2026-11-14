package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/pkg/models"
)

// dialConsole opens a VM's console through the public API as a role.
func dialConsole(t *testing.T, srv *httptest.Server, token, vmID, requestID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	hdr := http.Header{"Authorization": {"Bearer " + token}}
	if requestID != "" {
		hdr.Set("X-Request-ID", requestID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/vms/" + vmID + "/console/serial"
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
}

func readUntil(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read %q so far, waiting for %q: %v", got.String(), want, err)
		}
		got.Write(data)
	}
	return got.String()
}

// TestConsoleEndToEnd: the serial console of a running VM, from a
// WebSocket client through skep-api and the agent socket to the console
// device, with both audit logs, the attachment limit, and a console that
// closes when its session ends.
func TestConsoleEndToEnd(t *testing.T) {
	s := newStack(t)
	srv := httptest.NewServer(s.handler)
	t.Cleanup(srv.Close)

	create := []byte(`{"name":"c1","cpu_count":1,"memory":"512M","disks":[{"size":"1G","pool":"devpool","thin":true}],"nics":[{"bridge":"bridge0"}]}`)
	rec := s.call(http.MethodPost, "/api/v1/vms", auth.RoleOperator, create, "")
	var created models.VMCreated
	json.Unmarshal(rec.Body.Bytes(), &created)
	s.waitStatus(t, created.Task.ID, models.TaskSucceeded)
	id := created.VM.ID

	// Not running: refused before any WebSocket.
	if _, resp, err := dialConsole(t, srv, s.tokens[auth.RoleOperator], id, ""); err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("console of a stopped VM: %v %v", err, resp)
	}
	rec = s.call(http.MethodPost, "/api/v1/vms/"+id+"/actions", auth.RoleOperator, []byte(`{"action":"start"}`), "")
	var task models.Task
	json.Unmarshal(rec.Body.Bytes(), &task)
	s.waitStatus(t, task.ID, models.TaskSucceeded)

	// A viewer may not attach.
	if _, resp, err := dialConsole(t, srv, s.tokens[auth.RoleViewer], id, ""); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("console as viewer: %v %v", err, resp)
	}

	c, _, err := dialConsole(t, srv, s.tokens[auth.RoleOperator], id, "req-console")
	if err != nil {
		t.Fatal(err)
	}
	// The scrollback holds the guest's output from before the attachment.
	readUntil(t, c, "simulated serial console of VM "+id)
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("hello guest")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "hello guest")

	// Four at most: three more fit, a fifth is refused.
	var extra []*websocket.Conn
	for i := 0; i < 3; i++ {
		x, _, err := dialConsole(t, srv, s.tokens[auth.RoleAdmin], id, "")
		if err != nil {
			t.Fatalf("attachment %d: %v", i+2, err)
		}
		extra = append(extra, x)
	}
	_, resp, err := dialConsole(t, srv, s.tokens[auth.RoleAdmin], id, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("fifth attachment: %v %v", err, resp)
	}
	for _, x := range extra {
		x.Close(websocket.StatusNormalClosure, "")
	}

	// Logging out elsewhere closes the console within the session check.
	if rec := s.call(http.MethodPost, "/api/v1/auth/logout", auth.RoleOperator, nil, ""); rec.Code/100 != 2 {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != 4001 {
				t.Errorf("the console closed with %v, want 4001", err)
			}
			break
		}
	}

	// Both logs, linked by the request ID.
	deadline := time.Now().Add(5 * time.Second)
	for {
		agentN := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action IN ('vm.console.attach','vm.console.detach')
			AND request_id = 'req-console' AND resource_id = ?`, id)
		apiN := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action IN ('vm.console.attach','vm.console.detach')
			AND request_id = 'req-console' AND resource_id = ?`, id)
		if agentN == 2 && apiN == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console audit entries: agent %d, api %d; want 2 each", agentN, apiN)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.console.attach' AND outcome = 'denied'`); n < 2 {
		t.Errorf("%d denied attachments in the agent's log, want the stopped VM and the fifth", n)
	}
	if n := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'authz.deny' AND details LIKE '%vm:console%'`); n != 1 {
		t.Errorf("%d console denials in the access log, want the viewer's", n)
	}
}

// TestConsoleOutlivesServerDeadlines: a console stays open longer than the
// per-request deadlines of both servers, skep-api's and the agent's.
func TestConsoleOutlivesServerDeadlines(t *testing.T) {
	s := newStack(t)
	srv := httptest.NewUnstartedServer(s.handler)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = time.Second, time.Second
	srv.Start()
	t.Cleanup(srv.Close)

	create := []byte(`{"name":"long","cpu_count":1,"memory":"256M","disks":[{"size":"1G","pool":"devpool","thin":true}]}`)
	rec := s.call(http.MethodPost, "/api/v1/vms", auth.RoleOperator, create, "")
	var created models.VMCreated
	json.Unmarshal(rec.Body.Bytes(), &created)
	s.waitStatus(t, created.Task.ID, models.TaskSucceeded)
	rec = s.call(http.MethodPost, "/api/v1/vms/"+created.VM.ID+"/actions", auth.RoleOperator, []byte(`{"action":"start"}`), "")
	var task models.Task
	json.Unmarshal(rec.Body.Bytes(), &task)
	s.waitStatus(t, task.ID, models.TaskSucceeded)

	c, _, err := dialConsole(t, srv, s.tokens[auth.RoleOperator], created.VM.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "simulated serial console")
	// Longer than skep-api's 1 s deadlines and the agent server's (see
	// newStack: agentServerTimeout).
	time.Sleep(agentServerTimeout + time.Second)
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("still here")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, "still here")
}
