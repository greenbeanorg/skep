//go:build skep_webui

package webui

import (
	"embed"
	"io/fs"
)

// With the skep_webui build tag the binary carries the interface as built
// by "make build" or "make build-freebsd", which copy web/dist here first.
// The directory is not in the repository; a build with the tag and without
// it fails, as it should.

//go:embed all:dist
var dist embed.FS

var files fs.FS = mustSub(dist, "dist")

const embedded = true

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
