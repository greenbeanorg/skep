//go:build unix

package hostops

import (
	"io/fs"
	"syscall"
)

// allocatedBytes is the space a file's blocks take, which is less than its
// size for a sparse file.
func allocatedBytes(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}

// identity is who owns a file and which file it is.
type identity struct {
	Dev, Ino uint64
	UID      uint32
}

// fileIdentity reads a file's device, inode, and owner.
func fileIdentity(fi fs.FileInfo) (identity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, false
	}
	return identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino), UID: st.Uid}, true
}
