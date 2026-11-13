//go:build !skep_webui

package webui

import (
	"embed"
	"io/fs"
)

// Without the skep_webui build tag the binary carries only the placeholder
// page: "go build ./..." and "go test ./..." need no Node.js. "make build"
// and "make build-freebsd" build the interface and set the tag.

//go:embed placeholder/index.html
var placeholder embed.FS

var files fs.FS = mustSub(placeholder, "placeholder")

const embedded = false

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
