package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/pkg/models"
)

func loginReq(username, password string, kind auth.SessionKind) auth.LoginRequest {
	return auth.LoginRequest{
		Username: username, Password: password, Kind: kind,
		SourceIP: "192.0.2.50", UserAgent: "skep-test/1.0", RequestID: "req-login",
	}
}

func (f *fixture) login(t *testing.T, username string, kind auth.SessionKind) auth.LoginResult {
	t.Helper()
	res, err := f.authn.Login(context.Background(), loginReq(username, goodPassword, kind))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLoginRequiresAnAdministrator(t *testing.T) {
	f := newFixture(t)
	_, err := f.authn.Login(context.Background(), loginReq("andy", goodPassword, auth.CookieSession))
	if !errors.Is(err, auth.ErrSetupRequired) {
		t.Fatalf("error = %v, want setup_required", err)
	}
	var me *models.Error
	if !errors.As(err, &me) || !strings.Contains(me.Message, "skep-api admin create") {
		t.Errorf("the message must name the command: %v", err)
	}
	// A viewer alone is not enough.
	f.create(t, "bob", auth.RoleViewer)
	if _, err := f.authn.Login(context.Background(), loginReq("bob", goodPassword, auth.CookieSession)); !errors.Is(err, auth.ErrSetupRequired) {
		t.Errorf("with only a viewer: %v", err)
	}
}

func TestLoginCreatesASession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	f.audit.reset()

	for _, kind := range []auth.SessionKind{auth.CookieSession, auth.BearerSession} {
		res := f.login(t, "andy", kind)
		if len(res.Token) != 43 || res.User.ID != u.ID || res.Session.Kind != kind || res.Session.UserID != u.ID {
			t.Fatalf("%s login = %+v", kind, res)
		}
		if got := res.Session.ExpiresAt.Sub(res.Session.CreatedAt); got != 12*time.Hour {
			t.Errorf("absolute lifetime = %v, want 12h", got)
		}
		// The database holds the hash of the token, never the token.
		var stored, kindStored, ip, ua string
		if err := f.db.QueryRow(`SELECT token_hash, kind, source_ip, user_agent FROM sessions WHERE token_hash = ?`,
			auth.HashToken(res.Token)).Scan(&stored, &kindStored, &ip, &ua); err != nil {
			t.Fatal(err)
		}
		if stored == res.Token || len(stored) != 64 || kindStored != string(kind) || ip != "192.0.2.50" || ua != "skep-test/1.0" {
			t.Errorf("stored session: %q %q %q %q", stored, kindStored, ip, ua)
		}

		// The token is accepted only through its own channel.
		id, err := f.authn.Authenticate(ctx, res.Token, kind)
		if err != nil || id.User.ID != u.ID || id.Session.TokenHash != stored {
			t.Errorf("%s token through its own channel: %+v, %v", kind, id, err)
		}
		other := auth.BearerSession
		if kind == auth.BearerSession {
			other = auth.CookieSession
		}
		if _, err := f.authn.Authenticate(ctx, res.Token, other); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s token through the %s channel: %v, want unauthenticated", kind, other, err)
		}
	}
	// Two logins, two different sessions.
	if n := f.sessions(t, u.ID); n != 2 {
		t.Errorf("sessions = %d, want 2", n)
	}

	logins := f.audit.find(auth.ActionLogin)
	if len(logins) != 2 {
		t.Fatalf("login audit = %+v", logins)
	}
	for i, kind := range []string{"cookie", "bearer"} {
		e := logins[i]
		if e.Outcome != audit.Success || e.ActorID != u.ID || e.ActorName != "andy" || e.SourceIP != "192.0.2.50" ||
			e.RequestID != "req-login" || e.Details["session_kind"] != kind {
			t.Errorf("login entry = %+v", e)
		}
	}
	// Neither the token nor its hash is ever audited.
	all, _ := f.db.Query(`SELECT token_hash FROM sessions`)
	defer all.Close()
	for all.Next() {
		var h string
		all.Scan(&h)
		for _, e := range f.audit.entries {
			for _, v := range e.Details {
				if s, ok := v.(string); ok && strings.Contains(s, h) {
					t.Errorf("the audit log contains a token hash: %+v", e)
				}
			}
		}
	}
	after, _ := f.users.ByID(ctx, u.ID)
	if after.LastLoginAt == nil {
		t.Error("the login time was not recorded")
	}
	if _, err := f.authn.Login(ctx, loginReq("andy", goodPassword, "jwt")); err == nil {
		t.Error("an unknown session kind was accepted")
	}
}

func TestFailedLogins(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	disabled := f.create(t, "bob", auth.RoleViewer)
	yes := true
	if _, err := f.users.Update(ctx, admin, disabled.ID, auth.UserUpdate{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	f.audit.reset()

	for name, tt := range map[string]struct {
		username, password string
		wantActor, wantID  string
	}{
		"wrong password":      {"andy", "not the right password", "andy", u.ID},
		"unknown user":        {"carol", goodPassword, "carol", ""},
		"disabled user":       {"bob", goodPassword, "bob", disabled.ID},
		"not a username":      {"Robert'); DROP TABLE users;--", goodPassword, "invalid-username", ""},
		"empty":               {"", "", "invalid-username", ""},
		"enormous password":   {"andy", strings.Repeat("x", 100000), "andy", ""},
		"newline in username": {"andy\nfake-log-line", goodPassword, "invalid-username", ""},
	} {
		f.audit.reset()
		// A different address per case, to stay clear of the limits.
		req := loginReq(tt.username, tt.password, auth.CookieSession)
		req.SourceIP = "192.0.2." + string(rune('1'+len(name)%9))
		_, err := f.authn.Login(ctx, req)
		// Every failure looks the same from outside.
		if err != auth.ErrInvalidCredentials {
			t.Errorf("%s: error = %v, want exactly ErrInvalidCredentials", name, err)
		}
		entries := f.audit.find(auth.ActionLogin)
		if len(entries) != 1 || entries[0].Outcome != audit.Failure || entries[0].ActorName != tt.wantActor ||
			entries[0].ActorID != tt.wantID || entries[0].SourceIP != req.SourceIP {
			t.Errorf("%s: audit = %+v", name, entries)
		}
	}
	if n := f.sessions(t, u.ID) + f.sessions(t, disabled.ID); n != 0 {
		t.Errorf("a failed login created %d session(s)", n)
	}
}

// TestUnknownUserTakesAsLongAsARealOne: a login for an unknown or disabled
// user verifies a dummy hash, so its duration says nothing about whether
// the username exists.
func TestUnknownUserTakesAsLongAsARealOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)

	measure := func(username, ip string) time.Duration {
		best := time.Hour
		for range 3 {
			req := loginReq(username, "not the right password", auth.CookieSession)
			req.SourceIP = ip
			start := time.Now()
			f.authn.Login(ctx, req)
			best = min(best, time.Since(start))
		}
		return best
	}
	known := measure("andy", "192.0.2.1")
	unknown := measure("nobody", "192.0.2.2")
	ratio := float64(unknown) / float64(known)
	if ratio < 0.5 || ratio > 2 {
		t.Errorf("failed login for a real user took %v, for an unknown one %v (ratio %.2f); they must be alike", known, unknown, ratio)
	}
}

func TestSessionLifetime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	lastSeen := func(token string) string {
		var s string
		f.db.QueryRow(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, auth.HashToken(token)).Scan(&s)
		return s
	}

	// Idle: 30 minutes without use end the session.
	res := f.login(t, "andy", auth.CookieSession)
	first := lastSeen(res.Token)
	f.clock.advance(30 * time.Second)
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession); err != nil {
		t.Fatal(err)
	}
	if lastSeen(res.Token) != first {
		t.Error("last_seen_at was written within a minute of the previous write")
	}
	f.clock.advance(29 * time.Minute)
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession); err != nil {
		t.Fatalf("29.5 minutes after the last write, still within the idle timeout: %v", err)
	}
	if lastSeen(res.Token) == first {
		t.Error("last_seen_at was not written after more than a minute")
	}
	f.clock.advance(30 * time.Minute)
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("after 30 idle minutes: %v", err)
	}

	// Background requests are checked, idle timeout included, but do not
	// count as use (ADR 0008, decision 3).
	res = f.login(t, "andy", auth.CookieSession)
	first = lastSeen(res.Token)
	f.clock.advance(20 * time.Minute)
	if id, err := f.authn.AuthenticateBackground(ctx, res.Token, auth.CookieSession); err != nil || id.User.ID != u.ID {
		t.Fatalf("background request within the idle timeout: %+v, %v", id, err)
	}
	if lastSeen(res.Token) != first {
		t.Error("a background request extended the idle timeout")
	}
	if _, err := f.authn.AuthenticateBackground(ctx, res.Token, auth.BearerSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("background request through the wrong channel: %v", err)
	}
	f.clock.advance(10 * time.Minute)
	if _, err := f.authn.AuthenticateBackground(ctx, res.Token, auth.CookieSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("background request after 30 idle minutes: %v, want unauthenticated", err)
	}
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("an ordinary request after the session idled out: %v", err)
	}

	// Absolute: 12 hours after the login, however busy the session was.
	res = f.login(t, "andy", auth.BearerSession)
	for range 47 {
		f.clock.advance(15 * time.Minute)
		if _, err := f.authn.Authenticate(ctx, res.Token, auth.BearerSession); err != nil {
			t.Fatalf("an active session ended early: %v", err)
		}
	}
	f.clock.advance(15 * time.Minute)
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.BearerSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("12 hours after the login: %v", err)
	}

	// The sweep removes all three expired sessions, and only those.
	live := f.login(t, "andy", auth.CookieSession)
	f.audit.reset()
	n, err := f.authn.Sweep(ctx)
	if err != nil || n != 3 || f.sessions(t, u.ID) != 1 {
		t.Errorf("sweep removed %d (%v); %d session(s) left, want 3 removed and 1 left", n, err, f.sessions(t, u.ID))
	}
	if _, err := f.authn.Authenticate(ctx, live.Token, auth.CookieSession); err != nil {
		t.Errorf("the live session did not survive the sweep: %v", err)
	}
	// Expiry is not a revocation and is not audited per session.
	if len(f.audit.entries) != 0 {
		t.Errorf("the sweep wrote audit entries: %+v", f.audit.entries)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)
	res := f.login(t, "andy", auth.CookieSession)

	other, _ := auth.NewToken()
	for name, token := range map[string]string{
		"empty":          "",
		"unknown token":  other,
		"the token hash": auth.HashToken(res.Token),
		"truncated":      res.Token[:42],
		"with a suffix":  res.Token + "x",
	} {
		if _, err := f.authn.Authenticate(ctx, token, auth.CookieSession); err != auth.ErrUnauthenticated {
			t.Errorf("%s: %v, want exactly ErrUnauthenticated", name, err)
		}
	}
}

func TestLogoutAndRevocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	b := f.create(t, "bob", auth.RoleOperator)

	res := f.login(t, "andy", auth.CookieSession)
	second := f.login(t, "andy", auth.BearerSession)
	id, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession)
	if err != nil {
		t.Fatal(err)
	}
	f.audit.reset()
	if err := f.authn.Logout(ctx, id, "192.0.2.50", "req-out"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.CookieSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("the token works after logout: %v", err)
	}
	// Logout ends one session, not the user's others.
	if _, err := f.authn.Authenticate(ctx, second.Token, auth.BearerSession); err != nil {
		t.Errorf("logout ended another session: %v", err)
	}
	if out := f.audit.find(auth.ActionLogout); len(out) != 1 || out[0].ActorID != u.ID || out[0].Details["session_kind"] != "cookie" ||
		out[0].RequestID != "req-out" {
		t.Errorf("logout audit = %+v", out)
	}

	// A role change takes effect at the next request, and ends the
	// sessions.
	bob := f.login(t, "bob", auth.BearerSession)
	viewer := auth.RoleViewer
	if _, err := f.users.Update(ctx, admin, b.ID, auth.UserUpdate{RoleID: &viewer}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authn.Authenticate(ctx, bob.Token, auth.BearerSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("a session survived a role change: %v", err)
	}
	bob = f.login(t, "bob", auth.BearerSession)
	if id, err := f.authn.Authenticate(ctx, bob.Token, auth.BearerSession); err != nil || id.User.Role.ID != auth.RoleViewer {
		t.Errorf("the new role is not what the next session sees: %+v, %v", id.User.Role, err)
	}
	// So does a password change.
	if err := f.users.SetPassword(ctx, admin, b.ID, "another long passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authn.Authenticate(ctx, bob.Token, auth.BearerSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("a session survived a password change: %v", err)
	}
	// A disabled user's token is refused even if a session row existed.
	yes := true
	f.addSession(t, b.ID, auth.CookieSession, "192.0.2.9")
	if _, err := f.users.Update(ctx, admin, b.ID, auth.UserUpdate{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if f.sessions(t, b.ID) != 0 {
		t.Error("disabling a user left sessions behind")
	}
}

func TestLoginThrottling(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	bad := loginReq("andy", "not the right password", auth.CookieSession)

	// Five failures, then the sixth attempt is refused without checking
	// the password, with a wait.
	for i := range 5 {
		if _, err := f.authn.Login(ctx, bad); err != auth.ErrInvalidCredentials {
			t.Fatalf("failure %d: %v", i+1, err)
		}
		f.clock.advance(7 * time.Second)
	}
	f.clock.advance(-7 * time.Second)
	f.audit.reset()
	good := loginReq("andy", goodPassword, auth.CookieSession)
	_, err := f.authn.Login(ctx, good)
	var limited *auth.RateLimitedError
	if !errors.As(err, &limited) || limited.Limit != auth.LimitUsernameAddress || limited.RetryAfter != time.Second {
		t.Fatalf("sixth attempt: %v, want a 1s delay for this username and address", err)
	}
	if !errors.Is(err, &models.Error{Code: auth.CodeRateLimited}) {
		t.Error("a RateLimitedError must match the rate_limited code")
	}
	// Even the right password is refused during the delay: the refusal
	// must not be an oracle.
	if f.sessions(t, u.ID) != 0 {
		t.Error("a throttled login created a session")
	}
	// Audited aggregated, when the minute is over (or at a flush of
	// everything), not on its own.
	if e := f.audit.find(auth.ActionLogin); len(e) != 0 {
		t.Errorf("a throttled attempt was audited on its own: %+v", e)
	}
	f.authn.FlushThrottled(ctx, true)
	if e := f.audit.find(auth.ActionLogin); len(e) != 1 || e[0].Outcome != audit.Denied ||
		e[0].Details["reason"] != auth.CodeRateLimited || e[0].Details["attempts"] != 1 ||
		fmt.Sprint(e[0].Details["limits"]) != "["+auth.LimitUsernameAddress+"]" || e[0].ActorName != "andy" {
		t.Errorf("throttle audit = %+v", e)
	}
	// The same user from another address is not delayed.
	elsewhere := good
	elsewhere.SourceIP = "198.51.100.20"
	if _, err := f.authn.Login(ctx, elsewhere); err != nil {
		t.Errorf("the user's own address was throttled: %v", err)
	}
	// After the delay the right password works, and clears the count.
	f.clock.advance(time.Second)
	if _, err := f.authn.Login(ctx, good); err != nil {
		t.Errorf("after the delay: %v", err)
	}
	// Nothing was written to the user: this is not a lockout.
	after, _ := f.users.ByID(ctx, u.ID)
	if after.Disabled {
		t.Error("throttling disabled the account")
	}
}

func TestLoginUpgradesWeakHashes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	if err := f.hasher.SetParams(auth.Params{MemoryKiB: auth.MinParams.MemoryKiB, Passes: 3, Lanes: 1}); err != nil {
		t.Fatal(err)
	}
	// A failed login changes nothing.
	f.authn.Login(ctx, loginReq("andy", "not the right password", auth.CookieSession))
	if after, _ := f.users.ByID(ctx, u.ID); after.PasswordHash != u.PasswordHash {
		t.Fatal("a failed login rewrote the hash")
	}
	f.login(t, "andy", auth.CookieSession)
	after, _ := f.users.ByID(ctx, u.ID)
	if after.PasswordHash == u.PasswordHash || !strings.Contains(after.PasswordHash, ",t=3,") {
		t.Errorf("hash after login = %s, want it recomputed with 3 passes", after.PasswordHash)
	}
	// The upgrade is not a password change: the session stays.
	if f.sessions(t, u.ID) != 1 {
		t.Errorf("sessions = %d after a hash upgrade", f.sessions(t, u.ID))
	}
	f.login(t, "andy", auth.CookieSession)
	if again, _ := f.users.ByID(ctx, u.ID); again.PasswordHash != after.PasswordHash {
		t.Error("a current hash was recomputed")
	}
}

func TestVerifyPassword(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)
	res := f.login(t, "andy", auth.CookieSession)
	id, _ := f.authn.Authenticate(ctx, res.Token, auth.CookieSession)
	f.audit.reset()

	if err := f.authn.VerifyPassword(ctx, id, goodPassword, "192.0.2.50", "req-1"); err != nil {
		t.Errorf("the right password: %v", err)
	}
	if err := f.authn.VerifyPassword(ctx, id, "not the right password", "192.0.2.50", "req-2"); err != auth.ErrInvalidCredentials {
		t.Errorf("a wrong password: %v", err)
	}
	if e := f.audit.find(auth.ActionPasswordSet); len(e) != 1 || e[0].Outcome != audit.Failure {
		t.Errorf("audit = %+v", e)
	}
}

func TestNewAuthenticatorValidation(t *testing.T) {
	f := newFixture(t)
	base := auth.AuthenticatorOptions{
		Users: f.store, Sessions: nil, Hasher: f.hasher, Audit: f.audit,
		Session: auth.SessionConfig{IdleTimeout: time.Minute, AbsoluteTimeout: time.Hour},
	}
	if _, err := auth.NewAuthenticator(base); err == nil {
		t.Error("a missing session store was accepted")
	}
	for name, cfg := range map[string]auth.SessionConfig{
		"no idle timeout":        {AbsoluteTimeout: time.Hour},
		"idle above the limit":   {IdleTimeout: 25 * time.Hour, AbsoluteTimeout: 48 * time.Hour},
		"absolute below idle":    {IdleTimeout: time.Hour, AbsoluteTimeout: time.Minute},
		"absolute above 30 days": {IdleTimeout: time.Hour, AbsoluteTimeout: 31 * 24 * time.Hour},
	} {
		o := f.authnOptions()
		o.Session = cfg
		if _, err := auth.NewAuthenticator(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestThrottledLoginsAreAggregated: a flood of refused attempts from one
// address is one access audit entry per minute, not one per attempt.
func TestThrottledLoginsAreAggregated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)
	f.clock.mu.Lock()
	f.clock.t = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	f.clock.mu.Unlock()

	// After 5 failures for one username from one address, further
	// attempts are refused by the throttle (and later also by the limit
	// of 10 per address and minute).
	flood := func(ip string, n int) {
		for range n {
			req := loginReq("nobody", "whatever password", auth.BearerSession)
			req.SourceIP = ip
			f.authn.Login(ctx, req)
			f.clock.advance(100 * time.Millisecond)
		}
	}
	flood("192.0.2.50", 210)
	flood("192.0.2.51", 15)
	checked := func() int {
		n := 0
		for _, e := range f.audit.find(auth.ActionLogin) {
			if e.Details["aggregated"] != true {
				n++
			}
		}
		return n
	}
	aggregated := func() []audit.AccessEntry {
		var out []audit.AccessEntry
		for _, e := range f.audit.find(auth.ActionLogin) {
			if e.Details["aggregated"] == true {
				out = append(out, e)
			}
		}
		return out
	}
	// 10 attempts were checked (5 per address) and audited one each; the
	// 215 refused ones are not yet written: the minute is not over.
	if n := checked(); n != 10 {
		t.Errorf("%d checked attempts audited, want 10", n)
	}
	if a := aggregated(); len(a) != 0 {
		t.Fatalf("aggregated entries before the minute ended: %+v", a)
	}
	f.authn.FlushThrottled(ctx, false)
	if a := aggregated(); len(a) != 0 {
		t.Fatalf("a flush within the minute wrote %d entries", len(a))
	}
	f.clock.advance(time.Minute)
	f.authn.FlushThrottled(ctx, false)
	a := aggregated()
	if len(a) != 2 {
		t.Fatalf("%d aggregated entries, want one per address", len(a))
	}
	byIP := map[string]audit.AccessEntry{a[0].SourceIP: a[0], a[1].SourceIP: a[1]}
	if e := byIP["192.0.2.50"]; e.Details["attempts"] != 205 || e.Outcome != audit.Denied || e.ActorName != "nobody" {
		t.Errorf("192.0.2.50: %+v", e)
	}
	if e := byIP["192.0.2.51"]; e.Details["attempts"] != 10 {
		t.Errorf("192.0.2.51: %+v", e)
	}
}
