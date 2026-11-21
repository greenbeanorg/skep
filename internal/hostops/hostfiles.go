package hostops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// HostFileRoots are the host directories whose files a template build
// must leave unchanged (ADR 0010 decision 7a; the step 6 decision, item 7).
var HostFileRoots = []string{"/etc", "/var/db/pkg", "/usr/share/certs"}

// HostFileExclusions are the exact paths under the HostFileRoots that the
// host-file check leaves out (ADR 0010, step 6 decision, item 7,
// "Exclusions"). Each entry is an absolute path with its reason recorded
// in the ADR; there are no wildcards, and a listed path that is a
// directory is still walked.
//
//   - /var/db/pkg/vuln.xml: periodic's daily pkg audit -F rewrites it.
var HostFileExclusions = []string{"/var/db/pkg/vuln.xml"}

// HostFileState maps the path of every regular file and symbolic link
// under the HostFileRoots to a description of it: type, mode, owner,
// size, modification time, and the SHA-256 of a regular file or the
// target of a link.
type HostFileState map[string]string

// Changed lists, sorted, every path whose description differs between
// two states, or that only one of them has.
func (s HostFileState) Changed(after HostFileState) []string {
	var out []string
	for p, d := range s {
		if after[p] != d {
			out = append(out, p)
		}
	}
	for p := range after {
		if _, ok := s[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// HostFiles implements Templates.
func (h *host) HostFiles(ctx context.Context) (HostFileState, error) {
	roots := h.hostFileRoots
	if roots == nil {
		roots = HostFileRoots
	}
	return recordFiles(ctx, roots, HostFileExclusions)
}

// recordFiles walks each root without following symbolic links. A root
// that does not exist is recorded as missing, so its appearance is a
// change too. A file or link whose path is exactly one of excluded is not
// recorded; directories are always walked.
func recordFiles(ctx context.Context, roots, excluded []string) (HostFileState, error) {
	skip := make(map[string]bool, len(excluded))
	for _, p := range excluded {
		skip[p] = true
	}
	state := HostFileState{}
	for _, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			state[root] = "missing"
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() || skip[path] {
				return nil
			}
			fi, err := os.Lstat(path)
			if err != nil {
				return err
			}
			desc, ok, err := describeFile(path, fi)
			if err != nil {
				return err
			}
			if ok {
				state[path] = desc
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("hostops: record the host files under %s: %w", root, err)
		}
	}
	return state, nil
}

// describeFile describes a regular file or a symbolic link; anything else
// (sockets, FIFOs, devices) is not recorded.
func describeFile(path string, fi fs.FileInfo) (string, bool, error) {
	uid, gid := -1, -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(st.Uid), int(st.Gid)
	}
	head := fmt.Sprintf("mode=%s uid=%d gid=%d size=%d mtime=%d", fi.Mode(), uid, gid, fi.Size(), fi.ModTime().UnixNano())
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", false, err
		}
		return "link " + head + " target=" + target, true, nil
	case fi.Mode().IsRegular():
		sum, err := hashNoFollow(path)
		if err != nil {
			return "", false, err
		}
		return "file " + head + " sha256=" + sum, true, nil
	}
	return "", false, nil
}

// hashNoFollow hashes a regular file opened with O_NOFOLLOW.
func hashNoFollow(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ownerUID returns the owner of a file.
func ownerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
