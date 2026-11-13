package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestThrottleLogAggregates(t *testing.T) {
	l := newThrottleLog()
	t0 := time.Date(2026, 11, 12, 12, 0, 5, 0, time.UTC)

	ended, started := l.add("192.0.2.1", "andy", "cookie", LimitAddress, t0)
	if len(ended) != 0 || !started {
		t.Fatalf("first attempt: ended %d, started %v", len(ended), started)
	}
	for i := 1; i < 100; i++ {
		ended, started = l.add("192.0.2.1", "andy", "bearer", LimitUsernameAddress, t0.Add(time.Duration(i)*100*time.Millisecond))
		if len(ended) != 0 || started {
			t.Fatalf("attempt %d in the same minute: ended %d, started %v", i+1, len(ended), started)
		}
	}
	l.add("192.0.2.2", "carol", "cookie", LimitAddress, t0)

	// The minute is not over: nothing is due.
	if due := l.due(t0.Add(30*time.Second), false); len(due) != 0 {
		t.Fatalf("%d buckets due within the minute", len(due))
	}
	// The next attempt from 192.0.2.1 in a new minute hands back the old
	// bucket and starts a new one.
	ended, started = l.add("192.0.2.1", "andy", "cookie", LimitAddress, t0.Add(time.Minute))
	if len(ended) != 1 || !started {
		t.Fatalf("rollover: ended %d, started %v", len(ended), started)
	}
	e := ended[0].entry()
	if e.SourceIP != "192.0.2.1" || e.ActorName != "andy" || e.Details["attempts"] != 100 ||
		fmt.Sprint(e.Details["limits"]) != "[address username_address]" ||
		fmt.Sprint(e.Details["session_kinds"]) != "[bearer cookie]" ||
		e.Details["window_start"] != "2026-11-12T12:00:00Z" || e.Details["aggregated"] != true {
		t.Errorf("entry = %+v", e)
	}
	// After the minute, 192.0.2.2's bucket is due; the new one is not.
	due := l.due(t0.Add(time.Minute), false)
	if len(due) != 1 || due[0].address != "192.0.2.2" {
		t.Fatalf("due = %+v", due)
	}
	if all := l.due(t0.Add(time.Minute), true); len(all) != 1 || all[0].attempts != 1 {
		t.Fatalf("due(all) = %+v", all)
	}
	if left := l.due(t0.Add(time.Hour), true); len(left) != 0 {
		t.Errorf("%d buckets left after flushing everything", len(left))
	}
}

func TestThrottleLogUsernames(t *testing.T) {
	l := newThrottleLog()
	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for i := range 15 {
		l.add("192.0.2.1", fmt.Sprintf("user%02d", i), "cookie", LimitAddress, t0)
	}
	l.add("192.0.2.1", "Not A Username!", "cookie", LimitAddress, t0)
	e := l.due(t0, true)[0].entry()
	names := e.Details["usernames"].([]string)
	if len(names) != maxAggregatedUsernames || e.Details["usernames_omitted"] != true || e.ActorName != ActorMultipleUsernames {
		t.Errorf("usernames %v, omitted %v, actor %q", names, e.Details["usernames_omitted"], e.ActorName)
	}
	// Input that is not a username is never recorded as given.
	l.add("192.0.2.3", "Not A Username!", "cookie", LimitAddress, t0)
	e = l.due(t0, true)[0].entry()
	if e.ActorName != "invalid-username" || fmt.Sprint(e.Details["usernames"]) != "[invalid-username]" {
		t.Errorf("invalid username recorded as %q %v", e.ActorName, e.Details["usernames"])
	}
}

func TestThrottleLogBounded(t *testing.T) {
	l := newThrottleLog()
	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for i := range maxThrottleBuckets + 500 {
		l.add(fmt.Sprintf("2001:db8::%x", i), "andy", "cookie", LimitUsername, t0)
	}
	if len(l.buckets) != maxThrottleBuckets+1 {
		t.Fatalf("%d buckets, want %d plus one overflow bucket", len(l.buckets), maxThrottleBuckets)
	}
	o := l.buckets[overflowAddress].entry()
	if o.Details["overflow"] != true || o.Details["addresses"] != 500 || o.Details["attempts"] != 500 || o.SourceIP != "" {
		t.Errorf("overflow entry = %+v", o)
	}
}
