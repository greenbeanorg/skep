package auth

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestThrottle() (*Throttle, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	th := NewThrottle(DefaultThrottleConfig())
	th.now = c.now
	return th, c
}

func TestThrottlePerAddress(t *testing.T) {
	th, c := newTestThrottle()
	// Ten attempts a minute from one address, whatever the usernames.
	for i := range 10 {
		if ok, _, limit := th.Attempt("192.0.2.1", fmt.Sprintf("user%d", i)); !ok {
			t.Fatalf("attempt %d refused by %s", i+1, limit)
		}
		c.advance(time.Second)
	}
	ok, retry, limit := th.Attempt("192.0.2.1", "someone")
	if ok || limit != LimitAddress || retry != 50*time.Second {
		t.Errorf("11th attempt: ok=%v limit=%q retry=%v, want a refusal by the address limit for 50s", ok, limit, retry)
	}
	// Another address is not affected.
	if ok, _, _ := th.Attempt("192.0.2.2", "someone"); !ok {
		t.Error("another address was refused")
	}
	// Refused attempts do not extend the wait.
	c.advance(50 * time.Second)
	if ok, _, limit := th.Attempt("192.0.2.1", "someone"); !ok {
		t.Errorf("after the window drained: refused by %s", limit)
	}
}

func TestThrottleBackoffPerUsernameAndAddress(t *testing.T) {
	th, c := newTestThrottle()
	const addr, user = "192.0.2.1", "andy"
	attempt := func() (bool, time.Duration, string) {
		// Stay under the per-address limit: this test is about the delay.
		c.advance(7 * time.Second)
		return th.Attempt(addr, user)
	}

	// Four failures: no delay yet.
	for i := range 4 {
		if ok, _, limit := attempt(); !ok {
			t.Fatalf("attempt %d refused by %s", i+1, limit)
		}
		th.Failure(addr, user)
	}
	if ok, _, _ := attempt(); !ok {
		t.Fatal("the fifth attempt must still be allowed")
	}
	// The fifth failure starts the delay: 1s, 2s, 4s, ... capped at 60s.
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		th.Failure(addr, user)
		ok, retry, limit := th.Attempt(addr, user)
		if ok || limit != LimitUsernameAddress || retry != w*time.Second {
			t.Fatalf("after failure %d: ok=%v limit=%q retry=%v, want a delay of %ds", 5+i, ok, limit, retry, w)
		}
		// Someone guessing from here does not slow the same user down
		// from another address...
		if ok, _, limit := th.Attempt("198.51.100.7", user); !ok {
			t.Fatalf("the user's own address was delayed (%s)", limit)
		}
		// ...nor another user from this address.
		if ok, _, limit := th.Attempt(addr, "bob"); !ok {
			t.Fatalf("another username from this address was delayed (%s)", limit)
		}
		c.advance(w * time.Second)
		// Keep the per-address window from interfering.
		c.advance(time.Minute)
		if ok, _, limit := th.Attempt(addr, user); !ok {
			t.Fatalf("after waiting %ds: still refused by %s", w, limit)
		}
	}

	// A success clears the count.
	th.Success(addr, user)
	c.advance(time.Minute)
	th.Failure(addr, user)
	if ok, _, limit := th.Attempt(addr, user); !ok {
		t.Errorf("one failure after a success: refused by %s", limit)
	}

	// So do fifteen minutes without attempts.
	for range 6 {
		th.Failure(addr, user)
	}
	if ok, _, _ := th.Attempt(addr, user); ok {
		t.Fatal("expected a delay")
	}
	c.advance(15 * time.Minute)
	if ok, _, limit := th.Attempt(addr, user); !ok {
		t.Errorf("after 15 minutes of silence: refused by %s", limit)
	}
	th.Failure(addr, user)
	c.advance(7 * time.Second)
	if ok, _, limit := th.Attempt(addr, user); !ok {
		t.Errorf("the failure count was not forgotten: refused by %s", limit)
	}
}

func TestThrottleUsernameCeilingAcrossAddresses(t *testing.T) {
	th, c := newTestThrottle()
	// Fifty failures for one username, each from a different address:
	// the per-address key never triggers.
	for i := range 50 {
		addr := fmt.Sprintf("203.0.113.%d", i)
		if ok, _, limit := th.Attempt(addr, "andy"); !ok {
			t.Fatalf("attempt %d refused by %s", i+1, limit)
		}
		th.Failure(addr, "andy")
		c.advance(time.Second)
	}
	ok, retry, limit := th.Attempt("198.51.100.99", "andy")
	if ok || limit != LimitUsername {
		t.Fatalf("51st attempt from a new address: ok=%v limit=%q, want the per-username ceiling", ok, limit)
	}
	if retry <= 14*time.Minute || retry > 15*time.Minute {
		t.Errorf("retry after = %v, want until the oldest failure leaves the 15 minute window", retry)
	}
	// It is a throttle on one username, not a lock on the system.
	if ok, _, limit := th.Attempt("198.51.100.99", "bob"); !ok {
		t.Errorf("another username was refused by %s", limit)
	}
	// It drains by itself.
	c.advance(retry)
	if ok, _, limit := th.Attempt("198.51.100.100", "andy"); !ok {
		t.Errorf("after the window drained: refused by %s", limit)
	}
}

func TestThrottleGarbageUsernamesShareOneBucket(t *testing.T) {
	th, _ := newTestThrottle()
	for i := range 20 {
		addr := fmt.Sprintf("203.0.113.%d", i)
		th.Attempt(addr, fmt.Sprintf("Not A Username %d\n", i))
		th.Failure(addr, fmt.Sprintf("Not A Username %d\n", i))
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.usernames) != 1 {
		t.Errorf("garbage usernames created %d entries, want 1", len(th.usernames))
	}
}

func TestThrottleStaysBounded(t *testing.T) {
	th, c := newTestThrottle()
	for i := range maxThrottleEntries + 500 {
		addr := fmt.Sprintf("2001:db8::%x", i)
		th.Attempt(addr, "andy")
		th.Failure(addr, fmt.Sprintf("u%d", i))
		if i%1000 == 0 {
			c.advance(time.Millisecond)
		}
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.addresses) > maxThrottleEntries || len(th.pairs) > maxThrottleEntries || len(th.usernames) > maxThrottleEntries {
		t.Errorf("maps grew to %d / %d / %d entries, limit %d", len(th.addresses), len(th.pairs), len(th.usernames), maxThrottleEntries)
	}
}
