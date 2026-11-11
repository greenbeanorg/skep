# ADR 0001: Go toolchain version and SQLite driver

- Status: Accepted
- Date: 2026-11-11

## Context

Skep's backend is written in Go and its production target is FreeBSD, while
development and CI run on Linux. Every commit is cross-compiled with
`GOOS=freebsd GOARCH=amd64`. Local node state is stored in SQLite. Two
choices follow from that: which Go version the module pins, and which SQLite
driver it uses.

## Decision 1: Go version

`go.mod` declares:

```
go 1.26.0
toolchain go1.27.4
```

- `toolchain go1.27.4` is the pin. It is the newest stable release at the
  time of writing, and Go's toolchain management makes any older `go`
  command download and use exactly that version, so developers and CI build
  with the same compiler.
- `go 1.26.0` is the minimum language version. Go supports the two newest
  release lines, 1.27 and 1.26. Declaring the older one keeps the module
  buildable with a packaged Go on FreeBSD that lags one release behind, when
  someone builds natively with `GOTOOLCHAIN=local`.

The toolchain line is raised when a new patch release ships. The minimum is
raised when a release line leaves upstream support.

## Decision 2: SQLite driver

Skep uses **`modernc.org/sqlite`**, a pure-Go translation of SQLite, through
`database/sql`.

### Alternatives considered

| Driver | Mechanism | Why not |
|---|---|---|
| `github.com/mattn/go-sqlite3` | cgo binding to the C library | Needs a C cross-toolchain and a FreeBSD sysroot to cross-compile from Linux. That breaks the simple `GOOS=freebsd go build` that CI depends on. |
| `github.com/ncruces/go-sqlite3` | SQLite compiled to WebAssembly, run with wazero | Also cgo-free and a reasonable choice. Not chosen because it adds a WebAssembly runtime to a privileged host agent, and because `modernc.org/sqlite` has a longer record on FreeBSD. |

### Consequences

- `CGO_ENABLED=0` everywhere. Binaries are statically linked, and the
  FreeBSD build needs no C compiler. The Makefile enforces this.
- `modernc.org/sqlite` is slower than the C library for write-heavy loads.
  Local node state is small and low-volume, so this is acceptable.
- The driver brings indirect dependencies (`modernc.org/libc` and others).
  They are pure Go.
- The driver choice is contained in `internal/database`, which registers the
  driver and builds the connection string. Nothing else imports it, so the
  driver can be replaced without touching other packages.
- Because cgo is off, `go test -race` cannot run on a machine without a C
  compiler. CI, which has one, runs the tests with the race detector.

## Validation

The FreeBSD amd64 binary was cross-compiled on Linux and run on FreeBSD
15.1-RELEASE-p6. It created the database, applied migration 0001, and served
requests.
