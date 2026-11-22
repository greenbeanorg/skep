// Command pkgstage stages the Skep FreeBSD package (ADR 0012 Decision 1):
// the file tree under the prefix, +MANIFEST with the package scripts, and
// a plist with every file's owner and mode. It does not build the .pkg:
// that is pkg-create(8)'s job, run by hand on a FreeBSD lab VM for each
// release (ADR 0012 answer 2; docs/installation/packaging.md).
//
// Usage (make stage runs it):
//
//	go run ./scripts/pkgstage -bin bin/freebsd-amd64 -out dist/pkg -version 0.1.0
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	var o Options
	flag.StringVar(&o.Repo, "repo", ".", "repository root")
	flag.StringVar(&o.BinDir, "bin", "bin/freebsd-amd64", "directory with the FreeBSD amd64 binaries")
	flag.StringVar(&o.Out, "out", "dist/pkg", "output directory (replaced only if pkgstage made it)")
	flag.StringVar(&o.Version, "version", "", "package version, such as 0.1.0")
	flag.BoolVar(&o.CheckELF, "check-elf", true, "require FreeBSD amd64 ELF binaries")
	flag.Parse()
	r, err := Stage(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pkgstage:", err)
		os.Exit(1)
	}
	fmt.Printf("pkgstage: staged %s\n", r.Stage)
	fmt.Printf("pkgstage: manifest %s, plist %s, checksums %s\n", r.Manifest, r.Plist, r.Sums)
	fmt.Printf("pkgstage: on a FreeBSD 15 amd64 lab VM: pkg create -M %s -p %s -r %s -o <dir>\n", r.Manifest, r.Plist, r.Stage)
}
