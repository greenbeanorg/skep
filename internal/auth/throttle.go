package auth

import (
	"sync"
	"time"
)

// ThrottleConfig holds the login limits (ADR 0006 decision 4). They slow
// guessing down and never lock an account: nothing is written to a user,
// existing sessions keep working, and the local admin command is
// unaffected.
type ThrottleConfig struct {
	// PerAddress is the number of login attempts one source address may
	// make per AddressWindow, whatever the username and the outcome.
	PerAddress    int
	AddressWindow time.Duration
	// After FailuresBeforeDelay failures from one address for one
	// username, each further attempt from that address for that username
	// must wait: BaseDelay, doubling with every failure up to MaxDelay.
	// The count is forgotten after a success or after FailureMemory
	// without attempts.
	FailuresBeforeDelay int
	BaseDelay           time.Duration
	MaxDelay            time.Duration
	FailureMemory       time.Duration
	// PerUsername is the number of failures for one username, from all
	// addresses together, per UsernameWindow. Above it, logins for that
	// username are refused until the window has drained. It bounds a
	// guessing attack spread over many addresses.
	PerUsername    int
	UsernameWindow time.Duration
}

// DefaultThrottleConfig returns the defaults of ADR 0006.
func DefaultThrottleConfig() ThrottleConfig {
	return ThrottleConfig{
		PerAddress:          10,
		AddressWindow:       time.Minute,
		FailuresBeforeDelay: 5,
		BaseDelay:           time.Second,
		MaxDelay:            time.Minute,
		FailureMemory:       15 * time.Minute,
		PerUsername:         50,
		UsernameWindow:      15 * time.Minute,
	}
}

// Limits that can refuse a login attempt.
const (
	LimitAddress         = "address"
	LimitUsernameAddress = "username_address"
	LimitUsername        = "username"
)

// maxThrottleEntries bounds each of the throttle's maps. When one is full
// its expired entries are dropped, and if that is not enough, arbitrary
// ones: under a flood from very many addresses the throttle forgets rather
// than grows.
const maxThrottleEntries = 50000

// invalidUsernameKey is the bucket of every submitted name that does not
// have the shape of a username, so that garbage cannot fill the maps.
const invalidUsernameKey = "\x00invalid"

type pairState struct {
	failures int
	last     time.Time
	// next is when the next attempt is allowed.
	next time.Time
}

// Throttle is the in-memory login rate limiter. Its state is lost at a
// restart, which is acceptable.
type Throttle struct {
	cfg ThrottleConfig
	now func() time.Time

	mu        sync.Mutex
	addresses map[string][]time.Time
	pairs     map[string]*pairState
	usernames map[string][]time.Time
}

// NewThrottle returns a Throttle with the given limits.
func NewThrottle(cfg ThrottleConfig) *Throttle {
	return &Throttle{
		cfg:       cfg,
		now:       time.Now,
		addresses: map[string][]time.Time{},
		pairs:     map[string]*pairState{},
		usernames: map[string][]time.Time{},
	}
}

func usernameKey(username string) string {
	if !ValidUsername(username) {
		return invalidUsernameKey
	}
	return username
}

func pairKey(address, username string) string { return usernameKey(username) + "\x00" + address }

// recent drops the times older than window.
func recent(times []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(times) && now.Sub(times[i]) >= window {
		i++
	}
	return times[i:]
}

// Attempt decides whether a login attempt may proceed, and counts it
// against the address. When it may not, it says how long to wait and which
// limit refused it.
func (t *Throttle) Attempt(address, username string) (ok bool, retryAfter time.Duration, limit string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	// Per address: every attempt that is let through counts, whatever
	// its outcome.
	attempts := recent(t.addresses[address], now, t.cfg.AddressWindow)
	if len(attempts) >= t.cfg.PerAddress {
		t.addresses[address] = attempts
		return false, attempts[0].Add(t.cfg.AddressWindow).Sub(now), LimitAddress
	}
	if _, known := t.addresses[address]; !known {
		t.makeRoom(now)
	}
	t.addresses[address] = append(attempts, now)

	// Per username and address: a growing delay after repeated failures.
	if p := t.pairs[pairKey(address, username)]; p != nil {
		if now.Sub(p.last) >= t.cfg.FailureMemory {
			delete(t.pairs, pairKey(address, username))
		} else if now.Before(p.next) {
			return false, p.next.Sub(now), LimitUsernameAddress
		}
	}

	// Per username across all addresses: a ceiling on failures.
	key := usernameKey(username)
	failures := recent(t.usernames[key], now, t.cfg.UsernameWindow)
	if len(failures) == 0 {
		delete(t.usernames, key)
	} else {
		t.usernames[key] = failures
	}
	if len(failures) >= t.cfg.PerUsername {
		return false, failures[0].Add(t.cfg.UsernameWindow).Sub(now), LimitUsername
	}
	return true, 0, ""
}

// Failure records a failed login.
func (t *Throttle) Failure(address, username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	pk := pairKey(address, username)
	p := t.pairs[pk]
	if p == nil || now.Sub(p.last) >= t.cfg.FailureMemory {
		if p == nil {
			t.makeRoom(now)
		}
		p = &pairState{}
		t.pairs[pk] = p
	}
	p.failures++
	p.last = now
	if over := p.failures - t.cfg.FailuresBeforeDelay; over >= 0 {
		delay := t.cfg.MaxDelay
		// BaseDelay doubles with each failure; stop shifting once the
		// cap is certain to be reached.
		if over < 16 {
			if d := t.cfg.BaseDelay << uint(over); d < t.cfg.MaxDelay {
				delay = d
			}
		}
		p.next = now.Add(delay)
	}

	key := usernameKey(username)
	if _, known := t.usernames[key]; !known {
		t.makeRoom(now)
	}
	t.usernames[key] = append(recent(t.usernames[key], now, t.cfg.UsernameWindow), now)
}

// Success forgets the failures of this address for this username. The
// per-username ceiling is not reset: one success does not end an attack
// from elsewhere.
func (t *Throttle) Success(address, username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pairs, pairKey(address, username))
}

// makeRoom keeps the maps bounded. The caller holds t.mu.
func (t *Throttle) makeRoom(now time.Time) {
	if len(t.addresses) >= maxThrottleEntries {
		for k, v := range t.addresses {
			if len(recent(v, now, t.cfg.AddressWindow)) == 0 {
				delete(t.addresses, k)
			}
		}
		for k := range t.addresses {
			if len(t.addresses) < maxThrottleEntries {
				break
			}
			delete(t.addresses, k)
		}
	}
	if len(t.pairs) >= maxThrottleEntries {
		for k, p := range t.pairs {
			if now.Sub(p.last) >= t.cfg.FailureMemory {
				delete(t.pairs, k)
			}
		}
		for k := range t.pairs {
			if len(t.pairs) < maxThrottleEntries {
				break
			}
			delete(t.pairs, k)
		}
	}
	if len(t.usernames) >= maxThrottleEntries {
		for k, v := range t.usernames {
			if len(recent(v, now, t.cfg.UsernameWindow)) == 0 {
				delete(t.usernames, k)
			}
		}
		for k := range t.usernames {
			if len(t.usernames) < maxThrottleEntries {
				break
			}
			delete(t.usernames, k)
		}
	}
}
