package forward

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
)

// Timings of the peers copy (D14).
const (
	// PeersMaxAge is how old the last answer may be before no peer is
	// usable.
	PeersMaxAge = 15 * time.Second
	// peersWait is peers-read's wait.
	peersWait = 5 * time.Second
	// peersPause is the pause after a failed read.
	peersPause = time.Second
)

// PeersSource is skep-cluster's peers-read (peerapi.Client).
type PeersSource interface {
	Peers(ctx context.Context, after uint64, wait time.Duration) (peerapi.PeersAnswer, error)
}

// Peers keeps the last peers-read answer and its time (D14). An answer
// older than PeersMaxAge, an error, or no answer yet means no usable peer:
// nothing is forwarded and the node listener accepts nobody.
type Peers struct {
	src PeersSource
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last time.Time
	ans  peerapi.PeersAnswer
	err  error
	// changed is closed and replaced after every answer.
	changed chan struct{}
}

// NewPeers returns the copy. Run keeps it.
func NewPeers(src PeersSource, log *slog.Logger, now func() time.Time) *Peers {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Peers{src: src, log: log, now: now, changed: make(chan struct{})}
}

// Run long-polls peers-read until ctx ends.
func (p *Peers) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := p.Sync(ctx); err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(peersPause):
			}
		}
	}
}

// Sync reads the peers once, waiting up to 5 s for a change of the
// membership index (no wait before the first answer or after an error).
func (p *Peers) Sync(ctx context.Context) error {
	p.mu.Lock()
	after, wait := p.ans.MembershipIndex, peersWait
	if p.last.IsZero() || p.err != nil {
		wait = 0
	}
	p.mu.Unlock()
	a, err := p.src.Peers(ctx, after, wait)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if p.err == nil {
			p.log.Warn("cannot read the peers from skep-cluster; nothing is forwarded and no node is accepted", "error", err)
		}
		p.err = err
		return err
	}
	if p.err != nil {
		p.log.Info("skep-cluster reports the peers again")
	}
	p.err, p.last, p.ans = nil, p.now(), a
	close(p.changed)
	p.changed = make(chan struct{})
	return nil
}

// Changed returns a channel closed at the next answer.
func (p *Peers) Changed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// Current returns the last answer and whether it may be used: it came
// less than PeersMaxAge ago and no read has failed since. The answer is
// returned either way (for the listen address and health).
func (p *Peers) Current() (peerapi.PeersAnswer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fresh := !p.last.IsZero() && p.err == nil && p.now().Sub(p.last) <= PeersMaxAge
	return p.ans, fresh
}

// Problem says why the peer list may not be used, or "".
func (p *Peers) Problem() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.err != nil:
		return "skep-cluster's peer list cannot be read: " + p.err.Error()
	case p.last.IsZero():
		return "no peer list from skep-cluster yet"
	case p.now().Sub(p.last) > PeersMaxAge:
		return "the peer list is older than 15 s"
	}
	return ""
}

// Usable returns the usable peers (D13) of a fresh answer; none when the
// answer may not be used.
func (p *Peers) Usable() []peerapi.Peer {
	a, fresh := p.Current()
	if !fresh {
		return nil
	}
	var out []peerapi.Peer
	for _, x := range a.Peers {
		if peerapi.Usable(x, a.NodeID) {
			out = append(out, x)
		}
	}
	return out
}

// ByAPIPin returns the usable peer whose API pin is pin.
func (p *Peers) ByAPIPin(pin string) (peerapi.Peer, bool) {
	if pin == "" {
		return peerapi.Peer{}, false
	}
	for _, x := range p.Usable() {
		if x.APIPin == pin {
			return x, true
		}
	}
	return peerapi.Peer{}, false
}

// AcceptPin is the node listener's handshake check (D17): the key is the
// API pin of a usable peer now.
func (p *Peers) AcceptPin(pin string) bool {
	_, ok := p.ByAPIPin(pin)
	return ok
}
