package console

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

const vmID = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Write(_ context.Context, e audit.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) actions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.entries {
		out = append(out, e.Action+"/"+string(e.Outcome))
	}
	return out
}

// guest is the far end of a simulated console device.
type guest struct{ net.Conn }

func newBroker(t *testing.T) (*Broker, *memAudit, chan guest) {
	t.Helper()
	au := &memAudit{}
	guests := make(chan guest, 4)
	b := New(Options{Audit: au, Open: func(string) (io.ReadWriteCloser, error) {
		agent, g := net.Pipe()
		guests <- guest{g}
		return agent, nil
	}})
	return b, au, guests
}

var who = Who{Actor: models.TaskActor{ID: "u1", Name: "andy"}, PeerUID: 1002, RequestID: "req-1"}

func recv(t *testing.T, s *Session) string {
	t.Helper()
	select {
	case p := <-s.Output():
		return string(p)
	case <-time.After(2 * time.Second):
		t.Fatal("no output")
	}
	return ""
}

func TestScrollbackFanOutAndInput(t *testing.T) {
	b, au, guests := newBroker(t)
	b.Track(vmID, true)
	g := <-guests
	// Output before anyone attaches is kept.
	g.Write([]byte("boot banner\r\n"))
	time.Sleep(20 * time.Millisecond)

	s1, err := b.Attach(context.Background(), vmID, true, who)
	if err != nil {
		t.Fatal(err)
	}
	if string(s1.Scrollback) != "boot banner\r\n" {
		t.Errorf("scrollback = %q", s1.Scrollback)
	}
	s2, _ := b.Attach(context.Background(), vmID, true, who)
	g.Write([]byte("login: "))
	if recv(t, s1) != "login: " || recv(t, s2) != "login: " {
		t.Error("output did not reach both clients")
	}
	// Input from any client reaches the guest.
	go s2.Write([]byte("root\r"))
	buf := make([]byte, 16)
	n, _ := g.Read(buf)
	if string(buf[:n]) != "root\r" {
		t.Errorf("guest read %q", buf[:n])
	}

	// At most four.
	s3, _ := b.Attach(context.Background(), vmID, true, who)
	s4, _ := b.Attach(context.Background(), vmID, true, who)
	if _, err := b.Attach(context.Background(), vmID, true, who); err == nil || err.(*models.Error).Code != models.CodeConsoleBusy {
		t.Fatalf("fifth attachment: %v", err)
	}
	s4.Detach(context.Background())
	s5, err := b.Attach(context.Background(), vmID, true, who)
	if err != nil {
		t.Fatalf("attachment after a detach: %v", err)
	}

	// The VM stops: every session ends, and the device is closed.
	b.Track(vmID, false)
	for _, s := range []*Session{s1, s2, s3, s5} {
		select {
		case <-s.Ended():
		case <-time.After(2 * time.Second):
			t.Fatal("a session outlived its console")
		}
		s.Detach(context.Background())
	}
	if _, err := g.Write([]byte("x")); err == nil {
		t.Error("the device is still open")
	}
	// Not running: refused, and audited.
	if _, err := b.Attach(context.Background(), vmID, false, who); err == nil || err.(*models.Error).Code != models.CodeVMNotRunning {
		t.Errorf("attach to a stopped VM: %v", err)
	}
	got := strings.Join(au.actions(), " ")
	if strings.Count(got, "vm.console.attach/success") != 5 || strings.Count(got, "vm.console.attach/denied") != 2 ||
		strings.Count(got, "vm.console.detach/success") != 5 {
		t.Errorf("audit = %s", got)
	}
}

func TestScrollbackIsBounded(t *testing.T) {
	b, _, guests := newBroker(t)
	b.Track(vmID, true)
	g := <-guests
	chunk := strings.Repeat("x", 4096)
	for i := 0; i < 20; i++ {
		g.Write([]byte(chunk))
	}
	g.Write([]byte("END"))
	time.Sleep(20 * time.Millisecond)
	s, err := b.Attach(context.Background(), vmID, true, who)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Scrollback) != ScrollbackBytes || !strings.HasSuffix(string(s.Scrollback), "END") {
		t.Errorf("scrollback is %d bytes", len(s.Scrollback))
	}
}
