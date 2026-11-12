package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Defaults for the Hasher.
const (
	// DefaultHashConcurrency is how many hashes may run at once. With the
	// default parameters a flood of logins then costs at most 128 MiB and
	// two cores.
	DefaultHashConcurrency = 2
	// DefaultHashWait is how long a login waits for a free slot before
	// it is refused.
	DefaultHashWait = 2 * time.Second
)

// ErrBusy means every hashing slot stayed taken for the whole wait. The
// API answers it with 429.
var ErrBusy = errors.New("auth: too many logins are being verified at once")

// Hasher hashes and verifies passwords with bounded concurrency.
type Hasher struct {
	slots chan struct{}
	wait  time.Duration

	mu     sync.RWMutex
	params Params
	// dummy is a hash of a random password made with the current
	// parameters. Verifying against it takes as long as verifying a real
	// user's password, so the response time does not say whether a
	// username exists.
	dummy string
}

// NewHasher returns a Hasher. It computes the dummy hash, which takes as
// long as one login.
func NewHasher(params Params, concurrency int, wait time.Duration) (*Hasher, error) {
	if concurrency < 1 {
		return nil, errors.New("auth: hash concurrency must be positive")
	}
	h := &Hasher{slots: make(chan struct{}, concurrency), wait: wait}
	if err := h.SetParams(params); err != nil {
		return nil, err
	}
	return h, nil
}

// Params returns the parameters new hashes are made with.
func (h *Hasher) Params() Params {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.params
}

// SetParams changes the parameters for new hashes and recomputes the dummy
// hash with them: the dummy must never be older than the parameters, or an
// unknown user would answer faster or slower than a real one.
func (h *Hasher) SetParams(params Params) error {
	if err := params.Validate(); err != nil {
		return err
	}
	random, err := NewToken()
	if err != nil {
		return err
	}
	dummy, err := hashPassword(random, params)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.params, h.dummy = params, dummy
	h.mu.Unlock()
	return nil
}

// acquire takes a hashing slot, waiting at most h.wait.
func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	default:
	}
	timer := time.NewTimer(h.wait)
	defer timer.Stop()
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.slots }

// Hash hashes a password with the current parameters.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return hashPassword(password, h.Params())
}

// Verify checks password against a stored hash. rehash is true when the
// password is right and the hash was made with weaker parameters than the
// current ones, so the caller should store a new hash.
func (h *Hasher) Verify(ctx context.Context, password, phc string) (ok, rehash bool, err error) {
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	ok, used, err := verifyPassword(password, phc)
	if err != nil {
		return false, false, err
	}
	return ok, ok && used.weakerThan(h.Params()), nil
}

// VerifyDummy spends the time of one verification and always fails. It is
// used when there is no user to verify against.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) error {
	if err := h.acquire(ctx); err != nil {
		return err
	}
	defer h.release()
	h.mu.RLock()
	dummy := h.dummy
	h.mu.RUnlock()
	_, _, err := verifyPassword(password, dummy)
	return err
}
