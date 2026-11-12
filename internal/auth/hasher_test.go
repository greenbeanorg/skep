package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func testHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher(MinParams, DefaultHashConcurrency, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHasherHashAndVerify(t *testing.T) {
	h := testHasher(t)
	ctx := context.Background()
	hash, err := h.Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery staple", hash)
	if err != nil || !ok || rehash {
		t.Errorf("right password: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	ok, rehash, err = h.Verify(ctx, "wrong", hash)
	if err != nil || ok || rehash {
		t.Errorf("wrong password: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if _, _, err := h.Verify(ctx, "x", "not a hash"); err == nil {
		t.Error("a malformed stored hash must be an error, not a failed login")
	}
}

func TestRehashWhenParametersWereRaised(t *testing.T) {
	h := testHasher(t)
	ctx := context.Background()
	old, _ := h.Hash(ctx, "correct horse battery staple")

	stronger := Params{MemoryKiB: MinParams.MemoryKiB, Passes: MinParams.Passes + 1, Lanes: 1}
	if err := h.SetParams(stronger); err != nil {
		t.Fatal(err)
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery staple", old)
	if err != nil || !ok || !rehash {
		t.Errorf("old hash, right password: ok=%v rehash=%v err=%v; want a rehash", ok, rehash, err)
	}
	// A wrong password never triggers a rehash.
	if ok, rehash, _ := h.Verify(ctx, "wrong", old); ok || rehash {
		t.Errorf("old hash, wrong password: ok=%v rehash=%v", ok, rehash)
	}
	fresh, _ := h.Hash(ctx, "correct horse battery staple")
	if !strings.Contains(fresh, ",t=3,") {
		t.Errorf("new hash does not use the new parameters: %s", fresh)
	}
	if _, rehash, _ := h.Verify(ctx, "correct horse battery staple", fresh); rehash {
		t.Error("a current hash asked for a rehash")
	}
	if err := h.SetParams(Params{MemoryKiB: 1024, Passes: 1, Lanes: 1}); err == nil {
		t.Error("parameters below the minimum were accepted")
	}
}

// TestDummyHashFollowsTheParameters: the dummy hash, verified for unknown
// users, must always be made with the current parameters, or the response
// time would tell real usernames from unknown ones.
func TestDummyHashFollowsTheParameters(t *testing.T) {
	h := testHasher(t)
	dummyParams := func() Params {
		h.mu.RLock()
		defer h.mu.RUnlock()
		p, _, _, err := parsePHC(h.dummy)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if dummyParams() != MinParams {
		t.Errorf("dummy parameters = %+v, want %+v", dummyParams(), MinParams)
	}
	before := h.dummy
	stronger := Params{MemoryKiB: 2 * MinParams.MemoryKiB, Passes: 3, Lanes: 1}
	if err := h.SetParams(stronger); err != nil {
		t.Fatal(err)
	}
	if dummyParams() != stronger || h.dummy == before {
		t.Errorf("after a parameter change the dummy uses %+v, want %+v", dummyParams(), stronger)
	}
	if err := h.VerifyDummy(context.Background(), "anything at all"); err != nil {
		t.Errorf("VerifyDummy: %v", err)
	}
}

func TestHashConcurrencyIsBounded(t *testing.T) {
	h, err := NewHasher(MinParams, 2, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Take both slots, as two logins in progress would.
	h.slots <- struct{}{}
	h.slots <- struct{}{}

	start := time.Now()
	_, err = h.Hash(context.Background(), "correct horse battery staple")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("third hash: %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond || waited > 2*time.Second {
		t.Errorf("waited %v before refusing, want about 50ms", waited)
	}
	if _, _, err := h.Verify(context.Background(), "x", "y"); !errors.Is(err, ErrBusy) {
		t.Errorf("Verify: %v, want ErrBusy", err)
	}
	if err := h.VerifyDummy(context.Background(), "x"); !errors.Is(err, ErrBusy) {
		t.Errorf("VerifyDummy: %v, want ErrBusy", err)
	}

	// A cancelled request stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Hash(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}

	// A slot frees up: the waiting hash runs.
	go func() {
		time.Sleep(10 * time.Millisecond)
		<-h.slots
	}()
	if _, err := h.Hash(context.Background(), "correct horse battery staple"); err != nil {
		t.Errorf("after a slot was freed: %v", err)
	}
	<-h.slots

	// Never more than two at once, however many ask.
	h2, _ := NewHasher(MinParams, 2, 30*time.Second)
	var mu sync.Mutex
	running, peak := 0, 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h2.acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			h2.release()
		}()
	}
	wg.Wait()
	if peak != 2 {
		t.Errorf("peak concurrency = %d, want 2", peak)
	}
	if _, err := NewHasher(MinParams, 0, time.Second); err == nil {
		t.Error("zero concurrency accepted")
	}
}

func TestPasswordPolicy(t *testing.T) {
	for _, ok := range []string{
		"twelve chars",                 // exactly 12
		"correct horse battery staple", // no composition rules
		"aaaaaaaaaaaa",                 // no complexity rules either
		"пароль-пароль",                // counted in characters, not bytes
		strings.Repeat("x", 256),       // the upper bound
		"            ",                 // spaces are characters
	} {
		if err := ValidatePassword(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":        "",
		"11 chars":     "eleven char",
		"257 chars":    strings.Repeat("x", 257),
		"invalid utf8": "valid-length-\xff\xfe-but-not-utf8",
	} {
		if err := ValidatePassword(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUsernameAndDisplayName(t *testing.T) {
	for _, ok := range []string{"a", "andy", "andy.alexander", "ops-1", "svc_backup", strings.Repeat("a", 32)} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "Andy", "1andy", "-andy", "an dy", "andy\n", "andy@example.org", "local:0", strings.Repeat("a", 33), "änd"} {
		if err := ValidateUsername(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateDisplayName("Andy Alexander (ops)"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"line\nbreak", "tab\there", strings.Repeat("x", 129), "bad\xffutf8"} {
		if err := ValidateDisplayName(bad); err == nil {
			t.Errorf("display name %q accepted", bad)
		}
	}
}

func TestTokens(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if a == b || len(a) != 43 {
		t.Errorf("tokens %q, %q: want two different 43-character values (32 bytes, base64url)", a, b)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("token %q is not URL-safe", a)
	}
	if h := HashToken(a); len(h) != 64 || h == a || h != HashToken(a) || h == HashToken(b) {
		t.Errorf("HashToken = %q", h)
	}
}
