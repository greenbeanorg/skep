// Package console is the agent's serial console broker (ADR 0009
// decision 8b). The agent is the only reader of a VM's console device: it
// opens the device while the VM runs, keeps the last 64 KiB of output, and
// fans it out to at most four attached clients, whose input goes to the
// guest. Every attachment is audited with its actor.
package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Limits of ADR 0009 decision 8b.
const (
	ScrollbackBytes = 64 << 10
	MaxAttachments  = 4
	// outputQueue is how many chunks of output a client may fall behind
	// before it is disconnected: a slow client never holds up the guest.
	outputQueue = 512
)

// Opener opens the agent's side of a VM's console: hostops VMM.OpenConsole.
type Opener func(vmID string) (io.ReadWriteCloser, error)

// Options configures a Broker.
type Options struct {
	Open   Opener
	Audit  audit.Writer
	Logger *slog.Logger
	Now    func() time.Time
}

// Broker holds the consoles of the running VMs.
type Broker struct {
	open  Opener
	audit audit.Writer
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex
	consoles map[string]*console
}

// New returns a Broker.
func New(opts Options) *Broker {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Broker{open: opts.Open, audit: opts.Audit, log: opts.Logger, now: opts.Now, consoles: map[string]*console{}}
}

// console is one open console device.
type console struct {
	vmID string
	dev  io.ReadWriteCloser
	done chan struct{}

	mu     sync.Mutex
	ring   []byte
	subs   map[*Session]struct{}
	closed bool
}

// Track opens the console of a VM that runs and closes the console of one
// that does not. The agent's watcher calls it with what it observed, so
// output is kept from the moment the VM starts, before anyone attaches.
func (b *Broker) Track(vmID string, running bool) {
	b.mu.Lock()
	c := b.consoles[vmID]
	b.mu.Unlock()
	switch {
	case running && c == nil:
		if _, err := b.get(vmID); err != nil {
			b.log.Warn("console: could not open a VM's console", "vm_id", vmID, "error", err)
		}
	case !running && c != nil:
		b.drop(c, "the virtual machine stopped")
	}
}

// get returns the open console of a VM, opening it if needed.
func (b *Broker) get(vmID string) (*console, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.consoles[vmID]; c != nil {
		return c, nil
	}
	dev, err := b.open(vmID)
	if err != nil {
		return nil, err
	}
	c := &console{vmID: vmID, dev: dev, done: make(chan struct{}), subs: map[*Session]struct{}{}}
	b.consoles[vmID] = c
	go b.read(c)
	return c, nil
}

// read copies the device's output into the scrollback and to every
// attached client until the device closes.
func (b *Broker) read(c *console) {
	buf := make([]byte, 4096)
	for {
		n, err := c.dev.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			c.mu.Lock()
			c.ring = append(c.ring, chunk...)
			if over := len(c.ring) - ScrollbackBytes; over > 0 {
				c.ring = append([]byte(nil), c.ring[over:]...)
			}
			for s := range c.subs {
				select {
				case s.out <- chunk:
				default:
					// Too far behind: this client goes, the guest does not
					// wait for it.
					delete(c.subs, s)
					s.end("the client could not keep up with the console output")
				}
			}
			c.mu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				b.log.Debug("console: the device closed", "vm_id", c.vmID, "error", err)
			}
			b.drop(c, "the console closed")
			return
		}
	}
}

// drop closes a console and ends its sessions.
func (b *Broker) drop(c *console, reason string) {
	b.mu.Lock()
	if b.consoles[c.vmID] == c {
		delete(b.consoles, c.vmID)
	}
	b.mu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for s := range c.subs {
		s.end(reason)
	}
	c.subs = nil
	c.mu.Unlock()
	_ = c.dev.Close()
	close(c.done)
}

// Who is the actor of an attachment, as the internal API knows it.
type Who struct {
	Actor     models.TaskActor
	PeerUID   uint32
	RequestID string
	SourceIP  string
}

// Session is one attached client.
type Session struct {
	b     *Broker
	c     *console
	who   Who
	start time.Time
	// Scrollback is the output kept before the attachment.
	Scrollback []byte
	out        chan []byte
	ended      chan struct{}
	endOnce    sync.Once
	reason     string

	mu       sync.Mutex
	bytesIn  int64
	bytesOut int64
}

func (s *Session) end(reason string) {
	s.endOnce.Do(func() {
		s.reason = reason
		close(s.ended)
	})
}

// Output carries the guest's output after the scrollback.
func (s *Session) Output() <-chan []byte { return s.out }

// Ended is closed when the console or the session ends; Reason says why.
func (s *Session) Ended() <-chan struct{} { return s.ended }

// Reason says why the session ended.
func (s *Session) Reason() string { return s.reason }

// CountOutput records output delivered to the client.
func (s *Session) CountOutput(n int) {
	s.mu.Lock()
	s.bytesOut += int64(n)
	s.mu.Unlock()
}

// Write sends input to the guest.
func (s *Session) Write(p []byte) (int, error) {
	select {
	case <-s.ended:
		return 0, errors.New("console: the session has ended")
	default:
	}
	n, err := s.c.dev.Write(p)
	s.mu.Lock()
	s.bytesIn += int64(n)
	s.mu.Unlock()
	return n, err
}

func (b *Broker) writeAudit(ctx context.Context, e audit.Entry) {
	if b.audit == nil {
		return
	}
	if err := b.audit.Write(ctx, e); err != nil {
		b.log.Error("console: could not write the audit entry", "action", e.Action, "error", err)
	}
}

func entry(who Who, vmID, action string, phase audit.Phase, outcome audit.Outcome, details map[string]any) audit.Entry {
	return audit.Entry{
		ActorID: who.Actor.ID, ActorName: who.Actor.Name, PeerUID: who.PeerUID,
		Action: action, ResourceType: models.VMResource, ResourceID: vmID,
		Phase: phase, Outcome: outcome, SourceIP: who.SourceIP, RequestID: who.RequestID, Details: details,
	}
}

// Audit actions of the console.
const (
	ActionAttach = "vm.console.attach"
	ActionDetach = "vm.console.detach"
)

// Attach attaches a client to the console of a running VM. running says
// whether the VM runs, as the caller observed it. A refusal is audited.
func (b *Broker) Attach(ctx context.Context, vmID string, running bool, who Who) (*Session, error) {
	refuse := func(err *models.Error) (*Session, error) {
		b.writeAudit(ctx, entry(who, vmID, ActionAttach, audit.Requested, audit.Denied,
			map[string]any{"error_code": err.Code}))
		return nil, err
	}
	if !running {
		return refuse(&models.Error{Code: models.CodeVMNotRunning,
			Message: "the virtual machine is not running; its console exists only while it runs"})
	}
	c, err := b.get(vmID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return refuse(&models.Error{Code: models.CodeVMNotRunning, Message: "the console has closed"})
	}
	if len(c.subs) >= MaxAttachments {
		c.mu.Unlock()
		return refuse(&models.Error{Code: models.CodeConsoleBusy,
			Message: "the console already has the maximum of 4 attached clients",
			Details: map[string]any{"max_attachments": MaxAttachments}})
	}
	s := &Session{
		b: b, c: c, who: who, start: b.now(),
		Scrollback: append([]byte(nil), c.ring...),
		out:        make(chan []byte, outputQueue), ended: make(chan struct{}),
	}
	c.subs[s] = struct{}{}
	attached := len(c.subs)
	c.mu.Unlock()
	b.writeAudit(ctx, entry(who, vmID, ActionAttach, audit.Requested, audit.Success,
		map[string]any{"attachments": attached}))
	return s, nil
}

// Detach ends a session and audits it with its duration and byte counts.
func (s *Session) Detach(ctx context.Context) {
	c := s.c
	c.mu.Lock()
	if c.subs != nil {
		delete(c.subs, s)
	}
	c.mu.Unlock()
	s.end("detached")
	s.mu.Lock()
	details := map[string]any{
		"duration_seconds": int64(s.b.now().Sub(s.start) / time.Second),
		"bytes_in":         s.bytesIn,
		"bytes_out":        s.bytesOut,
	}
	s.mu.Unlock()
	s.b.writeAudit(ctx, entry(s.who, c.vmID, ActionDetach, audit.Finished, audit.Success, details))
}
