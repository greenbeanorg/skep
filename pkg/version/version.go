// Package version exposes Skep build metadata.
//
// The variables are set at build time with -ldflags, for example:
//
//	go build -ldflags "-X github.com/greenbeanorg/skep/pkg/version.Version=0.1.0"
//
// The Makefile does this for every binary. A plain "go build" leaves the
// development defaults in place.
package version

import "runtime"

// Build metadata injected with -ldflags -X. They are variables only because
// the linker needs them to be; nothing changes them at run time.
var (
	// Version is the Skep release version.
	Version = "0.1.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the UTC build timestamp in RFC 3339 format.
	BuildDate = "unknown"
)

// Info describes a Skep build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the build metadata of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String returns a one-line, human-readable description of the build.
func (i Info) String() string {
	return i.Version + " (commit " + i.Commit + ", built " + i.BuildDate + ", " +
		i.GoVersion + " " + i.OS + "/" + i.Arch + ")"
}
