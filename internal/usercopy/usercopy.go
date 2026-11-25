// Package usercopy is skep-api's side of the replicated users (ADR 0013
// amendment 4, step 8c-1): the read copy in api.db, kept by a long poll of
// skep-cluster's users-read, whether there is quorum, and the user writes
// sent through users-write.
//
// The copy is written only by apidb.ApplyAnswer, which this package calls
// with each answer from a node with quorum.
package usercopy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Timings (D16, D17).
const (
	// MaxAge is how old the last answer may be before there is no quorum.
	MaxAge = 15 * time.Second
	// pollWait is users-read's wait.
	pollWait = 5 * time.Second
	// retryPause is the pause after a failed read.
	retryPause = time.Second
)

// CheckName is the health check of the copy (D20).
const CheckName = "cluster_users"

// Copy keeps the read copy and knows whether there is quorum.
type Copy struct {
	store  *apidb.UserStore
	client *userapi.Client
	log    *slog.Logger
	now    func() time.Time

	mu sync.Mutex
	// last is when the last answer came; facts is what it said.
	last    time.Time
	facts   userapi.UsersAnswer
	readErr error
	// applied is the copy's users index after the last apply.
	applied uint64
	changed chan struct{}
}

// New returns the copy. Run keeps it.
func New(store *apidb.UserStore, client *userapi.Client, log *slog.Logger) *Copy {
	if log == nil {
		log = slog.Default()
	}
	return &Copy{store: store, client: client, log: log, now: time.Now, changed: make(chan struct{})}
}

// Run long-polls users-read and applies each answer, until ctx ends.
func (c *Copy) Run(ctx context.Context) {
	for ctx.Err() == nil {
		start := c.now()
		c.mu.Lock()
		before := c.applied
		c.mu.Unlock()
		err := c.SyncOnce(ctx, pollWait)
		c.mu.Lock()
		progress := c.applied != before
		c.mu.Unlock()
		// An answer that applied nothing and came at once (an import
		// pending, a joining node, a copy that is ahead) or an error:
		// pause, so that the poll does not spin.
		if err != nil || (!progress && c.now().Sub(start) < retryPause) {
			select {
			case <-ctx.Done():
			case <-time.After(retryPause):
			}
		}
	}
}

// SyncOnce reads the users once, waiting up to wait for a change, and
// applies the answer if it comes from a node with quorum.
func (c *Copy) SyncOnce(ctx context.Context, wait time.Duration) error {
	cs, err := c.store.CopyState(ctx)
	if err != nil {
		return err
	}
	after := cs.UsersIndex
	c.mu.Lock()
	if c.facts.ClusterID != "" && c.facts.ClusterID != cs.ClusterID {
		after = 0 // another cluster: ask without waiting on our index
	}
	if c.last.IsZero() || c.readErr != nil {
		wait = 0 // no answer yet: quorum is not known until one comes
	}
	c.mu.Unlock()
	ans, err := c.client.UsersRead(ctx, after, wait)
	c.mu.Lock()
	if err != nil {
		if c.readErr == nil {
			c.log.Warn("cannot read the users from skep-cluster; treated as no quorum", "error", err)
		}
		c.readErr = err
		c.mu.Unlock()
		return err
	}
	if c.readErr != nil {
		c.log.Info("skep-cluster answers again")
	}
	c.readErr, c.last, c.facts = nil, c.now(), ans
	c.facts.Users = nil // the hashes are kept only in api.db
	c.mu.Unlock()
	if problem := factsProblem(ans); problem != "" {
		return nil // no quorum: nothing is applied
	}
	res, err := c.store.ApplyAnswer(ctx, ans, c.now())
	if err != nil {
		c.log.Error("cannot apply the cluster's users to the copy", "error", err)
		return err
	}
	if res.Changed {
		c.log.Info("cluster users applied to the copy", "cluster_id", ans.ClusterID, "users_index", res.UsersIndex,
			"users", len(ans.Users), "replaced", res.Replaced, "sessions_ended", res.SessionsEnded)
	}
	c.mu.Lock()
	if res.UsersIndex != c.applied {
		c.applied = res.UsersIndex
		close(c.changed)
		c.changed = make(chan struct{})
	}
	c.mu.Unlock()
	return nil
}

func factsProblem(a userapi.UsersAnswer) string {
	switch {
	case a.Removed:
		return "this node was removed from its cluster"
	case a.Joining:
		return "this node is joining a cluster"
	case a.Stalled:
		return "this node's replicated state machine has stopped"
	case !a.LeaderKnown:
		return "the cluster has no leader"
	}
	return ""
}

// Problem says why there is no quorum, or "" (D17).
func (c *Copy) Problem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.readErr != nil:
		return "skep-cluster is not reachable on its socket"
	case c.last.IsZero():
		return "no answer from skep-cluster yet"
	case c.now().Sub(c.last) > MaxAge:
		return fmt.Sprintf("the last answer from skep-cluster is older than %s", MaxAge)
	}
	return factsProblem(c.facts)
}

// Quorum reports whether there is quorum.
func (c *Copy) Quorum() bool { return c.Problem() == "" }

// waitIndex waits until the copy's users index reaches idx. With sync,
// it reads and applies itself (a command without the running server).
func (c *Copy) waitIndex(ctx context.Context, idx uint64, sync bool) error {
	for {
		cs, err := c.store.CopyState(ctx)
		if err != nil {
			return err
		}
		if cs.UsersIndex >= idx {
			return nil
		}
		if sync {
			if err := c.SyncOnce(ctx, time.Second); err != nil && ctx.Err() == nil {
				time.Sleep(200 * time.Millisecond)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		c.mu.Lock()
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Health is the copy's health check (D20, not critical): no quorum, an
// import pending, and how old the copy is.
func (c *Copy) Health(ctx context.Context) string {
	cs, err := c.store.CopyState(ctx)
	if err != nil {
		return "cannot read the copy's state"
	}
	if cs.Import == apidb.ImportPending {
		return "this host's users are not yet the cluster's: run \"skep-api admin import-users\" as root"
	}
	if p := c.Problem(); p != "" {
		c.mu.Lock()
		last := c.last
		c.mu.Unlock()
		age := "never updated since skep-api started"
		if !last.IsZero() {
			age = fmt.Sprintf("last answer %s ago", c.now().Sub(last).Round(time.Second))
		}
		return "no quorum (" + p + "): user changes are refused and only administrators may log in, degraded; " + age
	}
	return ""
}

// ErrUsersExist is the import's refusal when the cluster has users.
var ErrUsersExist = &models.Error{Code: "users_exist", Message: "the cluster has users already"}

// isTransient reports whether a write may be repeated with the same
// operation ID, and whether its outcome is unknown.
func isTransient(err error) (transient, unknown bool) {
	var e *userapi.Error
	switch {
	case errors.Is(err, userapi.ErrUnreachable):
		return true, false
	case errors.Is(err, userapi.ErrNoAnswer):
		return true, true
	case errors.As(err, &e):
		switch e.Code {
		case "no_leader", "unavailable", "not_committed", "removed", "joining":
			return true, false
		case "outcome_unknown":
			return true, true
		}
	}
	return false, false
}
