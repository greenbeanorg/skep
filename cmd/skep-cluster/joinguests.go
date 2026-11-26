package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The agent's list operations as root (step 8d-2, D12): pages of
// agentPage, each answer read up to agentAnswerLimit.
const (
	agentPage        = 200
	agentAnswerLimit = 4 << 20
)

// agentClient is an HTTP client on skep-agent's socket. Only root-run
// commands use it; the service never connects to the agent.
func agentClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}}
}

// agentError is an error answer of the agent: its status and code.
type agentError struct {
	status  int
	code    string
	message string
}

func (e *agentError) Error() string {
	return fmt.Sprintf("skep-agent answered %d %s: %s", e.status, e.code, e.message)
}

// agentCall makes one request to the agent as root with the join's actor
// and decodes a 200 answer into out, reading at most agentAnswerLimit.
func agentCall(ctx context.Context, socket, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+agentproto.Prefix+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	agentproto.Caller{ActorID: joinActor, ActorName: joinActor}.SetHeaders(req.Header)
	resp, err := agentClient(socket).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach skep-agent on %s (is skep_agent running?): %w", socket, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, agentAnswerLimit+1))
	if err != nil {
		return fmt.Errorf("skep-agent's answer to %s %s: %w", method, path, err)
	}
	if len(b) > agentAnswerLimit {
		return fmt.Errorf("skep-agent's answer to %s %s is larger than %d bytes", method, path, agentAnswerLimit)
	}
	if resp.StatusCode != http.StatusOK {
		var e models.ErrorResponse
		_ = json.Unmarshal(b, &e)
		return &agentError{status: resp.StatusCode, code: e.Error.Code, message: e.Error.Message}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("skep-agent's answer to %s %s: %w", method, path, err)
	}
	return nil
}

// agentGuestList reads every VM and every jail of the host from the
// agent's list operations (D12), each page of agentPage by the agent's
// cursor, and stops with an error as soon as the total passes
// MaxJoinGuests. An item becomes {id, kind, name}, its kind by the list
// it came from.
func agentGuestList(ctx context.Context, socket string) ([]clusterstate.JoinGuest, error) {
	var out []clusterstate.JoinGuest
	for _, l := range []struct{ path, kind string }{{"/vms", clusterstate.KindVM}, {"/containers", clusterstate.KindContainer}} {
		cursor := ""
		for {
			q := url.Values{"limit": {fmt.Sprint(agentPage)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var page struct {
				Items []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"items"`
				NextCursor string `json:"next_cursor"`
			}
			if err := agentCall(ctx, socket, http.MethodGet, l.path+"?"+q.Encode(), nil, &page); err != nil {
				return nil, err
			}
			for _, it := range page.Items {
				out = append(out, clusterstate.JoinGuest{ID: it.ID, Kind: l.kind, Name: it.Name})
			}
			if len(out) > clusterstate.MaxJoinGuests {
				return nil, fmt.Errorf("this host has more than %d guests; at most %d may join with it", clusterstate.MaxJoinGuests, clusterstate.MaxJoinGuests)
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor || len(page.Items) == 0 {
				return nil, fmt.Errorf("skep-agent's list %s does not advance", l.path)
			}
			cursor = page.NextCursor
		}
	}
	return out, nil
}

// checkHostGuests is the rule a host's list must meet to join (D10 step
// 2): D4's limits and the cluster's rules for each guest.
func checkHostGuests(guests []clusterstate.JoinGuest) error {
	if len(guests) == 0 {
		return nil
	}
	if err := clusterstate.CheckJoinGuestLimits(guests); err != nil {
		return err
	}
	return clusterstate.CheckJoinGuests(guests)
}

// drainAnswer is the agent's answer of its drain operations.
type drainAnswer struct {
	Reason          string     `json:"reason,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	UnfinishedTasks int        `json:"unfinished_tasks"`
}

// agentDrain sets or renews (POST), reads (GET) or clears (DELETE) the
// agent's drain with the reason "join" (D11).
func agentDrain(ctx context.Context, socket, method string) (drainAnswer, error) {
	var body []byte
	if method == http.MethodPost {
		body = []byte(`{"reason":"join"}`)
	}
	var a drainAnswer
	err := agentCall(ctx, socket, method, "/drain", body, &a)
	return a, err
}

// isDrainActive reports whether err is the agent's 409 drain_active: a
// drain of another reason is active.
func isDrainActive(err error) bool {
	var ae *agentError
	return errors.As(err, &ae) && ae.code == models.CodeDrainActive
}

// drainRenewEvery is how often the join renews its drain (D11).
const drainRenewEvery = 20 * time.Second

// joinDrain is the join's drain (D11): set once, renewed every
// drainRenewEvery, cleared at most once and only while it is the join's
// own (the agent's clear removes a drain of any reason).
type joinDrain struct {
	e      env
	socket string
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	failed  error // the first failed renewal before the service stopped
	stopped bool  // the service is stopped: a failed renewal is printed
	ours    bool  // the drain is the join's own
	cleared bool
}

// setJoinDrain sets the drain and starts its renewal.
func setJoinDrain(ctx context.Context, e env, socket string) (*joinDrain, error) {
	if _, err := e.agentDrain(ctx, socket, http.MethodPost); err != nil {
		if isDrainActive(err) {
			return nil, fmt.Errorf("refused: skep-agent has a drain with another reason active (%s); nothing was changed", termSafe(err.Error()))
		}
		// The answer may have been lost after the drain was set: clear
		// it, which is harmless when it was not.
		_, cerr := e.agentDrain(context.WithoutCancel(ctx), socket, http.MethodDelete)
		return nil, errors.Join(fmt.Errorf("refused: setting skep-agent's drain failed (%s); the drain was cleared if it had been set; nothing else was changed", termSafe(err.Error())), cerr)
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &joinDrain{e: e, socket: socket, cancel: cancel, done: make(chan struct{}), ours: true}
	go d.renew(rctx)
	return d, nil
}

func (d *joinDrain) renew(ctx context.Context) {
	defer close(d.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(drainRenewEvery):
		}
		_, err := d.e.agentDrain(ctx, d.socket, http.MethodPost)
		if err == nil || ctx.Err() != nil {
			continue
		}
		d.mu.Lock()
		if isDrainActive(err) {
			d.ours = false // it expired and another took its place
		}
		if d.stopped {
			fmt.Fprintf(d.e.stderr, "skep-cluster: renewing skep-agent's drain failed (%s); the join goes on: the service is stopped, so no guest can be created through the API\n", termSafe(err.Error()))
		} else if d.failed == nil {
			d.failed = err
		}
		d.mu.Unlock()
	}
}

// renewFailed returns the first renewal that failed before the service
// was stopped.
func (d *joinDrain) renewFailed() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failed
}

// serviceStopped marks that the service is stopped (D11): from now on a
// failed renewal is printed and the join goes on.
func (d *joinDrain) serviceStopped() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
}

// clear stops the renewal and clears the drain, once, if it is still the
// join's own. Every path of runJoin that set the drain calls it.
func (d *joinDrain) clear(ctx context.Context) error {
	d.cancel()
	<-d.done
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cleared || !d.ours {
		return nil
	}
	d.cleared = true
	if _, err := d.e.agentDrain(context.WithoutCancel(ctx), d.socket, http.MethodDelete); err != nil {
		return fmt.Errorf("clearing skep-agent's drain failed (%s); it expires by itself within 60 s", termSafe(err.Error()))
	}
	return nil
}

// waitTasks waits until the agent reports no unfinished task, reading the
// drain's own answer, for at most wait (D11).
func (d *joinDrain) waitTasks(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		a, err := d.e.agentDrain(ctx, d.socket, http.MethodGet)
		switch {
		case err != nil:
			return fmt.Errorf("reading skep-agent's drain failed: %s", termSafe(err.Error()))
		case a.Reason != "join":
			return errors.New("skep-agent's drain is no longer the join's")
		case a.UnfinishedTasks == 0:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("skep-agent still has %d unfinished tasks after %s (-task-wait)", a.UnfinishedTasks, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// countKinds counts a list's VMs and jails.
func countKinds(guests []clusterstate.JoinGuest) (vms, jails int) {
	for _, g := range guests {
		if g.Kind == clusterstate.KindVM {
			vms++
		} else {
			jails++
		}
	}
	return vms, jails
}

// printGuests shows the guests that join with the host (D10 step 4): how
// many of each kind, and the first 20 names of each.
func printGuests(w io.Writer, guests []clusterstate.JoinGuest) {
	vms, jails := countKinds(guests)
	if vms+jails == 0 {
		fmt.Fprintf(w, "This host has no VMs and no jails.\n")
		return
	}
	fmt.Fprintf(w, "This host's guests join with it: %d VMs and %d jails.\n", vms, jails)
	for _, k := range []struct{ kind, label string }{{clusterstate.KindVM, "VMs"}, {clusterstate.KindContainer, "Jails"}} {
		var names []string
		total := 0
		for _, g := range guests {
			if g.Kind == k.kind {
				total++
				if len(names) < 20 {
					names = append(names, termSafe(g.Name))
				}
			}
		}
		if total == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s:", k.label)
		for _, n := range names {
			fmt.Fprintf(w, " %s", n)
		}
		if total > len(names) {
			fmt.Fprintf(w, " and %d more", total-len(names))
		}
		fmt.Fprintln(w)
	}
}

// sameGuests reports whether two lists hold the same guests in the same
// order (the agent lists by name).
func sameGuests(a, b []clusterstate.JoinGuest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stageInput is what the join stage reads on its standard input (D13).
type stageInput struct {
	Guests []clusterstate.JoinGuest `json:"guests"`
}

// maxStageInput bounds the stage's standard input.
const maxStageInput = 256 << 10

// readStageInput reads the stage's standard input strictly: one JSON
// object {"guests":[...]}, at most maxStageInput bytes, whose list meets
// D4's limits and the cluster's rules.
func readStageInput(r io.Reader) ([]clusterstate.JoinGuest, error) {
	if r == nil {
		return nil, errors.New("the join stage needs its guest list on its standard input")
	}
	b, err := io.ReadAll(io.LimitReader(r, maxStageInput+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxStageInput {
		return nil, fmt.Errorf("the guest list on the standard input is larger than %d bytes", maxStageInput)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var in stageInput
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("the guest list on the standard input: %w", err)
	}
	if dec.More() {
		return nil, errors.New("the guest list on the standard input: more than one JSON value")
	}
	if in.Guests == nil {
		return nil, errors.New(`the guest list on the standard input has no "guests" field`)
	}
	if err := checkHostGuests(in.Guests); err != nil {
		return nil, err
	}
	return in.Guests, nil
}
