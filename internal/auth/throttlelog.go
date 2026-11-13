package auth

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
)

// Throttled login attempts are not audited one row each: a refused attempt
// costs an unauthenticated client almost nothing, so one row per attempt
// would let it grow the access audit log as fast as it can send requests.
// They are aggregated into one entry per source address per minute, with
// the number of attempts (ADR 0007, amendment b; ADR 0006 implementation
// notes). Everything else about a login is still audited one entry each.

const (
	// throttleWindow is the aggregation period.
	throttleWindow = time.Minute
	// maxAggregatedUsernames bounds the usernames listed in one entry.
	maxAggregatedUsernames = 10
	// maxThrottleBuckets bounds the addresses aggregated at once. Attempts
	// from further addresses share one overflow bucket, so memory stays
	// bounded when an attacker has many addresses.
	maxThrottleBuckets = 4096
	// ThrottleFlushInterval is how often Run writes the entries of
	// minutes that have ended.
	ThrottleFlushInterval = 10 * time.Second
	// ActorMultipleUsernames is the actor name of an aggregated entry
	// whose attempts named more than one username.
	ActorMultipleUsernames = "multiple-usernames"
	// overflowAddress is the bucket key of attempts beyond
	// maxThrottleBuckets addresses.
	overflowAddress = ""
)

// throttleBucket collects the throttled attempts of one source address in
// one minute.
type throttleBucket struct {
	address   string
	window    time.Time // start of the minute
	first     time.Time
	last      time.Time
	attempts  int
	limits    map[string]struct{}
	kinds     map[string]struct{}
	usernames map[string]struct{}
	// usernamesOmitted is set when more than maxAggregatedUsernames
	// distinct usernames were seen.
	usernamesOmitted bool
	// addresses counts distinct addresses in the overflow bucket, up to
	// maxThrottleBuckets.
	addresses map[string]struct{}
}

func newBucket(address string, window, at time.Time) *throttleBucket {
	return &throttleBucket{
		address: address, window: window, first: at,
		limits: map[string]struct{}{}, kinds: map[string]struct{}{}, usernames: map[string]struct{}{},
	}
}

// throttleLog is the aggregator. It is safe for concurrent use.
type throttleLog struct {
	mu      sync.Mutex
	buckets map[string]*throttleBucket
}

func newThrottleLog() *throttleLog { return &throttleLog{buckets: map[string]*throttleBucket{}} }

// add counts one throttled attempt. It returns the buckets of this
// address whose minute has ended (to be written now), and whether this
// attempt started a new bucket.
func (l *throttleLog) add(address, username, kind, limit string, at time.Time) (ended []*throttleBucket, started bool) {
	window := at.Truncate(throttleWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	key := address
	if _, ok := l.buckets[key]; !ok && len(l.buckets) >= maxThrottleBuckets {
		key = overflowAddress
	}
	b := l.buckets[key]
	if b != nil && !b.window.Equal(window) {
		ended = append(ended, b)
		b = nil
	}
	if b == nil {
		b = newBucket(key, window, at)
		if key == overflowAddress {
			b.addresses = map[string]struct{}{}
		}
		l.buckets[key] = b
		started = true
	}
	b.attempts++
	b.last = at
	b.limits[limit] = struct{}{}
	b.kinds[kind] = struct{}{}
	name := auditName(username)
	if _, seen := b.usernames[name]; !seen {
		if len(b.usernames) < maxAggregatedUsernames {
			b.usernames[name] = struct{}{}
		} else {
			b.usernamesOmitted = true
		}
	}
	if b.addresses != nil && len(b.addresses) < maxThrottleBuckets {
		b.addresses[address] = struct{}{}
	}
	return ended, started
}

// due removes and returns the buckets whose minute has ended by now, or
// every bucket when all is set.
func (l *throttleLog) due(now time.Time, all bool) []*throttleBucket {
	window := now.Truncate(throttleWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*throttleBucket
	for k, b := range l.buckets {
		if all || b.window.Before(window) {
			out = append(out, b)
			delete(l.buckets, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].first.Before(out[j].first) })
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// entry is the audit entry of a bucket.
func (b *throttleBucket) entry() audit.AccessEntry {
	usernames := sortedKeys(b.usernames)
	actor := ActorMultipleUsernames
	if len(usernames) == 1 && !b.usernamesOmitted {
		actor = usernames[0]
	}
	details := map[string]any{
		"reason":        CodeRateLimited,
		"aggregated":    true,
		"attempts":      b.attempts,
		"window_start":  b.window.UTC().Format(time.RFC3339),
		"first_attempt": b.first.UTC().Format(time.RFC3339Nano),
		"last_attempt":  b.last.UTC().Format(time.RFC3339Nano),
		"limits":        sortedKeys(b.limits),
		"session_kinds": sortedKeys(b.kinds),
		"usernames":     usernames,
	}
	if b.usernamesOmitted {
		details["usernames_omitted"] = true
	}
	if b.addresses != nil {
		details["overflow"] = true
		details["addresses"] = len(b.addresses)
	}
	return audit.AccessEntry{
		ActorName: actor, Action: ActionLogin, Outcome: audit.Denied, SourceIP: b.address, Details: details,
	}
}

// writeThrottled writes the entries of ended buckets, and logs each.
func (a *Authenticator) writeThrottled(ctx context.Context, buckets []*throttleBucket) {
	for _, b := range buckets {
		e := b.entry()
		if err := a.audit.Write(ctx, e); err != nil {
			a.log.Error("could not write to the access audit log", "error", err,
				"action", e.Action, "source_ip", e.SourceIP, "attempts", b.attempts)
		}
		a.log.Warn("login attempts throttled", "source_ip", b.address, "attempts", b.attempts,
			"window_start", b.window.UTC(), "usernames", e.Details["usernames"], "limits", e.Details["limits"])
	}
}

// FlushThrottled writes the aggregated entries of throttled login attempts
// whose minute has ended; with all set, every pending entry. skep-api
// calls it with all set before it closes the database, so that no counted
// attempt is lost at shutdown.
func (a *Authenticator) FlushThrottled(ctx context.Context, all bool) {
	a.writeThrottled(ctx, a.throttled.due(a.now(), all))
}
