// Package membership is the applied-membership file,
// /var/db/skep/cluster/membership.json (ADR 0013 amendment 1, point 5).
//
// The service writes it, as the skep-cluster account, whenever an applied
// entry changes the membership, before that index is reported as applied
// and before the listener uses the new membership. At start the node
// accepts keys by this file until its state machine has reached the file's
// index. The file never goes back to a lower (epoch, index), and it holds
// no pending entries.
package membership

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// FileName is the file's name in the data directory.
const FileName = "membership.json"

// Mode is the file's mode.
const Mode fs.FileMode = 0o600

// maxSize bounds the file read at start.
const maxSize = 1 << 20

// format is the version of the file's encoding.
const format = 1

// Member is one member entry as the file holds it: ID, name, address, both
// pins and the state.
type Member struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Pin     string `json:"pin"`
	APIPin  string `json:"api_pin,omitempty"`
	State   string `json:"state"`
}

// File is the file's content.
type File struct {
	Format    int      `json:"format"`
	ClusterID string   `json:"cluster_id"`
	Epoch     uint64   `json:"epoch"`
	Index     uint64   `json:"index"`
	Members   []Member `json:"members"`
}

// Less reports whether f is older than (epoch, index).
func (f File) less(epoch, index uint64) bool {
	return f.Epoch < epoch || (f.Epoch == epoch && f.Index < index)
}

// FromMembership converts an applied membership.
func FromMembership(m clusterstate.Membership) File {
	f := File{Format: format, ClusterID: m.ClusterID, Epoch: m.Epoch, Index: m.Index, Members: []Member{}}
	for _, mem := range m.Members {
		f.Members = append(f.Members, Member{ID: mem.ID, Name: mem.Name, Address: mem.Address, Pin: mem.Pin, APIPin: mem.APIPin, State: mem.State})
	}
	return f
}

// Read reads the file in dir. It returns (nil, nil) when there is none.
func Read(dir string) (*File, error) {
	path := filepath.Join(dir, FileName)
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxSize+1))
	if err != nil {
		return nil, err
	}
	n := len(b)
	if n > maxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSize)
	}
	dec := json.NewDecoder(bytes.NewReader(b[:n]))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", path)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

func (f File) validate() error {
	if f.Format != format {
		return fmt.Errorf("format %d is unknown to this build", f.Format)
	}
	if !clusterstate.ValidNodeID(f.ClusterID) || f.Epoch == 0 || f.Index == 0 {
		return errors.New("cluster ID, epoch or index missing")
	}
	seen := map[string]bool{}
	for _, m := range f.Members {
		if !clusterstate.ValidNodeID(m.ID) || !clusterstate.ValidPin(m.Pin) || seen[m.ID] || seen[m.Pin] {
			return fmt.Errorf("member %q is not valid or duplicated", m.ID)
		}
		if m.State != clusterstate.StateJoining && m.State != clusterstate.StateMember && m.State != clusterstate.StateRemoving {
			return fmt.Errorf("member %s has state %q", m.ID, m.State)
		}
		if m.Address != "" {
			if err := clusterstate.CheckAddress(m.Address); err != nil {
				return err
			}
			if seen["addr:"+m.Address] {
				return fmt.Errorf("address %s is duplicated", m.Address)
			}
			seen["addr:"+m.Address] = true
		}
		seen[m.ID], seen[m.Pin] = true, true
	}
	return nil
}

// Writer keeps the file in one directory. It never writes a lower (epoch,
// index) than the file it holds.
type Writer struct {
	dir string
	mu  sync.Mutex
	cur *File
	// beforeRename is a test hook run between the temporary file's fsync
	// and the rename.
	beforeRename func() error
}

// NewWriter returns a writer for dir, starting from the file there.
func NewWriter(dir string) (*Writer, error) {
	f, err := Read(dir)
	if err != nil {
		return nil, err
	}
	return &Writer{dir: dir, cur: f}, nil
}

// Current returns the file as last read or written, or nil.
func (w *Writer) Current() *File {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	c := *w.cur
	c.Members = append([]Member(nil), w.cur.Members...)
	return &c
}

// Write makes m durable, unless the file already reflects the same or a
// later (epoch, index): then it changes nothing and returns nil.
func (w *Writer) Write(m clusterstate.Membership) error {
	f := FromMembership(m)
	if err := f.validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur != nil && !w.cur.less(f.Epoch, f.Index) {
		return nil
	}
	if err := writeAtomic(w.dir, f, w.beforeRename); err != nil {
		return err
	}
	w.cur = &f
	return nil
}

// writeAtomic writes the file: a temporary file in the same directory,
// fsync, rename, fsync of the directory.
func writeAtomic(dir string, f File, beforeRename func() error) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, Mode)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		fh.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := fh.Write(b); err != nil {
		return fail(err)
	}
	if err := fh.Chmod(Mode); err != nil {
		return fail(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("write %s: open the directory for fsync: %w", path, err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: fsync of the directory: %w", path, err)
	}
	return nil
}
