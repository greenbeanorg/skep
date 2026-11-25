package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// CreateCluster makes the node key (or takes the one on disk, repairing
// its certificate) and a store in state/ with this node as the only
// member of a cluster with the given ID ("" makes a new one). It runs as
// the service account ("init", and the last step of "leave --wipe").
func CreateCluster(ctx context.Context, dataDir string, cfg clusterconf.Config, clusterID string, now time.Time) (string, string, error) {
	var key *nodekey.Key
	var err error
	if nodekey.Exists(dataDir) {
		if key, _, err = nodekey.LoadRepair(dataDir, cfg.NodeID, now); err != nil {
			return "", "", fmt.Errorf("%w (left by an interrupted init; remove %s and %s as root to start over)",
				err, nodekey.KeyFile, nodekey.CertFile)
		}
	} else if key, err = nodekey.Create(dataDir, cfg.NodeID, now); err != nil {
		return "", "", err
	}
	if clusterID == "" {
		if clusterID, err = ids.NewUUID(); err != nil {
			return "", "", err
		}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return "", "", err
	}
	entry, err := clusterstate.NewEntry(clusterstate.TypeClusterInit, opID, clusterstate.ActorSystem, cfg.NodeID,
		clusterstate.ClusterInit{ClusterID: clusterID, Member: clusterstate.Member{
			ID: cfg.NodeID, Name: cfg.NodeName, Address: cfg.PeerAddress(), Pin: key.Pin(), State: clusterstate.StateMember,
		}})
	if err != nil {
		return "", "", err
	}
	stateDir := layout.State(dataDir)
	// Only commands run this (init, the last step of a wipe): the
	// library's start-up lines must not reach the terminal.
	opts := store.Options{Dir: stateDir, NodeID: cfg.NodeID, Quiet: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fail := func(err error) (string, string, error) {
		// Leave no half-made store: the next attempt starts clean.
		_ = layout.RemoveAll(stateDir)
		return "", "", err
	}
	if err := store.Bootstrap(opts, cfg.PeerAddress()); err != nil {
		return fail(err)
	}
	// The applied-membership file is written with the first entry, so
	// that a later wipe or status finds the cluster ID without the store.
	writer, err := membership.NewWriter(stateDir)
	if err != nil {
		return fail(err)
	}
	machine := clusterstate.New()
	machine.SetMembershipWriter(writer.Write)
	opts.Machine = machine
	node, err := store.Open(opts)
	if err != nil {
		return fail(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := node.WaitLeader(wctx); err != nil {
		node.Close()
		return fail(err)
	}
	if _, err := node.Apply(entry); err != nil {
		node.Close()
		return fail(fmt.Errorf("writing the first entry: %w", err))
	}
	if err := node.Close(); err != nil {
		return fail(err)
	}
	return clusterID, key.Pin(), nil
}

// WipeMarker is wipe.json (decision D8 of step 8b-2a): written first and
// removed last by "skep-cluster leave --wipe". While it exists the service
// refuses to start and the command, run again, finishes without asking.
type WipeMarker struct {
	Format int `json:"format"`
	// NewClusterID is the ID of the one-node cluster the wipe makes, the
	// same on every run of the command.
	NewClusterID string `json:"new_cluster_id"`
	// OldPin is the pin of the key before the wipe; the wipe replaces a
	// key with this pin and keeps any other (one it made itself). It is
	// empty when KeyUnreadable is set.
	OldPin string `json:"old_pin"`
	// KeyUnreadable is set when node.key could not be loaded when the
	// wipe began (missing, wrong mode, not a key, no certificate that
	// can be repaired): every run of the wipe then replaces the key.
	KeyUnreadable bool   `json:"key_unreadable,omitempty"`
	Started       string `json:"started"`
}

const wipeFormat = 1

// ReadWipeMarker reads wipe.json; (nil, nil) without one.
func ReadWipeMarker(dataDir string) (*WipeMarker, error) {
	path := filepath.Join(dataDir, layout.WipeMarker)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m WipeMarker
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pinOK := clusterstate.ValidPin(m.OldPin) && !m.KeyUnreadable || m.OldPin == "" && m.KeyUnreadable
	if m.Format != wipeFormat || !clusterstate.ValidNodeID(m.NewClusterID) || !pinOK {
		return nil, fmt.Errorf("%s: not a valid wipe marker", path)
	}
	return &m, nil
}

func writeFileAtomic(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = fh.Write(append(b, '\n'))
	if err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return layout.SyncDir(dir)
}

// WipeInfo says what "leave --wipe" would wipe: the cluster ID the node
// has now, from (in this order) state/removed.json, state/membership.json,
// join.json, or the store of a node that is alone; and whether a wipe is
// already under way (the marker exists), in which case the ID is the new
// one and nothing is asked again.
func WipeInfo(dataDir, nodeID string) (clusterID string, resume bool, err error) {
	return wipeInfo(dataDir, nodeID, false)
}

// WipeInfoFiles is WipeInfo from the files alone, which can be read while
// the service runs; an empty ID without an error: only the store can tell.
func WipeInfoFiles(dataDir string) (clusterID string, resume bool, err error) {
	return wipeInfo(dataDir, "", true)
}

func wipeInfo(dataDir, nodeID string, filesOnly bool) (clusterID string, resume bool, err error) {
	if err := layout.CheckOldLayout(dataDir); err != nil {
		return "", false, err
	}
	if m, err := ReadWipeMarker(dataDir); err != nil || m != nil {
		if err != nil {
			return "", false, err
		}
		return m.NewClusterID, true, nil
	}
	if r, err := ReadRemoved(dataDir); err == nil && r != nil {
		return r.ClusterID, false, nil
	}
	if f, err := membership.Read(layout.State(dataDir)); err == nil && f != nil {
		return f.ClusterID, false, nil
	}
	if js, err := joinstate.Read(dataDir); err == nil && js != nil {
		return js.Admission.ClusterID, false, nil
	}
	if filesOnly {
		return "", false, nil
	}
	if store.Exists(layout.State(dataDir)) {
		v, _, err := store.ReadState(layout.State(dataDir), nodeID)
		if err == nil && v.ClusterID != "" {
			return v.ClusterID, false, nil
		}
	}
	return "", false, errors.New("no cluster state was found in " + dataDir + " (is this host initialised?)")
}

// WipeBegin writes the marker when it is missing (a new cluster ID and the
// key's pin now) and returns it; with a marker present it returns that one.
// A node key that cannot be loaded does not stop the wipe: the marker then
// has an empty old pin and says the key was unreadable, and WipeFinish
// replaces the key.
func WipeBegin(dataDir string, now time.Time) (WipeMarker, error) {
	if m, err := ReadWipeMarker(dataDir); err != nil || m != nil {
		if err != nil {
			return WipeMarker{}, err
		}
		return *m, nil
	}
	id, err := ids.NewUUID()
	if err != nil {
		return WipeMarker{}, err
	}
	m := WipeMarker{Format: wipeFormat, NewClusterID: id, Started: now.UTC().Format(time.RFC3339)}
	if key, err := nodekey.Load(dataDir); err == nil {
		m.OldPin = key.Pin()
	} else {
		m.KeyUnreadable = true
	}
	if err := writeFileAtomic(dataDir, layout.WipeMarker, m); err != nil {
		return WipeMarker{}, err
	}
	return m, nil
}

// Crash points of WipeFinish (stop hooks for the tests).
const (
	wipeStopAfterStandalone = "after the moved-aside states are deleted"
	wipeStopAfterJoin       = "after join.json is deleted"
	wipeStopAfterState      = "after state/ is deleted"
	wipeStopAfterKey        = "after the new key"
	wipeStopAfterCluster    = "after the new one-node cluster, before the marker is removed"
)

// errStopped is returned by a stop hook.
var errStopped = errors.New("stopped by a test")

// WipeFinish does the file steps of "leave --wipe" after WipeBegin and
// after root has removed the listen address from cluster.json: it deletes
// every moved-aside state and join.json (before state/, decision D8),
// then state/, replaces the node key if it is still the old one, makes a
// one-node cluster with the marker's cluster ID and no address, and
// removes the marker last. Each step can be repeated, so the command can
// be run again after a crash at any point.
func WipeFinish(ctx context.Context, dataDir string, cfg clusterconf.Config, now time.Time) (string, string, error) {
	return wipeFinish(ctx, dataDir, cfg, now, nil)
}

func wipeFinish(ctx context.Context, dataDir string, cfg clusterconf.Config, now time.Time, stop func(string) bool) (string, string, error) {
	stopped := func(p string) bool { return stop != nil && stop(p) }
	m, err := ReadWipeMarker(dataDir)
	if err != nil {
		return "", "", err
	}
	if m == nil {
		return "", "", errors.New("there is no wipe marker: the wipe was not begun")
	}
	cfg.ListenAddress = "" // a wiped node has no listener (D8)
	aside, err := layout.Standalones(dataDir)
	if err != nil {
		return "", "", err
	}
	for _, a := range aside {
		if err := joinstate.DeleteStandalone(dataDir, a); err != nil {
			return "", "", err
		}
	}
	if stopped(wipeStopAfterStandalone) {
		return "", "", errStopped
	}
	if err := joinstate.Remove(dataDir); err != nil {
		return "", "", err
	}
	if stopped(wipeStopAfterJoin) {
		return "", "", errStopped
	}
	if err := layout.RemoveAll(layout.State(dataDir)); err != nil {
		return "", "", err
	}
	if stopped(wipeStopAfterState) {
		return "", "", errStopped
	}
	// A new key, unless the one on disk is already not the old one (made
	// by an earlier run of this wipe).
	replace := true
	if nodekey.Exists(dataDir) && !m.KeyUnreadable {
		if k, err := nodekey.Load(dataDir); err == nil && k.Pin() != m.OldPin {
			replace = false
		} else if err != nil {
			// A key without its certificate: a new key is made all
			// the same unless its pin can be read.
			if k, _, lerr := nodekey.LoadRepair(dataDir, cfg.NodeID, now); lerr == nil && k.Pin() != m.OldPin {
				replace = false
			}
		}
	}
	if replace {
		if err := layout.RemoveFile(filepath.Join(dataDir, nodekey.CertFile)); err != nil {
			return "", "", err
		}
		if err := layout.RemoveFile(filepath.Join(dataDir, nodekey.KeyFile)); err != nil {
			return "", "", err
		}
		if _, err := nodekey.Create(dataDir, cfg.NodeID, now); err != nil {
			return "", "", err
		}
		if err := layout.SyncDir(dataDir); err != nil {
			return "", "", err
		}
	}
	if stopped(wipeStopAfterKey) {
		return "", "", errStopped
	}
	clusterID, pin, err := CreateCluster(ctx, dataDir, cfg, m.NewClusterID, now)
	if err != nil {
		return "", "", err
	}
	if pin == m.OldPin {
		return "", "", errors.New("the new key has the old pin; the wipe is not finished")
	}
	if stopped(wipeStopAfterCluster) {
		return "", "", errStopped
	}
	if err := layout.RemoveFile(filepath.Join(dataDir, layout.WipeMarker)); err != nil {
		return "", "", err
	}
	return clusterID, pin, nil
}
