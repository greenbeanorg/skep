package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// Key rotation of the node key (D1 of step 8b-2b). A member entry can hold
// a second pin; both are accepted as that node. The node switches to its
// new key by itself once every member has applied the entry that added it;
// the leader drops the old pin once it has seen the node use the new one.
// Nothing is stopped at any point.
//
// On the rotating node, in this order: (1) the new key is made and written
// to node.key.next; (2) rotate.json records the rotation; (3) the leader is
// asked (key-add, the same operation ID on every retry); (4) once this
// node has seen every member at or past the add entry's index, node.key.next
// is renamed to node.key, a certificate is issued for it, and the listener
// and the diallers use it; (5) when this node applies member.key_done for
// itself, rotate.json is deleted.

// Rotation phases in rotate.json.
const (
	PhaseRotating = "rotating"
	PhaseAborting = "aborting"
)

// RotateState is rotate.json in the data directory.
type RotateState struct {
	Format  int    `json:"format"`
	OldPin  string `json:"old_pin"`
	NewPin  string `json:"new_pin"`
	OpID    string `json:"op_id"`
	Started string `json:"started"`
	Phase   string `json:"phase"`
}

const rotateFormat = 1

// rotateRetry is how often an unknown outcome is asked again.
const rotateRetry = 10 * time.Second

// ReadRotate reads rotate.json in dataDir; (nil, nil) without one.
func ReadRotate(dataDir string) (*RotateState, error) {
	path := filepath.Join(dataDir, layout.RotateFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r RotateState
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Format != rotateFormat || !clusterstate.ValidPin(r.OldPin) || !clusterstate.ValidPin(r.NewPin) || r.OldPin == r.NewPin ||
		!opIDRE.MatchString(r.OpID) || (r.Phase != PhaseRotating && r.Phase != PhaseAborting) {
		return nil, fmt.Errorf("%s: not a valid rotation record", path)
	}
	return &r, nil
}

func writeRotate(dataDir string, r RotateState) error {
	r.Format = rotateFormat
	return writeFileAtomic(dataDir, layout.RotateFile, r)
}

// RotateStartRule is the start rule's part for a rotation (after the wipe,
// layout and abandon rules, which win): node.key.next without rotate.json
// is deleted with a warning; rotate.json with node.key.next goes on; without
// node.key.next, a node.key with the new pin has switched (go on at step
// 5), and one with the old pin either finishes an abort (phase aborting:
// rotate.json is deleted) or refuses to start (phase rotating), naming the
// three files.
func RotateStartRule(dataDir string, warn func(msg string, args ...any)) error {
	r, err := ReadRotate(dataDir)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	next := filepath.Join(dataDir, nodekey.NextKeyFile)
	hasNext, err := layout.Exists(next)
	if err != nil {
		return err
	}
	if r == nil {
		if hasNext {
			if err := layout.RemoveFile(next); err != nil {
				return err
			}
			warn("node.key.next without rotate.json (a rotation stopped before it was recorded, or after it ended): deleted", "path", next)
		}
		return nil
	}
	if hasNext {
		return nil
	}
	pin, err := nodekey.FilePin(dataDir, nodekey.KeyFile)
	if err != nil {
		return fmt.Errorf("refusing to start: rotate.json exists and node.key cannot be read: %w", err)
	}
	switch {
	case pin == r.NewPin:
		return nil // switched; step 5 follows
	case pin == r.OldPin && r.Phase == PhaseAborting:
		if err := layout.RemoveFile(filepath.Join(dataDir, layout.RotateFile)); err != nil {
			return err
		}
		warn("an aborted key rotation had deleted node.key.next; rotate.json is deleted now", "old_pin", r.OldPin)
		return nil
	case pin == r.OldPin:
		return fmt.Errorf("refusing to start: %s says a key rotation is under way (phase rotating, new pin %s), but %s is missing and %s still has the old pin %s. "+
			"The rotation cannot go on. If \"skep-cluster status\" on a member does not list %s as this node's next pin, delete %s as root and start again",
			filepath.Join(dataDir, layout.RotateFile), r.NewPin, next, filepath.Join(dataDir, nodekey.KeyFile), r.OldPin, r.NewPin,
			filepath.Join(dataDir, layout.RotateFile))
	}
	return fmt.Errorf("refusing to start: node.key has pin %s, neither the old (%s) nor the new (%s) pin of rotate.json", pin, r.OldPin, r.NewPin)
}

// keyAddRequest is the body of key-add.
type keyAddRequest struct {
	OpID    string `json:"op_id"`
	NextPin string `json:"next_pin"`
	Actor   string `json:"actor"`
}

// keyAddAnswer is its answer: the index of the member.key_add entry.
type keyAddAnswer struct {
	AddIndex uint64 `json:"add_index"`
}

// keyAbortRequest is the body of key-abort.
type keyAbortRequest struct {
	OpID    string `json:"op_id"`
	NextPin string `json:"next_pin"`
	Actor   string `json:"actor"`
}

// Answers of key-abort.
const (
	abortProposed = "aborted"
	abortNotAdded = "not_added"
	abortFinished = "finished"
)

type keyAbortAnswer struct {
	Result string `json:"result"`
}

func (s *Service) rotationState() *RotateState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotation == nil {
		return nil
	}
	c := *s.rotation
	return &c
}

func (s *Service) setRotation(r *RotateState) {
	s.mu.Lock()
	s.rotation = r
	s.mu.Unlock()
}

// rotateStepDone runs the test hook; true stops the rotation work there.
func (s *Service) rotateStepDone(step string) bool {
	s.mu.Lock()
	hook := s.afterRotateStep
	s.mu.Unlock()
	if hook == nil || !hook(step) {
		return false
	}
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
	return true
}

// Crash points of a rotation (the test hook's step names).
const (
	rotStopAfterNext   = "after node.key.next"
	rotStopAfterRecord = "after rotate.json"
	rotStopAfterAdd    = "after the key-add answer"
	rotStopAfterRename = "after the rename"
	rotStopAfterSwitch = "after the switch"
	rotStopAfterAbort  = "after phase aborting"
	rotStopAfterNextRm = "after node.key.next is deleted"
)

func rotationErr(status int, code, msg string) *localapi.OpError {
	return &localapi.OpError{HTTPStatus: status, Code: code, Message: msg}
}

// Rotate implements localapi.Backend: "skep-cluster key rotate", root
// only. It makes the new key, records the rotation, and asks the leader
// once; a refusal undoes both files. The rest goes on in the background.
func (s *Service) Rotate(ctx context.Context, actor string) (localapi.RotateResult, error) {
	if s.isRemoved() {
		return localapi.RotateResult{}, s.removedError()
	}
	if s.joinState() != nil {
		return localapi.RotateResult{}, rotationErr(http.StatusConflict, "joining", "this node is joining a cluster; rotate its key once it is a member")
	}
	if r := s.rotationState(); r != nil {
		return localapi.RotateResult{}, rotationErr(http.StatusConflict, "rotation_in_progress",
			fmt.Sprintf("this node's key rotation is under way (phase %s, new pin %s)", r.Phase, r.NewPin))
	}
	if s.isFrozen() {
		return localapi.RotateResult{}, rotationErr(http.StatusServiceUnavailable, "unavailable", "this node is stopping")
	}
	view := s.machine.View()
	if view.Stalled != nil {
		return localapi.RotateResult{}, rotationErr(http.StatusServiceUnavailable, "unavailable", "this node's state machine is stopped; nothing was written")
	}
	self, ok := memberOf(view, s.cfg.NodeID)
	if !ok || self.State != clusterstate.StateMember {
		return localapi.RotateResult{}, refusedErr("this node is not a member in state member; nothing was written")
	}
	if self.Pin != s.key.Pin() {
		return localapi.RotateResult{}, refusedErr("the node key on disk is not the one pinned in this node's member entry; nothing was written")
	}
	for _, m := range view.Members {
		if m.NextPin != "" {
			return localapi.RotateResult{}, rotationErr(http.StatusConflict, "rotation_in_progress",
				fmt.Sprintf("member %s is rotating its key: one rotation in the cluster at a time; nothing was written", m.Name))
		}
		if m.State == clusterstate.StateJoining {
			return localapi.RotateResult{}, refusedErr(fmt.Sprintf("member %s is joining; no key rotation until that join has finished; nothing was written", m.Name))
		}
	}
	if len(view.Pendings) > 0 {
		return localapi.RotateResult{}, refusedErr("an admission is pending; no key rotation until it is used, withdrawn or expired; nothing was written")
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return localapi.RotateResult{}, err
	}
	// Step 1: the new key, on disk before anything names it.
	newPin, err := nodekey.CreateNext(s.dataDir)
	if err != nil {
		return localapi.RotateResult{}, err
	}
	if s.rotateStepDone(rotStopAfterNext) {
		return localapi.RotateResult{}, errStopped
	}
	// Step 2: the record.
	r := RotateState{OldPin: self.Pin, NewPin: newPin, OpID: opID, Started: s.now().UTC().Format(time.RFC3339), Phase: PhaseRotating}
	if err := writeRotate(s.dataDir, r); err != nil {
		_ = layout.RemoveFile(filepath.Join(s.dataDir, nodekey.NextKeyFile))
		return localapi.RotateResult{}, err
	}
	// This request makes the first attempt; the step loop waits.
	s.mu.Lock()
	s.nextRotate = s.now().Add(rotateRetry)
	s.mu.Unlock()
	s.setRotation(&r)
	s.event(audit.EventKeyRotation, map[string]string{"step": "started", "old_pin": r.OldPin, "new_pin": r.NewPin, "op_id": r.OpID, "actor": actor})
	s.log.Info("key rotation: started", "old_pin", r.OldPin, "new_pin", r.NewPin, "op_id", r.OpID)
	if s.rotateStepDone(rotStopAfterRecord) {
		return localapi.RotateResult{}, errStopped
	}
	// Step 3, first attempt.
	res := localapi.RotateResult{OldPin: r.OldPin, NewPin: r.NewPin, OpID: r.OpID, Phase: r.Phase}
	idx, refusal, err := s.askKeyAdd(ctx, r, actor)
	switch {
	case refusal != nil:
		s.rotationRefused(r, refusal.Message)
		return localapi.RotateResult{}, refusal
	case err != nil:
		res.Pending = "the leader was asked and no answer came; this node asks again every 10 s: " + err.Error()
	default:
		res.AddIndex = idx
	}
	s.mu.Lock()
	s.nextRotate = s.now().Add(rotateRetry)
	s.mu.Unlock()
	s.rotateStepDone(rotStopAfterAdd)
	return res, nil
}

// AbortRotation implements localapi.Backend: "skep-cluster key rotate
// --abort", root only, only while this node still uses its old key.
func (s *Service) AbortRotation(ctx context.Context, actor string) (localapi.RotateResult, error) {
	if s.isRemoved() {
		return localapi.RotateResult{}, s.removedError()
	}
	s.rotMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.rotMu.Unlock()
		}
	}()
	r := s.rotationState()
	if r == nil {
		return localapi.RotateResult{}, rotationErr(http.StatusConflict, "no_rotation", "this node has no key rotation under way (no rotate.json)")
	}
	if s.key.Pin() == r.NewPin {
		return localapi.RotateResult{}, refusedErr("this node already uses its new key (pin " + r.NewPin + "); the rotation can no longer be aborted, it ends by itself")
	}
	if r.Phase != PhaseAborting {
		r.Phase = PhaseAborting
		s.mu.Lock()
		s.nextRotate = s.now().Add(rotateRetry)
		s.mu.Unlock()
		// From here on the switch (step 4) never runs.
		if err := writeRotate(s.dataDir, *r); err != nil {
			return localapi.RotateResult{}, err
		}
		s.setRotation(r)
		s.event(audit.EventKeyRotation, map[string]string{"step": "abort requested", "new_pin": r.NewPin, "op_id": r.OpID, "actor": actor})
		s.log.Info("key rotation: abort requested", "new_pin", r.NewPin)
		if s.rotateStepDone(rotStopAfterAbort) {
			return localapi.RotateResult{}, errStopped
		}
	}
	s.rotMu.Unlock()
	locked = false
	res := localapi.RotateResult{OldPin: r.OldPin, NewPin: r.NewPin, OpID: r.OpID, Phase: r.Phase}
	if err := s.abortStep(ctx, *r, actor); err != nil {
		res.Pending = "the leader was asked and no answer came; this node asks again every 10 s: " + err.Error()
	}
	s.mu.Lock()
	s.nextRotate = s.now().Add(rotateRetry)
	s.mu.Unlock()
	return res, nil
}

// askKeyAdd asks the leader to add the next pin. It returns the add
// index, or the leader's refusal, or an error for an unknown outcome.
func (s *Service) askKeyAdd(ctx context.Context, r RotateState, actor string) (uint64, *localapi.OpError, error) {
	req := keyAddRequest{OpID: r.OpID, NextPin: r.NewPin, Actor: actor}
	var ans keyAddAnswer
	var err error
	if s.getStore().IsLeader() {
		var res any
		res, err = s.keyAddAsLeader(s.selfPeer(), req)
		if err == nil {
			ans = res.(keyAddAnswer)
		}
	} else {
		err = s.toLeader(ctx, raftnet.OpKeyAdd, req, &ans)
	}
	if err == nil {
		return ans.AddIndex, nil, nil
	}
	var oe *localapi.OpError
	var pe *raftnet.PeerError
	switch {
	case errors.As(err, &oe) && (oe.Code == "refused" || oe.Code == "rotation_in_progress"):
		return 0, oe, nil
	case errors.As(err, &pe) && (pe.Code == raftnet.CodeRefused || pe.Code == "rotation_in_progress"):
		return 0, rotationErr(http.StatusConflict, pe.Code, pe.Message), nil
	}
	return 0, nil, err
}

func (s *Service) selfPeer() raftnet.Peer {
	return raftnet.Peer{NodeID: s.cfg.NodeID, Name: s.cfg.NodeName, Address: s.cfg.PeerAddress(), Pin: s.key.Pin(), Kind: raftnet.KindMember}
}

// rotationRefused undoes steps 1 and 2: node.key.next first, then
// rotate.json. Nothing else changed.
func (s *Service) rotationRefused(r RotateState, why string) {
	if err := layout.RemoveFile(filepath.Join(s.dataDir, nodekey.NextKeyFile)); err != nil {
		s.log.Error("key rotation refused, and node.key.next could not be deleted; the next start deletes it", "error", err)
		return
	}
	if s.rotateStepDone(rotStopAfterNextRm) {
		return
	}
	if err := layout.RemoveFile(filepath.Join(s.dataDir, layout.RotateFile)); err != nil {
		s.log.Error("key rotation refused, and rotate.json could not be deleted", "error", err)
		return
	}
	s.setRotation(nil)
	s.event(audit.EventKeyRotation, map[string]string{"step": "refused", "new_pin": r.NewPin, "op_id": r.OpID, "reason": why})
	s.log.Warn("key rotation refused; nothing changed", "reason", why)
}

// rotationEnded deletes what is left of a rotation that ended: the next
// key (an abort), then rotate.json.
func (s *Service) rotationEnded(r RotateState, step string) {
	if err := layout.RemoveFile(filepath.Join(s.dataDir, nodekey.NextKeyFile)); err != nil {
		s.log.Error("key rotation ended, and node.key.next could not be deleted; retrying", "error", err)
		return
	}
	if step == "aborted" && s.rotateStepDone(rotStopAfterNextRm) {
		return
	}
	if err := layout.RemoveFile(filepath.Join(s.dataDir, layout.RotateFile)); err != nil {
		s.log.Error("key rotation ended, and rotate.json could not be deleted; retrying", "error", err)
		return
	}
	s.setRotation(nil)
	pin := r.OldPin
	if step == "done" {
		pin = r.NewPin
	}
	s.event(audit.EventKeyRotation, map[string]string{"step": step, "pin": pin, "op_id": r.OpID})
	s.log.Info("key rotation: "+step, "pin", pin)
}

// rotateStep is this node's rotation work, once a second.
func (s *Service) rotateStep(ctx context.Context) {
	r := s.rotationState()
	if r == nil || s.isFrozen() {
		return
	}
	view := s.machine.View()
	if view.Stalled != nil || s.acceptedNow().source != "state machine" {
		return
	}
	self, ok := memberOf(view, s.cfg.NodeID)
	if !ok {
		return // removed: removed mode and leave --wipe take it from here
	}
	_, added := s.machine.OpIndex(r.OpID)
	switch {
	case self.Pin == r.NewPin && self.NextPin == "":
		// Step 5: this node applied member.key_done keep new.
		s.rotationEnded(*r, "done")
		return
	case self.NextPin == "" && self.Pin == r.OldPin && added:
		// The rotation ended keeping the old key.
		if s.key.Pin() == r.NewPin {
			s.log.Error("the rotation ended keeping the old pin, but this node already switched to its new key", "old_pin", r.OldPin)
			return
		}
		s.rotationEnded(*r, "aborted")
		return
	}
	if r.Phase == PhaseAborting {
		if !s.due() {
			return
		}
		if err := s.abortStep(ctx, *r, "system"); err != nil {
			s.log.Info("key rotation: the abort's outcome is unknown; asking again in 10 s", "error", err)
		}
		return
	}
	if self.NextPin != r.NewPin {
		// Step 3: not added yet.
		if !s.due() {
			return
		}
		_, refusal, err := s.askKeyAdd(ctx, *r, "system")
		switch {
		case refusal != nil:
			s.rotationRefused(*r, refusal.Message)
		case err != nil:
			s.log.Info("key rotation: the leader's answer is unknown; asking again in 10 s", "error", err)
		}
		return
	}
	// Step 4: switch once every member has applied the add entry. The
	// decision and the switch hold rotMu, as an abort's phase write does,
	// so that a switch never follows an abort.
	if s.key.Pin() == r.NewPin {
		return
	}
	if waits := s.keyAddWaits(view, self); len(waits) > 0 {
		return
	}
	s.rotMu.Lock()
	defer s.rotMu.Unlock()
	if cur := s.rotationState(); cur == nil || cur.Phase != PhaseRotating {
		return
	}
	s.switchKey(*r)
}

// due reports whether the rotation step may ask the leader now.
func (s *Service) due() bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.nextRotate) {
		return false
	}
	s.nextRotate = now.Add(rotateRetry)
	return true
}

// keyAddWaits names the members not known (from this node's status
// rounds, its own memory of each member's highest applied index) to have
// applied the add entry of m's rotation.
func (s *Service) keyAddWaits(view clusterstate.View, m clusterstate.Member) []string {
	s.mu.Lock()
	highest := make(map[string]uint64, len(s.highest))
	for k, v := range s.highest {
		highest[k] = v
	}
	s.mu.Unlock()
	var out []string
	for _, x := range view.Members {
		hi, ok := highest[x.ID]
		if x.ID == s.cfg.NodeID {
			hi, ok = view.AppliedIndex, true
		}
		if !ok || hi < m.KeyAddIndex {
			out = append(out, x.Name)
		}
	}
	sort.Strings(out)
	return out
}

// switchKey is step 4: node.key.next becomes node.key (the old private key
// is gone from the disk at the rename: every member has durably applied
// the new pin), a certificate is issued for it, and the listener and the
// diallers use it from their next handshake; open connections are closed.
func (s *Service) switchKey(r RotateState) {
	next := filepath.Join(s.dataDir, nodekey.NextKeyFile)
	if err := os.Rename(next, filepath.Join(s.dataDir, nodekey.KeyFile)); err != nil {
		s.log.Error("key rotation: renaming node.key.next failed; retrying", "error", err)
		return
	}
	if err := layout.SyncDir(s.dataDir); err != nil {
		s.log.Error("key rotation: fsync of the data directory failed after the rename", "error", err)
	}
	if s.rotateStepDone(rotStopAfterRename) {
		return
	}
	k, _, err := nodekey.LoadRepair(s.dataDir, s.cfg.NodeID, s.now())
	if err != nil {
		s.log.Error("key rotation: the new key cannot be loaded; the next start repairs its certificate", "error", err)
		return
	}
	if k.Pin() != r.NewPin {
		s.log.Error("key rotation: node.key does not have the new pin after the rename", "pin", k.Pin(), "new_pin", r.NewPin)
		return
	}
	s.key.Adopt(k)
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	if l != nil {
		l.CloseAll()
	}
	s.event(audit.EventKeyRotation, map[string]string{"step": "switched", "new_pin": r.NewPin, "op_id": r.OpID})
	s.log.Info("key rotation: this node uses its new key now", "pin", r.NewPin)
	s.rotateStepDone(rotStopAfterSwitch)
}

// abortStep asks the leader to end the rotation keeping the old key, and
// deletes the files once the answer says the next pin was never added.
func (s *Service) abortStep(ctx context.Context, r RotateState, actor string) error {
	req := keyAbortRequest{OpID: r.OpID, NextPin: r.NewPin, Actor: actor}
	var ans keyAbortAnswer
	var err error
	if s.getStore().IsLeader() {
		var res any
		res, err = s.keyAbortAsLeader(s.selfPeer(), req)
		if err == nil {
			ans = res.(keyAbortAnswer)
		}
	} else {
		err = s.toLeader(ctx, raftnet.OpKeyAbort, req, &ans)
	}
	if err != nil {
		return err
	}
	switch ans.Result {
	case abortNotAdded:
		s.rotationEnded(r, "aborted")
	case abortFinished:
		// The member entry decides at the next step (done or aborted).
	}
	return nil
}

// handleKeyAdd is the leader's side of key-add: only from a member key,
// only for the node that key belongs to.
func (s *Service) handleKeyAdd(from raftnet.Peer, body json.RawMessage) (any, error) {
	var req keyAddRequest
	if err := strictJSON(body, &req); err != nil || !opIDRE.MatchString(req.OpID) || !clusterstate.ValidPin(req.NextPin) || req.Actor == "" {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the key-add request is not valid"}
	}
	res, err := s.keyAddAsLeader(from, req)
	return res, toPeerError(err)
}

func (s *Service) keyAddAsLeader(from raftnet.Peer, req keyAddRequest) (any, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if from.Kind != raftnet.KindMember {
		return nil, refusedErr("only a member rotates its key")
	}
	view := s.machine.View()
	mem, ok := memberOf(view, from.NodeID)
	if !ok || mem.State != clusterstate.StateMember {
		return nil, refusedErr("this node is not a member in state member")
	}
	if mem.NextPin == req.NextPin {
		return keyAddAnswer{AddIndex: mem.KeyAddIndex}, nil // a repeat
	}
	if _, done := s.machine.OpIndex(req.OpID); done {
		return nil, refusedErr("the rotation with this operation ID has already ended")
	}
	for _, m := range view.Members {
		if m.NextPin != "" {
			return nil, rotationErr(http.StatusConflict, "rotation_in_progress", fmt.Sprintf("member %s is rotating its key: one rotation in the cluster at a time", m.Name))
		}
		if m.State == clusterstate.StateJoining {
			return nil, refusedErr(fmt.Sprintf("member %s is joining; no key rotation until that join has finished", m.Name))
		}
	}
	if len(view.Pendings) > 0 {
		return nil, refusedErr("an admission is pending; no key rotation until it is used, withdrawn or expired")
	}
	e, err := clusterstate.NewEntry(clusterstate.TypeMemberKeyAdd, req.OpID, req.Actor, from.NodeID,
		clusterstate.MemberKeyAdd{NodeID: from.NodeID, NextPin: req.NextPin, At: s.now().Unix()})
	if err != nil {
		return nil, err
	}
	if _, err := s.getStore().Apply(e); err != nil {
		return nil, applyError(err)
	}
	mem, _ = s.machine.Member(from.NodeID)
	if mem.NextPin != req.NextPin {
		return nil, refusedErr("the key-add entry was written, but the member entry has no such next pin")
	}
	return keyAddAnswer{AddIndex: mem.KeyAddIndex}, nil
}

// handleKeyAbort is the leader's side of key-abort.
func (s *Service) handleKeyAbort(from raftnet.Peer, body json.RawMessage) (any, error) {
	var req keyAbortRequest
	if err := strictJSON(body, &req); err != nil || !opIDRE.MatchString(req.OpID) || !clusterstate.ValidPin(req.NextPin) || req.Actor == "" {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the key-abort request is not valid"}
	}
	res, err := s.keyAbortAsLeader(from, req)
	return res, toPeerError(err)
}

// keyAbortAsLeader: after a barrier (every entry of earlier terms that will
// ever commit has), a next pin present is ended with member.key_done keep
// old; an add that is not applied never will be ("not_added").
func (s *Service) keyAbortAsLeader(from raftnet.Peer, req keyAbortRequest) (any, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if from.Kind != raftnet.KindMember {
		return nil, refusedErr("only a member aborts its key rotation")
	}
	if err := s.getStore().Barrier(); err != nil {
		return nil, applyError(fmt.Errorf("%w (%v)", store.ErrOutcomeUnknown, err))
	}
	mem, ok := s.machine.Member(from.NodeID)
	if !ok {
		return nil, refusedErr("this node is not a member")
	}
	if mem.NextPin != "" && from.Pin == mem.NextPin {
		return nil, refusedErr("this node already uses its new key; the rotation can no longer be aborted")
	}
	if mem.NextPin == req.NextPin {
		e, err := clusterstate.NewEntry(clusterstate.TypeMemberKeyDone, clusterstate.KeyDoneOpID(mem.ID, mem.KeyAddIndex, clusterstate.KeepOld),
			req.Actor, from.NodeID, clusterstate.MemberKeyDone{NodeID: mem.ID, Keep: clusterstate.KeepOld, AddIndex: mem.KeyAddIndex})
		if err != nil {
			return nil, err
		}
		if _, err := s.getStore().Apply(e); err != nil {
			return nil, applyError(err)
		}
		return keyAbortAnswer{Result: abortProposed}, nil
	}
	if _, done := s.machine.OpIndex(req.OpID); done {
		return keyAbortAnswer{Result: abortFinished}, nil
	}
	return keyAbortAnswer{Result: abortNotAdded}, nil
}

// proposeKeyDone is the leader's rotation step (after removals, before
// expiries): member.key_done keep new for a member whose next pin every
// member has applied (by each member's highest answered applied index) and
// that the leader has seen use its new key in its most recent connection
// to or from it.
func (s *Service) proposeKeyDone(view clusterstate.View) bool {
	for _, m := range view.Members {
		if m.NextPin == "" || m.State != clusterstate.StateMember {
			continue
		}
		if len(s.keyAddWaits(view, m)) > 0 || !s.seenNewKey(m) {
			continue
		}
		e, err := clusterstate.NewEntry(clusterstate.TypeMemberKeyDone, clusterstate.KeyDoneOpID(m.ID, m.KeyAddIndex, clusterstate.KeepNew),
			clusterstate.ActorSystem, s.cfg.NodeID, clusterstate.MemberKeyDone{NodeID: m.ID, Keep: clusterstate.KeepNew, AddIndex: m.KeyAddIndex})
		if err == nil {
			_, err = s.getStore().Apply(e)
		}
		if err != nil {
			s.log.Warn("leader: ending a key rotation failed; retrying", "node_id", m.ID, "error", err)
		} else {
			s.log.Info("leader: key rotation ended; the old pin is dropped", "node_id", m.ID, "name", m.Name, "pin", m.NextPin)
		}
		return true
	}
	return false
}

// seenNewKey: the node (this one: its own key) last used its next pin.
func (s *Service) seenNewKey(m clusterstate.Member) bool {
	if m.ID == s.cfg.NodeID {
		return s.key.Pin() == m.NextPin
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[m.ID].pin == m.NextPin
}

// rotationProgress says, for each member with a next pin, what its
// rotation waits for, as this node sees it (health key_rotation).
func (s *Service) rotationProgress(view clusterstate.View) []string {
	var out []string
	for _, m := range view.Members {
		if m.NextPin == "" {
			continue
		}
		if waits := s.keyAddWaits(view, m); len(waits) > 0 {
			out = append(out, fmt.Sprintf("the key rotation of %s waits for %s to apply its new pin (index %d)", m.Name, joinNames(waits), m.KeyAddIndex))
			continue
		}
		switch {
		case m.ID == s.cfg.NodeID && s.key.Pin() != m.NextPin:
			out = append(out, fmt.Sprintf("the key rotation of %s (this node): every member has applied the new pin; this node switches to its new key", m.Name))
		case s.getStore().IsLeader() && !s.seenNewKey(m):
			out = append(out, fmt.Sprintf("the key rotation of %s waits for this node, the leader, to see it use its new key", m.Name))
		default:
			out = append(out, fmt.Sprintf("the key rotation of %s waits for the leader to drop the old pin", m.Name))
		}
	}
	return out
}

// pinSeen is the pin of a node's last connection to or from this node.
type pinSeen struct {
	pin string
	at  time.Time
}

// keysSeen lists, per member other than this node, the pin it last used.
func (s *Service) keysSeen(view clusterstate.View) []localapi.KeySeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []localapi.KeySeen
	for _, m := range view.Members {
		if p, ok := s.seen[m.ID]; ok && m.ID != s.cfg.NodeID {
			out = append(out, localapi.KeySeen{NodeID: m.ID, Name: m.Name, Pin: p.pin, At: p.at.UTC()})
		}
	}
	return out
}

// rotationStatus is this node's rotation for status.
func (s *Service) rotationStatus() *localapi.RotationStatus {
	r := s.rotationState()
	if r == nil {
		return nil
	}
	return &localapi.RotationStatus{Phase: r.Phase, OldPin: r.OldPin, NewPin: r.NewPin, OpID: r.OpID, Started: r.Started, Using: s.key.Pin()}
}
