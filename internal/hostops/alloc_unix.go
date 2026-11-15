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
