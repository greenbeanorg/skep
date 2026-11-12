//go:build !unix

package ctlstate

import (
	"errors"
	"io/fs"
)

// checkOwner cannot check ownership here, so it refuses: skepctl's state
// holds a session token and must not be kept where that is unknown.
func (s *Store) checkOwner(path string, info fs.FileInfo) error {
	return errors.New("skepctl keeps its state only on Unix systems")
}
