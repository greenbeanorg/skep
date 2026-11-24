// Package clusterstate is the replicated state of a Skep cluster and the
// log entries that change it (ADR 0013 A7, B5). It knows nothing about
// Raft: the store package feeds it committed entries in order.
//
// Step 8a has the cluster ID and the membership, with this node as the
// only member.
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

// Entry types of step 8a.
const (
	// TypeClusterInit creates the cluster: its ID and its first member.
	TypeClusterInit = "cluster.init"
	// TypeMemberAddress changes a member's address. In 8a only a
	// one-node cluster changes its own ("skep-cluster listen").
	TypeMemberAddress = "member.address"
)

// Supported is, per entry type, the highest version this build applies.
// Until the feature-level operation exists (step 8g) every build writes
// version 1 of each type.
var Supported = map[string]int{
	TypeClusterInit:   1,
	TypeMemberAddress: 1,
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
	if err := e.validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// decodeData reads an entry's data strictly.
func decodeData(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
