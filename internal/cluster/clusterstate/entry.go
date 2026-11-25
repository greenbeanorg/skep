// Package clusterstate is the replicated state of a Skep cluster and the
// log entries that change it (ADR 0013 A7, B5). It knows nothing about
// Raft: the store package feeds it committed entries in order.
//
// It holds the cluster ID, the membership (member entries in state
// "joining" or "member") and the pending entries of admitted hosts that
// have not joined yet (ADR 0013 B1, step 8b).
//
// Applying an entry is deterministic: it reads no clock, no file and does
// not depend on map order. Every time an entry needs is carried in the
// entry, written there by the leader that proposed it.
package clusterstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Entry is the envelope of every Skep log entry. Every entry carries a
// type and a version from the first replicated one (ADR 0013 B5), an
// operation ID that makes applying it twice a no-op (B11), and who asked
// for it (B10).
type Entry struct {
	// Type names what the entry does, for example "cluster.init".
	Type string `json:"type"`
	// Version is the version of this type's data. A build applies an
	// entry only up to the version it declares in Supported.
	Version int `json:"version"`
	// OpID identifies the operation. An entry whose OpID was already
	// applied changes nothing.
	OpID string `json:"op_id"`
	// Actor is who asked for the change: a user, or "system".
	Actor string `json:"actor"`
	// Origin is the ID of the node that proposed the entry.
	Origin string `json:"origin"`
	// RequestID links the entry to the API request behind it, if any.
	RequestID string `json:"request_id,omitempty"`
	// Data is the type's own content.
	Data json.RawMessage `json:"data"`
}

// Entry types.
const (
	// TypeClusterInit creates the cluster: its ID and its first member.
	TypeClusterInit = "cluster.init"
	// TypeMemberAddress changes a member's address. Only a one-node
	// cluster changes its own ("skep-cluster listen").
	TypeMemberAddress = "member.address"
	// TypePendingCreate admits a host: a pending entry with its ID, name,
	// address and pin, and an expiry time ("skep-cluster admit").
	TypePendingCreate = "pending.create"
	// TypePendingWithdraw removes a pending entry before it is used
	// ("skep-cluster admit --withdraw").
	TypePendingWithdraw = "pending.withdraw"
	// TypePendingExpire removes a pending entry whose expiry time has
	// passed by the clock of the leader that proposed it.
	TypePendingExpire = "pending.expire"
	// TypeMemberJoin is join step 1: it consumes the pending entry and
	// creates the member entry in state "joining".
	TypeMemberJoin = "member.join"
	// TypeMemberPromoted is join step 4: the member's state becomes
	// "member" once it is a voter.
	TypeMemberPromoted = "member.promoted"
	// TypeMemberRemoving is removal step 0: the member's state becomes
	// "removing" ("skep-cluster remove"). Its keys are still accepted.
	TypeMemberRemoving = "member.removing"
	// TypeMemberRemoved is removal step 2: the member entry and its pins
	// are removed, after the Raft configuration without it is committed.
	TypeMemberRemoved = "member.removed"
	// TypeMemberKeyAdd adds a second pin (next_pin) to a member: the
	// rotation of its node key begins (step 8b-2b, D1). Both pins are
	// accepted as that node.
	TypeMemberKeyAdd = "member.key_add"
	// TypeMemberKeyDone ends a rotation: keep "new" makes next_pin the
	// pin, keep "old" drops next_pin.
	TypeMemberKeyDone = "member.key_done"
)

// Key rotation outcomes of member.key_done.
const (
	KeepNew = "new"
	KeepOld = "old"
)

// Supported is, per entry type, the highest version this build applies.
// Until the feature-level operation exists (step 8g) every build writes
// version 1 of each type.
var Supported = map[string]int{
	TypeClusterInit:     1,
	TypeMemberAddress:   1,
	TypePendingCreate:   1,
	TypePendingWithdraw: 1,
	TypePendingExpire:   1,
	TypeMemberJoin:      1,
	TypeMemberPromoted:  1,
	TypeMemberRemoving:  1,
	TypeMemberRemoved:   1,
	TypeMemberKeyAdd:    1,
	TypeMemberKeyDone:   1,
}

// ActorSystem is the actor of entries no user asked for.
const ActorSystem = "system"

var opIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ClusterInit is the data of TypeClusterInit, version 1.
type ClusterInit struct {
	ClusterID string `json:"cluster_id"`
	Member    Member `json:"member"`
}

// MemberAddress is the data of TypeMemberAddress, version 1.
type MemberAddress struct {
	NodeID  string `json:"node_id"`
	Address string `json:"address"`
}

// PendingCreate is the data of TypePendingCreate, version 1. ExpiresAt is
// the proposing leader's clock plus the pending lifetime, in Unix seconds.
// Step 8b-1 carries no API pin (skep-api's node-to-node key comes with step
// 8e).
type PendingCreate struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Pin       string `json:"pin"`
	ExpiresAt int64  `json:"expires_at"`
}

// PendingWithdraw is the data of TypePendingWithdraw, version 1. Both the
// ID and the name must match the pending entry.
type PendingWithdraw struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
}

// PendingExpire is the data of TypePendingExpire, version 1. ExpiresAt
// names the pending entry it removes (a newer pending entry for the same
// ID has another expiry); At is the proposing leader's clock, which must
// have reached ExpiresAt.
type PendingExpire struct {
	NodeID    string `json:"node_id"`
	ExpiresAt int64  `json:"expires_at"`
	At        int64  `json:"at"`
}

// MemberJoin is the data of TypeMemberJoin, version 1 (join step 1). At is
// the proposing leader's clock: the pending entry must not have expired by
// then.
//
// Step 8d adds the joining host's guest list (ID, name, kind) as version 2
// of this type, so the name check and the inventory write stay in this one
// entry (ADR 0013 B3); version 1 means "no guests", which is all a host
// may have in step 8b.
type MemberJoin struct {
	NodeID string `json:"node_id"`
	Pin    string `json:"pin"`
	At     int64  `json:"at"`
}

// MemberPromoted is the data of TypeMemberPromoted, version 1 (join step 4).
type MemberPromoted struct {
	NodeID string `json:"node_id"`
}

// MemberRemoving is the data of TypeMemberRemoving, version 1 (removal
// step 0). ForgetGuests is the guest-list field of ADR 0013 B13 (the guest
// entries named with --forget-guests); version 1 carries it always empty,
// and a non-empty one is refused at apply. Step 8d, which adds the guest
// inventory, writes the guests in version 2 of this type, so that a build
// of this step stops at such an entry instead of applying it differently.
type MemberRemoving struct {
	NodeID       string   `json:"node_id"`
	ForgetGuests []string `json:"forget_guests"`
}

// MemberRemoved is the data of TypeMemberRemoved, version 1 (removal step
// 2). The leader proposes it only when the Raft configuration without the
// node is committed; applying it accepts it only for a member in state
// "removing".
type MemberRemoved struct {
	NodeID string `json:"node_id"`
}

// MemberKeyAdd is the data of TypeMemberKeyAdd, version 1. The operation
// ID is the envelope's (the rotating node's, the same on every retry); At
// is the proposing leader's clock in Unix seconds.
type MemberKeyAdd struct {
	NodeID  string `json:"node_id"`
	NextPin string `json:"next_pin"`
	At      int64  `json:"at"`
}

// MemberKeyDone is the data of TypeMemberKeyDone, version 1. AddIndex is
// the index of the member.key_add entry it ends, so that an entry of an
// earlier rotation never ends a later one. The leader writes the
// predictable operation ID keydone:<node ID>:<add index>:<new|old>.
type MemberKeyDone struct {
	NodeID   string `json:"node_id"`
	Keep     string `json:"keep"`
	AddIndex uint64 `json:"add_index"`
}

// KeyDoneOpID is the operation ID of a member.key_done entry.
func KeyDoneOpID(nodeID string, addIndex uint64, keep string) string {
	return fmt.Sprintf("keydone:%s:%d:%s", nodeID, addIndex, keep)
}

// NewEntry builds a version 1 entry of the given type.
func NewEntry(typ, opID, actor, origin string, data any) (Entry, error) {
	if _, ok := Supported[typ]; !ok {
		return Entry{}, fmt.Errorf("clusterstate: unknown entry type %q", typ)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Type: typ, Version: 1, OpID: opID, Actor: actor, Origin: origin, Data: raw}
	if err := e.validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (e Entry) validate() error {
	if e.Type == "" {
		return errors.New("clusterstate: entry without a type")
	}
	if e.Version < 1 {
		return errors.New("clusterstate: entry without a version")
	}
	if !opIDRE.MatchString(e.OpID) {
		return errors.New("clusterstate: entry without a valid operation ID")
	}
	if e.Actor == "" || e.Origin == "" {
		return errors.New("clusterstate: entry without an actor or an origin node")
	}
	return nil
}

// Encode returns the bytes written to the log.
func (e Entry) Encode() ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// Decode reads an entry from the log. It does not check that this build
// can apply it; Machine.Apply does.
func Decode(b []byte) (Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return Entry{}, fmt.Errorf("clusterstate: decode entry: %w", err)
	}
	if dec.More() {
		return Entry{}, errors.New("clusterstate: decode entry: more than one JSON value")
	}
	if err := e.validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// decodeData reads an entry's data strictly.
func decodeData(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}
