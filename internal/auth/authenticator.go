package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Session is one login. The token is not part of it: only its hash is ever
// stored.
type Session struct {
	TokenHash  string
	UserID     string
	Kind       SessionKind
	CreatedAt  time.Time
	LastSeenAt time.Time
	// ExpiresAt is the absolute end, fixed at creation.
	ExpiresAt time.Time
	SourceIP  string
	UserAgent string
}

// SessionStore persists sessions. The SQLite implementation is in
// internal/apidb. A missing session is models.ErrNotFound.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	SessionByHash(ctx context.Context, tokenHash string) (Session, error)
	// TouchSession records that the session was used.
	TouchSession(ctx context.Context, tokenHash string, at time.Time) error
	// DeleteSession removes one session and reports whether it existed.
	DeleteSession(ctx context.Context, tokenHash string) (bool, error)
	// DeleteExpiredSessions removes sessions past their absolute end or
	// not used since idleBefore, and returns how many.
	DeleteExpiredSessions(ctx context.Context, now, idleBefore time.Time) (int, error)
}

// SessionConfig holds the session lifetimes (ADR 0006 decision 3).
type SessionConfig struct {
	// IdleTimeout ends a session that was not used for this long.
	IdleTimeout time.Duration
	// AbsoluteTimeout ends a session this long after the login, whatever
	// its use.
	AbsoluteTimeout time.Duration
}

// Session defaults and limits.
const (
	DefaultIdleTimeout     = 30 * time.Minute
	DefaultAbsoluteTimeout = 12 * time.Hour
	MaxIdleTimeout         = 24 * time.Hour
	MaxAbsoluteTimeout     = 30 * 24 * time.Hour
	// touchInterval is how often, at most, a session's last use is
	// written.
	touchInterval = time.Minute
	// maxUserAgentLength bounds what is stored of a User-Agent header.
	maxUserAgentLength = 256
)

// Audit actions of authentication.
const (
	ActionLogin  = "auth.login"
	ActionLogout = "auth.logout"
)

// Error codes of authentication.
const (
	CodeUnauthenticated    = "unauthenticated"
	CodeInvalidCredentials = "invalid_credentials"
	CodeRateLimited        = "rate_limited"
	CodeSetupRequired      = "setup_required"
	CodeForbidden          = "forbidden"
)

var (
	// ErrUnauthenticated means the request carries no valid session.
	ErrUnauthenticated = &models.Error{Code: CodeUnauthenticated, Message: "authentication is required"}
	// ErrInvalidCredentials is the answer to every failed login. It does
	// not say whether the username exists.
	ErrInvalidCredentials = &models.Error{Code: CodeInvalidCredentials, Message: "the username or password is not correct"}
	// ErrSetupRequired is the answer to a login while no administrator
	// exists.
	ErrSetupRequired = &models.Error{
		Code: CodeSetupRequired,
		Message: "no administrator exists yet; create one on the host with " +
			"'skep-api admin create --username NAME'",
	}
)

// RateLimitedError is returned for a login attempt that a limit refused.
type RateLimitedError struct {
	// RetryAfter is how long the client should wait.
	RetryAfter time.Duration
	// Limit is which limit applied; it is audited, not shown.
	Limit string
}

// Error implements error.
func (e *RateLimitedError) Error() string { return CodeRateLimited + ": too many login attempts" }

// Is makes a RateLimitedError match a models.Error with the same code.
func (e *RateLimitedError) Is(target error) bool {
	t, ok := target.(*models.Error)
	return ok && t.Code == CodeRateLimited
}

// Identity is an authenticated request: a user and the session it came in
// on.
type Identity struct {
	User    User
	Session Session
}

// Actor returns the identity as the actor of an audited action.
func (id Identity) Actor(sourceIP, requestID string) Actor {
	return Actor{ID: id.User.ID, Name: id.User.Username, SourceIP: sourceIP, RequestID: requestID}
}

// AuthenticatorOptions configures an Authenticator.
type AuthenticatorOptions struct {
	Users    UserStore
	Sessions SessionStore
	Hasher   *Hasher
	Audit    AuditWriter
	Throttle ThrottleConfig
	Session  SessionConfig
	Logger   *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

// Authenticator performs logins and checks sessions.
type Authenticator struct {
	users    UserStore
	sessions SessionStore
	hasher   *Hasher
	audit    AuditWriter
	throttle *Throttle
	// throttled aggregates the audit entries of throttled logins.
	throttled *throttleLog
	cfg       SessionConfig
	log       *slog.Logger
	now       func() time.Time
}

// NewAuthenticator validates opts and returns an Authenticator.
func NewAuthenticator(opts AuthenticatorOptions) (*Authenticator, error) {
	if opts.Users == nil || opts.Sessions == nil || opts.Hasher == nil || opts.Audit == nil {
		return nil, errors.New("auth: users, sessions, hasher, and audit are required")
	}
	if opts.Session.IdleTimeout <= 0 || opts.Session.IdleTimeout > MaxIdleTimeout {
		return nil, fmt.Errorf("auth: the idle timeout must be positive and at most %s", MaxIdleTimeout)
	}
	if opts.Session.AbsoluteTimeout < opts.Session.IdleTimeout || opts.Session.AbsoluteTimeout > MaxAbsoluteTimeout {
		return nil, fmt.Errorf("auth: the absolute timeout must be at least the idle timeout and at most %s", MaxAbsoluteTimeout)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	throttle := NewThrottle(opts.Throttle)
	throttle.now = opts.Now
	return &Authenticator{
		users: opts.Users, sessions: opts.Sessions, hasher: opts.Hasher, audit: opts.Audit,
		throttle: throttle, throttled: newThrottleLog(), cfg: opts.Session, log: opts.Logger, now: opts.Now,
	}, nil
}

// LoginRequest is one login attempt.
type LoginRequest struct {
	Username string
	Password string
	// Kind is the kind of session the client asked for.
	Kind      SessionKind
	SourceIP  string
	UserAgent string
	RequestID string
}

// LoginResult is a successful login.
type LoginResult struct {
	// Token is the session token. The caller hands it to the client
	// through the channel that matches Session.Kind, and nowhere else.
	Token   string
	Session Session
	User    User
}

// auditName is the name recorded for a login attempt: the submitted
// username only if it has the shape of one, so the log cannot be used to
// store arbitrary input.
func auditName(username string) string {
	if ValidUsername(username) {
		return username
	}
	return "invalid-username"
}

// Login checks a username and password and, if they are right, starts a
// new session. Every outcome is audited.
func (a *Authenticator) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	if req.Kind != CookieSession && req.Kind != BearerSession {
		return LoginResult{}, models.Invalid(`"session" must be "cookie" or "bearer"`)
	}
	actor := Actor{Name: auditName(req.Username), SourceIP: req.SourceIP, RequestID: req.RequestID}
	refuse := func(outcome audit.Outcome, reason string, extra map[string]any) {
		details := map[string]any{"reason": reason, "session_kind": string(req.Kind)}
		for k, v := range extra {
			details[k] = v
		}
		record(ctx, a.audit, a.log, actor, audit.AccessEntry{Action: ActionLogin, Outcome: outcome, Details: details})
	}

	if ok, retry, limit := a.throttle.Attempt(req.SourceIP, req.Username); !ok {
		// Aggregated: one audit entry per address and minute, written
		// when the minute has ended (throttlelog.go). The first refusal
		// of a minute is logged at once, so an attack is visible early.
		ended, started := a.throttled.add(req.SourceIP, req.Username, string(req.Kind), limit, a.now())
		a.writeThrottled(ctx, ended)
		if started {
			a.log.Warn("login throttled; further refusals from this address are counted and audited "+
				"as one entry per minute", "source_ip", req.SourceIP, "username", actor.Name, "limit", limit)
		}
		return LoginResult{}, &RateLimitedError{RetryAfter: retry, Limit: limit}
	}

	// Without an administrator nobody can log in, and saying so reveals
	// nothing an attacker could use.
	if exists, err := a.users.AdminExists(ctx); err != nil {
		return LoginResult{}, err
	} else if !exists {
		refuse(audit.Failure, CodeSetupRequired, nil)
		return LoginResult{}, ErrSetupRequired
	}

	var (
		user  User
		found bool
	)
	if ValidUsername(req.Username) && len(req.Password) <= 4*MaxPasswordLength {
		u, err := a.users.UserByName(ctx, req.Username)
		switch {
		case err == nil:
			user, found = u, !u.Disabled
		case !errors.Is(err, models.ErrNotFound):
			return LoginResult{}, err
		}
	}

	var ok, rehash bool
	var err error
	if found {
		ok, rehash, err = a.hasher.Verify(ctx, req.Password, user.PasswordHash)
	} else {
		// Spend the same time as for a real user, so the response time
		// does not say whether the username exists or is disabled.
		err = a.hasher.VerifyDummy(ctx, req.Password)
	}
	if errors.Is(err, ErrBusy) {
		refuse(audit.Denied, CodeRateLimited, map[string]any{"limit": "hash_concurrency"})
		return LoginResult{}, &RateLimitedError{RetryAfter: time.Second, Limit: "hash_concurrency"}
	}
	if err != nil {
		return LoginResult{}, err
	}
	if !ok {
		a.throttle.Failure(req.SourceIP, req.Username)
		if user.ID != "" {
			actor.ID = user.ID
		}
		refuse(audit.Failure, CodeInvalidCredentials, nil)
		a.log.Warn("login failed", "source_ip", req.SourceIP, "username", actor.Name)
		return LoginResult{}, ErrInvalidCredentials
	}
	a.throttle.Success(req.SourceIP, req.Username)
	actor.ID = user.ID

	if rehash {
		// The stored hash uses weaker parameters than the current
		// ones. The password is right, so it can be hashed again.
		if stronger, err := a.hasher.Hash(ctx, req.Password); err == nil {
			if err := a.users.UpgradeHash(ctx, user.ID, user.PasswordHash, stronger); err != nil {
				a.log.Warn("could not upgrade a password hash", "user_id", user.ID, "error", err)
			} else {
				a.log.Info("password hash upgraded to the current parameters", "user_id", user.ID)
			}
		}
	}

	// Always a new session: a token from before the login is never
	// reused.
	token, err := NewToken()
	if err != nil {
		return LoginResult{}, err
	}
	now := a.now().UTC()
	sess := Session{
		TokenHash:  HashToken(token),
		UserID:     user.ID,
		Kind:       req.Kind,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(a.cfg.AbsoluteTimeout),
		SourceIP:   req.SourceIP,
		UserAgent:  truncate(req.UserAgent, maxUserAgentLength),
	}
	if err := a.sessions.CreateSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	if err := a.users.RecordLogin(ctx, user.ID, now); err != nil {
		a.log.Warn("could not record the login time", "user_id", user.ID, "error", err)
	}
	record(ctx, a.audit, a.log, actor, audit.AccessEntry{
		Action: ActionLogin, ResourceType: "user", ResourceID: user.ID, Outcome: audit.Success,
		Details: map[string]any{"session_kind": string(req.Kind)},
	})
	a.log.Info("login", "user_id", user.ID, "username", user.Username, "session_kind", string(req.Kind), "source_ip", req.SourceIP)
	return LoginResult{Token: token, Session: sess, User: user}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Authenticate returns the identity behind a session token that was
// presented through the channel of the given kind. A token is accepted
// only through the channel it was issued for. Every failure is
// ErrUnauthenticated: the caller learns nothing about why.
func (a *Authenticator) Authenticate(ctx context.Context, token string, kind SessionKind) (Identity, error) {
	// A token is 43 characters; anything else cannot be one.
	if len(token) != 43 {
		return Identity{}, ErrUnauthenticated
	}
	sess, err := a.sessions.SessionByHash(ctx, HashToken(token))
	if errors.Is(err, models.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	now := a.now().UTC()
	if sess.Kind != kind || !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) >= a.cfg.IdleTimeout {
		return Identity{}, ErrUnauthenticated
	}
	// The user and role are read on every request, so a change takes
	// effect at once. (Such a change also deletes the sessions.)
	user, err := a.users.UserByID(ctx, sess.UserID)
	if errors.Is(err, models.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	if user.Disabled {
		return Identity{}, ErrUnauthenticated
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		if err := a.sessions.TouchSession(ctx, sess.TokenHash, now); err != nil {
			a.log.Warn("could not record the use of a session", "user_id", user.ID, "error", err)
		} else {
			sess.LastSeenAt = now
		}
	}
	return Identity{User: user, Session: sess}, nil
}

// Logout ends the session of id.
func (a *Authenticator) Logout(ctx context.Context, id Identity, sourceIP, requestID string) error {
	if _, err := a.sessions.DeleteSession(ctx, id.Session.TokenHash); err != nil {
		return err
	}
	record(ctx, a.audit, a.log, id.Actor(sourceIP, requestID), audit.AccessEntry{
		Action: ActionLogout, ResourceType: "user", ResourceID: id.User.ID, Outcome: audit.Success,
		Details: map[string]any{"session_kind": string(id.Session.Kind), "session_source_ip": id.Session.SourceIP},
	})
	a.log.Info("logout", "user_id", id.User.ID, "username", id.User.Username)
	return nil
}

// VerifyPassword checks the current password of an authenticated user,
// before a change of that password. A wrong one counts as a failed login
// for the throttle and is audited.
func (a *Authenticator) VerifyPassword(ctx context.Context, id Identity, password, sourceIP, requestID string) error {
	actor := id.Actor(sourceIP, requestID)
	if ok, retry, limit := a.throttle.Attempt(sourceIP, id.User.Username); !ok {
		record(ctx, a.audit, a.log, actor, audit.AccessEntry{
			Action: ActionPasswordSet, ResourceType: "user", ResourceID: id.User.ID, Outcome: audit.Denied,
			Details: map[string]any{"reason": CodeRateLimited, "limit": limit},
		})
		return &RateLimitedError{RetryAfter: retry, Limit: limit}
	}
	ok, _, err := a.hasher.Verify(ctx, password, id.User.PasswordHash)
	if errors.Is(err, ErrBusy) {
		return &RateLimitedError{RetryAfter: time.Second, Limit: "hash_concurrency"}
	}
	if err != nil {
		return err
	}
	if !ok {
		a.throttle.Failure(sourceIP, id.User.Username)
		record(ctx, a.audit, a.log, actor, audit.AccessEntry{
			Action: ActionPasswordSet, ResourceType: "user", ResourceID: id.User.ID, Outcome: audit.Failure,
			Details: map[string]any{"reason": CodeInvalidCredentials},
		})
		return ErrInvalidCredentials
	}
	a.throttle.Success(sourceIP, id.User.Username)
	return nil
}

// Sweep deletes sessions that have expired. Expiry is not a revocation and
// is not audited per session.
func (a *Authenticator) Sweep(ctx context.Context) (int, error) {
	now := a.now().UTC()
	return a.sessions.DeleteExpiredSessions(ctx, now, now.Add(-a.cfg.IdleTimeout))
}

// Run sweeps expired sessions once per interval, and writes the
// aggregated audit entries of throttled logins whose minute has ended
// every ThrottleFlushInterval, until ctx is done. The caller writes what
// is still pending with FlushThrottled at shutdown.
func (a *Authenticator) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flush := time.NewTicker(ThrottleFlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			a.FlushThrottled(ctx, false)
		case <-ticker.C:
			if n, err := a.Sweep(ctx); err != nil {
				a.log.Warn("could not delete expired sessions", "error", err)
			} else if n > 0 {
				a.log.Debug("expired sessions deleted", "count", n)
			}
		}
	}
}

// SessionLifetime returns the configured lifetimes.
func (a *Authenticator) SessionLifetime() SessionConfig { return a.cfg }
