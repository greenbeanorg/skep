package usercopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Time limits of one write (D16).
const (
	// RepeatFor is how long a write whose outcome is unknown is repeated
	// with the same operation ID.
	RepeatFor = 10 * time.Second
	// SyncFor is how long the server waits for its copy to reach the
	// entry's index before it answers.
	SyncFor = 10 * time.Second
	// repeatPause is the pause between two attempts.
	repeatPause = 500 * time.Millisecond
)

// Writer sends user writes through skep-cluster: it implements
// auth.UserWriter.
type Writer struct {
	copy *Copy
	// sync makes the writer apply the answers itself: the admin commands,
	// which run without the server's poll.
	sync bool
}

// NewWriter returns the server's writer.
func NewWriter(c *Copy) *Writer { return &Writer{copy: c} }

// NewCommandWriter returns the writer of a local command: it updates the
// copy itself after each write.
func NewCommandWriter(c *Copy) *Writer { return &Writer{copy: c, sync: true} }

// Quorum implements auth.UserWriter. A command has no poll: it reads once.
func (w *Writer) Quorum() bool {
	if w.sync {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.copy.SyncOnce(ctx, 0)
	}
	return w.copy.Quorum()
}

// ActorString is a user entry's actor (D2): "user:<username>" for an API
// user, the local actor ("local:<uid>") for a command on the host.
func ActorString(a auth.Actor) string {
	if a.ID != "" {
		return "user:" + a.Name
	}
	return a.Name
}

// Write implements auth.UserWriter (D16): one operation ID for the
// request, repeated while the outcome is unknown, then the wait for the
// copy.
func (w *Writer) Write(ctx context.Context, uw auth.UserWrite) error {
	cs, err := w.copy.store.CopyState(ctx)
	if err != nil {
		return err
	}
	if cs.Import == apidb.ImportPending {
		return &models.Error{Code: auth.CodeUsersImportPending, Message: "this host's users are not yet the cluster's: " +
			"run \"skep-api admin import-users\" as root first; nothing was written"}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(uw.Data)
	if err != nil {
		return err
	}
	req := userapi.UsersWriteRequest{Type: uw.Type, OpID: opID, Actor: ActorString(uw.Actor), RequestID: uw.Actor.RequestID, Data: data}
	repeat, syncFor := RepeatFor, SyncFor
	if uw.Timeout > 0 {
		// The limit covers the whole write (the rehash at login, D19).
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, uw.Timeout)
		defer cancel()
		repeat, syncFor = uw.Timeout, uw.Timeout
	}
	res, err := w.send(ctx, req, repeat)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, syncFor)
	defer cancel()
	if err := w.copy.waitIndex(wctx, res.Index, w.sync); err != nil {
		return auth.NoQuorum(auth.OutcomeWrittenNotSynced, fmt.Sprintf(
			"the change was written to the cluster (index %d) but this node's copy has not reached it within %s", res.Index, syncFor))
	}
	return nil
}

// Send sends one users-write with a given operation ID, repeating it while
// the outcome is unknown for up to repeat, and maps a refusal.
func (w *Writer) Send(ctx context.Context, req userapi.UsersWriteRequest, repeat time.Duration) (userapi.UsersWriteResult, error) {
	return w.send(ctx, req, repeat)
}

func (w *Writer) send(ctx context.Context, req userapi.UsersWriteRequest, repeat time.Duration) (userapi.UsersWriteResult, error) {
	deadline := time.Now().Add(repeat)
	unknown := false
	for {
		res, err := w.copy.client.UsersWrite(ctx, req)
		if err == nil {
			return res, nil
		}
		transient, unk := isTransient(err)
		unknown = unknown || unk
		if !transient {
			return res, refusal(err)
		}
		if time.Now().Add(repeatPause).After(deadline) || ctx.Err() != nil {
			outcome := auth.OutcomeNotWritten
			if unknown {
				outcome = auth.OutcomeUnknown
			}
			return res, auth.NoQuorum(outcome, "skep-cluster has no quorum: the change could not be committed ("+errText(err)+")")
		}
		select {
		case <-ctx.Done():
		case <-time.After(repeatPause):
		}
	}
}

// errText is a write error's text for a message: the code and message of
// skep-cluster's answer. No request data is in it.
func errText(err error) string {
	var e *userapi.Error
	if errors.As(err, &e) {
		return e.Code + ": " + e.Message
	}
	return err.Error()
}

// RefusalCodes are the state machine's refusal codes (D5): an entry
// refused with one of them applied nothing.
var RefusalCodes = []string{"username_taken", "last_admin", "unknown_role", "not_found", "users_exist", "hash_changed", "too_many_users", "invalid"}

// Refused is a refusal with one of RefusalCodes. It wraps the error that
// internal/auth knows, and reads as it.
type Refused struct {
	Code string
	Err  error
}

func (r *Refused) Error() string { return r.Err.Error() }
func (r *Refused) Unwrap() error { return r.Err }

// refusal maps a refusal code to internal/auth's errors (D5).
func refusal(err error) error {
	var e *userapi.Error
	if !errors.As(err, &e) {
		return err
	}
	mapped := mapRefusal(e)
	if slices.Contains(RefusalCodes, e.Code) {
		return &Refused{Code: e.Code, Err: mapped}
	}
	return mapped
}

func mapRefusal(e *userapi.Error) error {
	switch e.Code {
	case "username_taken":
		return auth.ErrUsernameTaken
	case "last_admin":
		return auth.ErrLastAdmin
	case "unknown_role":
		return auth.ErrUnknownRole
	case "not_found":
		return fmt.Errorf("user: %w", models.ErrNotFound)
	case "invalid", "invalid_request":
		return models.Invalid(e.Message)
	case "users_exist":
		return ErrUsersExist
	}
	return &models.Error{Code: e.Code, Message: e.Message}
}

// LocalActor is the actor of a command run on the host by uid.
func LocalActor(uid int) auth.Actor { return auth.Actor{Name: audit.LocalActor(uid)} }
