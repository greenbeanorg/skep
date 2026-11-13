// Package auditarchive seals the entries of an audit log into archive
// segments, verifies them, and removes sealed entries from the live table
// (docs/decisions/0007-retention.md).
//
// The record is the live audit_log table plus the sealed segments. A
// segment is a contiguous range of entry IDs, written to a gzip-compressed
// JSON Lines file and recorded in the audit_segments table; inserting that
// row is the seal. The segments form a hash chain. An entry is removed
// from the live table only when it is inside a sealed segment whose file
// has just been read back and still matches its recorded hash; the
// database enforces the first half of that rule and this package the
// second.
//
// The same code serves both logs: skep-agent runs it on agent.db and
// skep-api on api.db. Neither process opens the other's database or
// archive (ADR 0004).
package auditarchive

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

// FormatVersion is the version of the segment file format.
const FormatVersion = 1

// timeLayout is how both databases store timestamps (see internal/database).
const timeLayout = "2006-01-02T15:04:05.000Z"

// monthLayout is how a segment's month is written.
const monthLayout = "2006-01"

// Log describes one audit log: its name in segment headers and the columns
// of its audit_log table, in the fixed order they are written to a segment.
type Log struct {
	// Name is "agent" for the agent's operation audit log and "api" for
	// skep-api's access audit log.
	Name string
	// Columns always starts with id and occurred_at.
	Columns []string
}

// The two audit logs.
var (
	// AgentLog is the operation audit log in agent.db.
	AgentLog = Log{Name: "agent", Columns: []string{
		"id", "occurred_at", "actor_id", "actor_name", "peer_uid", "action",
		"resource_type", "resource_id", "task_id", "phase", "outcome",
		"task_status", "source_ip", "request_id", "details",
	}}
	// AccessLog is the access audit log in api.db.
	AccessLog = Log{Name: "api", Columns: []string{
		"id", "occurred_at", "actor_id", "actor_name", "action",
		"resource_type", "resource_id", "outcome", "source_ip", "request_id",
		"details",
	}}
)

// jsonColumn is the column that holds a JSON document, which is written to
// a segment as JSON and not as a string.
const jsonColumn = "details"

// Header is the first line of a segment file.
type Header struct {
	Format int    `json:"format"`
	Log    string `json:"log"`
	NodeID string `json:"node_id"`
	// Month is the calendar month (UTC) the segment was sealed for. It is
	// a label: FirstID and LastID alone decide what the segment contains.
	Month   string `json:"month"`
	FirstID int64  `json:"first_id"`
	LastID  int64  `json:"last_id"`
	Entries int64  `json:"entries"`
	// Earliest and Latest are the lowest and highest occurred_at in the
	// range, for information. They may lie outside Month.
	Earliest string `json:"earliest_occurred_at"`
	Latest   string `json:"latest_occurred_at"`
	// PreviousHash is the hash of the segment before this one, or empty
	// for the first.
	PreviousHash string `json:"previous_hash"`
}

var nodeIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// FileName returns the name of the file of a segment.
func FileName(nodeID, month string, firstID, lastID int64) string {
	return fmt.Sprintf("audit-%s-%s-%d-%d.jsonl.gz", nodeID, month, firstID, lastID)
}

// appendEntry appends one entry as a JSON object and a newline: the
// columns in their fixed order, NULL as null.
func appendEntry(buf []byte, columns []string, values []any) ([]byte, error) {
	buf = append(buf, '{')
	for i, col := range columns {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '"')
		buf = append(buf, col...)
		buf = append(buf, '"', ':')
		v := values[i]
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		switch v := v.(type) {
		case nil:
			buf = append(buf, "null"...)
		case int64:
			buf = strconv.AppendInt(buf, v, 10)
		case string:
			if col == jsonColumn {
				// The schema guarantees valid JSON. Compacting removes any
				// newline, which would end the line early.
				var compact bytes.Buffer
				if err := json.Compact(&compact, []byte(v)); err != nil {
					return nil, fmt.Errorf("column %s is not valid JSON: %w", col, err)
				}
				buf = append(buf, compact.Bytes()...)
				continue
			}
			quoted, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", col, err)
			}
			buf = append(buf, quoted...)
		default:
			return nil, fmt.Errorf("column %s has unexpected type %T", col, v)
		}
	}
	return append(buf, '}', '\n'), nil
}

// rowSource yields the entries of a range in ID order. next returns false
// at the end.
type rowSource interface {
	next() (id int64, values []any, ok bool, err error)
}

// writeSegment writes the header and the entries from src to w and returns
// the SHA-256 of what it wrote. It fails if src does not yield exactly the
// entries the header announces.
func writeSegment(w io.Writer, log Log, h Header, src rowSource) (string, error) {
	sum := sha256.New()
	out := bufio.NewWriterSize(io.MultiWriter(w, sum), 256<<10)

	head, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode header: %w", err)
	}
	if _, err := out.Write(append(head, '\n')); err != nil {
		return "", err
	}

	var (
		buf   []byte
		count int64
		last  = h.FirstID - 1
	)
	for {
		id, values, ok, err := src.next()
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if id <= last || id > h.LastID {
			return "", fmt.Errorf("entry %d is out of order or outside %d-%d", id, h.FirstID, h.LastID)
		}
		last = id
		count++
		if buf, err = appendEntry(buf[:0], log.Columns, values); err != nil {
			return "", fmt.Errorf("entry %d: %w", id, err)
		}
		if _, err := out.Write(buf); err != nil {
			return "", err
		}
	}
	if count != h.Entries || last != h.LastID {
		return "", fmt.Errorf("the log changed while it was read: found %d entries ending at %d, expected %d ending at %d",
			count, last, h.Entries, h.LastID)
	}
	if err := out.Flush(); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// errMissing is returned by readSegment when the file does not exist.
var errMissing = errors.New("the segment file does not exist")

// readSegment reads a segment file, checks its structure, and returns its
// header and the SHA-256 of its uncompressed content. If entry is not nil
// it is called with every entry line, including the newline.
//
// The structure is sound when the header parses, every entry has an ID,
// the IDs are strictly ascending inside the header's range and end at its
// last ID, and the number of entries is the one the header announces.
func readSegment(path string, entry func(line []byte) error) (Header, string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Header{}, "", errMissing
	}
	if err != nil {
		return Header{}, "", err
	}
	if !info.Mode().IsRegular() {
		return Header{}, "", errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Header{}, "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return Header{}, "", fmt.Errorf("not a gzip file: %w", err)
	}
	sum := sha256.New()
	in := bufio.NewReaderSize(io.TeeReader(zr, sum), 256<<10)

	line, err := in.ReadBytes('\n')
	if err != nil {
		return Header{}, "", fmt.Errorf("read header: %w", noEOF(err))
	}
	var h Header
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return Header{}, "", fmt.Errorf("parse header: %w", err)
	}
	if h.Format != FormatVersion {
		return Header{}, "", fmt.Errorf("unknown format version %d", h.Format)
	}

	var count int64
	last := h.FirstID - 1
	for {
		line, err := in.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			return Header{}, "", fmt.Errorf("read entry after %d: %w", last, noEOF(err))
		}
		var e struct {
			ID *int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &e); err != nil || e.ID == nil {
			return Header{}, "", fmt.Errorf("the entry after %d is not a JSON object with an id", last)
		}
		if *e.ID <= last || *e.ID > h.LastID {
			return Header{}, "", fmt.Errorf("entry %d is out of order or outside %d-%d", *e.ID, h.FirstID, h.LastID)
		}
		last = *e.ID
		count++
		if entry != nil {
			if err := entry(line); err != nil {
				return Header{}, "", err
			}
		}
	}
	if count != h.Entries || last != h.LastID {
		return Header{}, "", fmt.Errorf("holds %d entries ending at %d, but its header announces %d ending at %d",
			count, last, h.Entries, h.LastID)
	}
	// The loop read to the end of the file, so gzip has checked its CRC
	// and length, and anything appended to the file was either rejected
	// or hashed with the rest.
	return h, hex.EncodeToString(sum.Sum(nil)), nil
}

// noEOF turns an EOF in the middle of a line into the error it is.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// parseMonth reads YYYY-MM as the first instant of that month, UTC.
func parseMonth(s string) (time.Time, error) {
	t, err := time.Parse(monthLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid month %q", s)
	}
	return t, nil
}

// monthStart returns the first instant of t's month, UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// nextMonth returns the first instant of the month after start.
func nextMonth(start time.Time) time.Time { return start.AddDate(0, 1, 0) }
