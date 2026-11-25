// Package audit is a node's cluster audit (ADR 0013 B10; D4 of step
// 8b-2b): /var/db/skep/cluster/audit/, written by skep-cluster as its
// service account, outside state/ so that a join's move-aside and a wipe
// keep it. The audit belongs to the node: its chain goes on across joins,
// abandons and wipes (the node ID stays).
//
// The audit is JSON lines in segment files audit-<8-digit number>.jsonl.
// Every record has a sequence number (per node, strictly rising, never
// reused), the local time, a kind, and prev_hash and hash: hash is the
// SHA-256 of the record's canonical encoding without hash, prev_hash
// included, so every record is chained to the one before it. The first
// record of a segment carries the last hash of the segment before it; a
// segment is sealed when its successor exists. A new segment starts when
// the UTC month changes or the file has passed 16 MiB.
//
// A log entry's record is keyed by (epoch, index) within one cluster: one
// at or below the highest key is never written again, so a replay after a
// restart writes nothing twice. Log indices start again in another
// cluster, so the key starts again at the records that change the
// node's cluster (reset marks): audit.started, node.init,
// node.wipe.finished and node.join.abandoned carry the key to go on from
// (the last index of the node's log at that moment); node.join.started
// sets it to zero and starts the join's skip: no log entry is recorded
// until this join's own member.join entry is applied (its operation ID is
// in the mark: a node that was a member of the same cluster before has
// older member.join entries in the log), or a snapshot whose member entry
// for the node carries that operation ID is installed, so the node's audit
// of a cluster starts at its join (ADR 0013 B10).
//
// One writer at a time: Open takes an exclusive, non-blocking flock on the
// audit's lock file. The running service holds it; the stages that write
// run only while the service is stopped. Verify reads without the lock and
// creates nothing.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

const (
	// DirName is the audit directory in the data directory.
	DirName = "audit"
	// DirMode and FileMode are the modes of the directory and the files.
	DirMode  fs.FileMode = 0o700
	FileMode fs.FileMode = 0o600
	// lockName is the lock file in the directory.
	lockName = ".lock"
	// MaxSegment is the size after which a new segment starts.
	MaxSegment = 16 << 20
	// maxLine bounds one record when reading.
	maxLine = 1 << 20
	// timeLayout is the local time of a record, UTC with milliseconds.
	timeLayout = "2006-01-02T15:04:05.000Z"
)

// Record kinds.
const (
	// KindEntry records a log entry as this node's state machine applied
	// it: applied, refused at apply (with the reason) or a duplicate.
	KindEntry = "entry"
	// KindStarted is the first record of an audit: the log entries up
	// to its (epoch, index) are not audited.
	KindStarted = "audit.started"
	// KindSnapshot marks a snapshot installed past the audited index:
	// the entries it covers are not audited on this node.
	KindSnapshot = "snapshot.installed"
)

// Local event kinds (records without epoch and index).
const (
	EventInit           = "node.init"
	EventJoinMovedAside = "node.join.started"
	EventJoined         = "node.join.finished"
	EventJoinAbandoned  = "node.join.abandoned"
	EventRemoved        = "node.removed"
	EventWipeBegun      = "node.wipe.begun"
	EventWipeFinished   = "node.wipe.finished"
	EventListenSet      = "node.listen.set"
	EventListenCleared  = "node.listen.cleared"
	EventNoticeDropped  = "node.notice.dropped"
	EventKeyRotation    = "node.key.rotation"
)

// Outcomes of a log entry.
const (
	OutcomeApplied   = "applied"
	OutcomeRefused   = "refused"
	OutcomeDuplicate = "duplicate"
)

// Record is one line of the audit. Hash is left out of the encoding the
// hash is computed over.
type Record struct {
	Seq       uint64 `json:"seq"`
	LocalTime string `json:"local_time"`
	Kind      string `json:"kind"`
	// Epoch and Index key a log entry (and mark audit.started and
	// snapshot.installed).
	Epoch uint64 `json:"epoch,omitempty"`
	Index uint64 `json:"index,omitempty"`
	// The log entry's own fields.
	EntryType    string `json:"entry_type,omitempty"`
	EntryVersion int    `json:"entry_version,omitempty"`
	// EntryTime is the entry's own time, in Unix seconds, where it has
	// one (the proposing leader's clock).
	EntryTime int64  `json:"entry_time,omitempty"`
	Actor     string `json:"actor,omitempty"`
	OpID      string `json:"op_id,omitempty"`
	Origin    string `json:"origin,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	// The node, name, address and pins the entry names.
	NodeID  string   `json:"node_id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Address string   `json:"address,omitempty"`
	Pins    []string `json:"pins,omitempty"`
	// Outcome is applied, refused or duplicate; Reason is why it was
	// refused.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Details are a local event's fields.
	Details map[string]string `json:"details,omitempty"`

	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash,omitempty"`
}

// canonical is the record's encoding without hash: what the hash covers.
func (r Record) canonical() ([]byte, error) {
	r.Hash = ""
	return json.Marshal(r)
}

// seal sets the hash.
func (r *Record) seal() error {
	b, err := r.canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	r.Hash = hex.EncodeToString(sum[:])
	return nil
}

// line is the record as written: the canonical encoding with the hash.
func (r Record) line() ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

var segRE = regexp.MustCompile(`^audit-([0-9]{8})\.jsonl$`)

// SegmentName returns the file name of segment n.
func SegmentName(n int) string { return fmt.Sprintf("audit-%08d.jsonl", n) }

// segments returns the segment numbers in dir, in order.
func segments(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		m := segRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// Dir returns the audit directory of a data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// Log is the open audit of this node: its only writer.
type Log struct {
	dir  string
	now  func() time.Time
	lock *os.File

	mu      sync.Mutex
	f       *os.File
	seg     int
	size    int64
	month   string
	seq     uint64
	last    string
	records uint64
	// highE, highI are the highest (epoch, index) since the last reset
	// mark.
	highE, highI uint64
	// skipNode is set during a join's skip: the node ID whose own
	// member.join ends it; skipOp is that entry's operation ID.
	skipNode string
	skipOp   string
	// failAppend is a test hook: when set, appends fail with it.
	failAppend error
	// segLimit is MaxSegment; a test lowers it.
	segLimit int64
}

// ErrLocked is returned by Open when another process holds the audit.
var ErrLocked = errors.New("another process is writing this node's cluster audit")

// Open opens (and creates, mode 0700) the audit directory of dataDir, takes
// its lock and reads the end of the chain. now defaults to time.Now.
func Open(dataDir string, now func() time.Time) (*Log, error) {
	if now == nil {
		now = time.Now
	}
	dir := Dir(dataDir)
	created := false
	if err := os.Mkdir(dir, DirMode); err == nil {
		created = true
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("audit: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("audit: %s is not a directory", dir)
	}
	if created {
		if err := os.Chmod(dir, DirMode); err != nil {
			return nil, fmt.Errorf("audit: %w", err)
		}
		if err := syncDir(dataDir); err != nil {
			return nil, fmt.Errorf("audit: %w", err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, FileMode)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := flockExclusive(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("audit: %w: %v", ErrLocked, err)
	}
	l := &Log{dir: dir, now: now, lock: lock, segLimit: MaxSegment}
	cut, err := repairTail(dir)
	if err != nil {
		l.Close()
		return nil, err
	}
	if err := l.readEnd(); err != nil {
		l.Close()
		return nil, err
	}
	if cut > 0 {
		// The cut-off record was never complete, so never reported as
		// written; the repair itself is recorded.
		if err := l.Event(EventTailRepaired, map[string]string{"bytes_removed": strconv.FormatInt(cut, 10)}); err != nil {
			l.Close()
			return nil, err
		}
	}
	return l, nil
}

// EventTailRepaired records that Open removed a cut-off last line (a
// write that a crash interrupted).
const EventTailRepaired = "audit.tail_repaired"

// repairTail removes a last line without a newline from the last segment:
// a write a crash interrupted. It returns how many bytes it removed.
func repairTail(dir string) (int64, error) {
	nums, err := segments(dir)
	if err != nil || len(nums) == 0 {
		return 0, err
	}
	path := filepath.Join(dir, SegmentName(nums[len(nums)-1]))
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return 0, nil
	}
	keep := int64(bytes.LastIndexByte(b, '\n') + 1)
	if keep == 0 {
		// A new segment whose first write was cut off.
		if err := os.Remove(path); err != nil {
			return 0, fmt.Errorf("audit: %w", err)
		}
		return int64(len(b)), syncDir(dir)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, FileMode)
	if err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(keep); err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	return int64(len(b)) - keep, nil
}

// readEnd reads every segment, checking the chain as it goes: the last
// sequence number and hash, the highest (epoch, index), the number of
// records. A broken audit is not written to: the error says to run
// "skep-cluster audit verify".
func (l *Log) readEnd() error {
	rep, err := Verify(l.dir)
	if err != nil {
		return err
	}
	if rep.Break != nil {
		return fmt.Errorf("audit: the cluster audit in %s is broken at %s line %d (%s); nothing more is written to it. "+
			"Run \"skep-cluster audit verify\" as root", l.dir, rep.Break.File, rep.Break.Line, rep.Break.Reason)
	}
	l.records = uint64(rep.Records)
	if rep.Last != nil {
		l.seq, l.last = rep.Last.Seq, rep.Last.Hash
	}
	l.highE, l.highI = rep.HighEpoch, rep.HighIndex
	l.skipNode, l.skipOp = rep.SkipNode, rep.SkipOp
	if n := len(rep.Segments); n > 0 {
		l.seg = rep.segNums[n-1]
		path := filepath.Join(l.dir, SegmentName(l.seg))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		l.size = info.Size()
		l.month = rep.lastMonth
	}
	return nil
}

// Close closes the audit and gives up its lock.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.f != nil {
		err = l.f.Close()
		l.f = nil
	}
	if l.lock != nil {
		if cerr := l.lock.Close(); err == nil {
			err = cerr
		}
		l.lock = nil
	}
	return err
}

// Empty reports whether the audit holds no record.
func (l *Log) Empty() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.records == 0
}

// Highest returns the highest (epoch, index) in the audit.
func (l *Log) Highest() (epoch, index uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.highE, l.highI
}

func after(e1, i1, e2, i2 uint64) bool { return e1 > e2 || (e1 == e2 && i1 > i2) }

// append writes one record: the sequence number, the time, the chain; then
// fsync of the file, and of the directory when the segment is new.
func (l *Log) append(r Record) error {
	if l.lock == nil {
		return errors.New("audit: closed")
	}
	if l.failAppend != nil {
		return l.failAppend
	}
	now := l.now().UTC()
	month := now.Format("2006-01")
	newSeg := l.seg == 0 || l.size >= l.segLimit || (l.month != "" && month != l.month)
	r.Seq = l.seq + 1
	r.LocalTime = now.Format(timeLayout)
	r.PrevHash = l.last
	if err := r.seal(); err != nil {
		return err
	}
	b, err := r.line()
	if err != nil {
		return err
	}
	if newSeg {
		if l.f != nil {
			_ = l.f.Close()
			l.f = nil
		}
		next := l.seg + 1
		f, err := os.OpenFile(filepath.Join(l.dir, SegmentName(next)), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, FileMode)
		if err != nil {
			return fmt.Errorf("audit: new segment: %w", err)
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		if err != nil {
			f.Close()
			_ = os.Remove(filepath.Join(l.dir, SegmentName(next)))
			return fmt.Errorf("audit: write: %w", err)
		}
		if err := syncDir(l.dir); err != nil {
			f.Close()
			return fmt.Errorf("audit: fsync of the directory: %w", err)
		}
		l.f, l.seg, l.size, l.month = f, next, int64(len(b)), month
	} else {
		if l.f == nil {
			f, err := os.OpenFile(filepath.Join(l.dir, SegmentName(l.seg)), os.O_WRONLY|os.O_APPEND, FileMode)
			if err != nil {
				return fmt.Errorf("audit: %w", err)
			}
			l.f = f
		}
		if _, err := l.f.Write(b); err != nil {
			return fmt.Errorf("audit: write: %w", err)
		}
		if err := l.f.Sync(); err != nil {
			return fmt.Errorf("audit: fsync: %w", err)
		}
		l.size += int64(len(b))
	}
	l.seq, l.last = r.Seq, r.Hash
	l.records++
	switch {
	case resetKinds[r.Kind]:
		l.highE, l.highI = r.Epoch, r.Index
		l.skipNode, l.skipOp = "", ""
		if r.Kind == EventJoinMovedAside {
			l.skipNode, l.skipOp = r.Details["node_id"], r.Details["join_op"]
		}
	case r.Index != 0 || r.Epoch != 0:
		if after(r.Epoch, r.Index, l.highE, l.highI) {
			l.highE, l.highI = r.Epoch, r.Index
		}
	}
	return nil
}

// resetKinds are the records that change the node's cluster: the key
// (epoch, index) starts again at theirs.
var resetKinds = map[string]bool{
	KindStarted:         true,
	EventInit:           true,
	EventWipeFinished:   true,
	EventJoinAbandoned:  true,
	EventJoinMovedAside: true,
}

// Mark writes a reset mark: kind is one of audit.started, node.init,
// node.wipe.finished, node.join.abandoned (with the key to go on from, the
// last index of the node's log) or node.join.started (key zero; details
// must name the node, "node_id", and this join's member.join operation
// ID, "join_op", which ends the skip).
func (l *Log) Mark(kind string, epoch, index uint64, details map[string]string) error {
	if !resetKinds[kind] {
		return fmt.Errorf("audit: %s is not a reset mark", kind)
	}
	if kind == EventJoinMovedAside && (details["node_id"] == "" || details["join_op"] == "") {
		return errors.New("audit: the join mark needs the node ID and the join's operation ID")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(Record{Kind: kind, Epoch: epoch, Index: index, Details: details})
}

// Event writes a local event (no epoch, no index).
func (l *Log) Event(kind string, details map[string]string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(Record{Kind: kind, Details: details})
}

// Started writes audit.started with the (epoch, index) up to which log
// entries are not audited, if the audit is empty; otherwise it does
// nothing.
func (l *Log) Started(epoch, index uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.records != 0 {
		return nil
	}
	return l.append(Record{Kind: KindStarted, Epoch: epoch, Index: index})
}

// AuditEntry implements clusterstate.AuditWriter: the record of a log
// entry, unless its (epoch, index) is at or below the highest since the
// last reset mark (a replay after a restart), or a join's skip is on and
// this is not the node's own applied member.join.
func (l *Log) AuditEntry(a clusterstate.AuditEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !after(a.Epoch, a.Index, l.highE, l.highI) {
		return nil
	}
	if l.skipNode != "" {
		if a.Type != clusterstate.TypeMemberJoin || a.NodeID != l.skipNode || a.Outcome != OutcomeApplied ||
			(l.skipOp != "" && a.OpID != l.skipOp) {
			return nil
		}
		l.skipNode, l.skipOp = "", ""
	}
	return l.append(Record{Kind: KindEntry, Epoch: a.Epoch, Index: a.Index, EntryType: a.Type, EntryVersion: a.Version,
		EntryTime: a.At, Actor: a.Actor, OpID: a.OpID, Origin: a.Origin, RequestID: a.RequestID,
		NodeID: a.NodeID, Name: a.Name, Address: a.Address, Pins: a.Pins, Outcome: a.Outcome, Reason: a.Reason})
}

// AuditSnapshot implements clusterstate.AuditWriter: a snapshot installed
// past the audited index is marked by one snapshot.installed record.
// During a join's skip, only a snapshot that lists the joining node is
// marked (and ends the skip); an earlier one is history.
func (l *Log) AuditSnapshot(epoch, index uint64, members []clusterstate.Member) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !after(epoch, index, l.highE, l.highI) {
		return nil
	}
	if l.skipNode != "" {
		listed := false
		for _, m := range members {
			listed = listed || (m.ID == l.skipNode && (l.skipOp == "" || m.JoinOp == l.skipOp))
		}
		if !listed {
			return nil
		}
		l.skipNode, l.skipOp = "", ""
	}
	return l.append(Record{Kind: KindSnapshot, Epoch: epoch, Index: index})
}

// WithLog opens the audit of dataDir, runs fn and closes it: for the
// stages that write with the service stopped.
func WithLog(dataDir string, fn func(*Log) error) error {
	l, err := Open(dataDir, nil)
	if err != nil {
		return err
	}
	ferr := fn(l)
	cerr := l.Close()
	if ferr != nil {
		return ferr
	}
	return cerr
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// Break is where verification found the audit broken.
type Break struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// Report is the result of Verify.
type Report struct {
	Records int `json:"records"`
	// Segments are the segment files, oldest first; every one but the
	// last is sealed (its successor exists).
	Segments []string `json:"segments"`
	First    *Record  `json:"first,omitempty"`
	Last     *Record  `json:"last,omitempty"`
	// HighEpoch and HighIndex are the highest (epoch, index) since the
	// last reset mark; SkipNode is set when a join's skip is on.
	HighEpoch uint64 `json:"high_epoch"`
	HighIndex uint64 `json:"high_index"`
	SkipNode  string `json:"skip_node,omitempty"`
	SkipOp    string `json:"skip_op,omitempty"`
	Break     *Break `json:"break,omitempty"`

	segNums   []int
	lastMonth string
}

// Sealed returns the sealed segments: all but the last.
func (r Report) Sealed() []string {
	if len(r.Segments) < 2 {
		return nil
	}
	return r.Segments[:len(r.Segments)-1]
}

// Verify checks the audit in dir: every record's hash, every link between
// records and between segments, sequence numbers rising by one, (epoch,
// index) never going down, segment numbers without gaps. It opens every
// file read-only and creates nothing. A missing directory is an empty
// audit. The error is for failing to read at all; what is wrong is in
// Report.Break.
func Verify(dir string) (Report, error) {
	var rep Report
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	nums, err := segments(dir)
	if err != nil {
		return rep, fmt.Errorf("audit: %w", err)
	}
	var prevSeq uint64
	prevHash := ""
	var hiE, hiI uint64
	for i, n := range nums {
		name := SegmentName(n)
		rep.Segments = append(rep.Segments, name)
		rep.segNums = append(rep.segNums, n)
		if i > 0 && n != nums[i-1]+1 {
			rep.Break = &Break{File: name, Line: 0, Reason: fmt.Sprintf("segment %s is missing", SegmentName(nums[i-1]+1))}
			return rep, nil
		}
		if i == 0 && n != 1 {
			rep.Break = &Break{File: name, Line: 0, Reason: "the first segment is not " + SegmentName(1)}
			return rep, nil
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return rep, fmt.Errorf("audit: %w", err)
		}
		br := bufio.NewReaderSize(f, 64<<10)
		lineNo := 0
		for {
			raw, rerr := readLine(br)
			if rerr == io.EOF {
				break
			}
			lineNo++
			fail := func(reason string) (Report, error) {
				f.Close()
				rep.Break = &Break{File: name, Line: lineNo, Reason: reason}
				return rep, nil
			}
			if rerr != nil {
				return fail(rerr.Error())
			}
			var r Record
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r); err != nil {
				return fail("not a record: " + err.Error())
			}
			want, err := r.line()
			if err != nil {
				return fail(err.Error())
			}
			if !bytes.Equal(append(append([]byte(nil), raw...), '\n'), want) {
				return fail("the line is not the record's canonical encoding (changed)")
			}
			check := r
			if err := check.seal(); err != nil {
				return fail(err.Error())
			}
			if check.Hash != r.Hash {
				return fail("the hash does not match the record (changed)")
			}
			if r.PrevHash != prevHash {
				return fail("prev_hash does not match the hash of the record before")
			}
			if r.Seq != prevSeq+1 {
				return fail(fmt.Sprintf("seq %d follows %d", r.Seq, prevSeq))
			}
			switch {
			case resetKinds[r.Kind]:
				hiE, hiI = r.Epoch, r.Index
				rep.SkipNode, rep.SkipOp = "", ""
				if r.Kind == EventJoinMovedAside {
					rep.SkipNode, rep.SkipOp = r.Details["node_id"], r.Details["join_op"]
				}
			case r.Epoch != 0 || r.Index != 0:
				rep.SkipNode, rep.SkipOp = "", ""
				if after(hiE, hiI, r.Epoch, r.Index) {
					return fail(fmt.Sprintf("(epoch %d, index %d) goes below (%d, %d)", r.Epoch, r.Index, hiE, hiI))
				}
				if r.Kind == KindEntry && hiE == r.Epoch && hiI == r.Index && rep.Records > 0 {
					return fail(fmt.Sprintf("log entry (epoch %d, index %d) is recorded twice", r.Epoch, r.Index))
				}
				hiE, hiI = r.Epoch, r.Index
			}
			prevSeq, prevHash = r.Seq, r.Hash
			rep.Records++
			rc := r
			if rep.First == nil {
				rep.First = &rc
			}
			rep.Last = &rc
			if t, err := time.Parse(timeLayout, r.LocalTime); err == nil {
				rep.lastMonth = t.UTC().Format("2006-01")
			}
		}
		f.Close()
		if lineNo == 0 {
			rep.Break = &Break{File: name, Line: 0, Reason: "the segment is empty"}
			return rep, nil
		}
	}
	rep.HighEpoch, rep.HighIndex = hiE, hiI
	return rep, nil
}

// readLine reads one line without its newline; a last line without a
// newline is a break (a cut-off write).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("a line is longer than %d bytes", maxLine)
		}
		switch {
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(buf) == 0:
			return nil, io.EOF
		case err == io.EOF:
			return nil, errors.New("the last line has no newline (a cut-off write)")
		default:
			return nil, err
		}
	}
}

// ReadAll returns every record of the audit in dir, oldest first, without
// checking it (Verify checks). It opens the files read-only.
func ReadAll(dir string) ([]Record, error) {
	nums, err := segments(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, n := range nums {
		f, err := os.Open(filepath.Join(dir, SegmentName(n)))
		if err != nil {
			return nil, err
		}
		br := bufio.NewReaderSize(f, 64<<10)
		for {
			raw, err := readLine(br)
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return nil, err
			}
			var r Record
			if err := json.Unmarshal(raw, &r); err != nil {
				f.Close()
				return nil, err
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out, nil
}
