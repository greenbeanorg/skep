# Licenses

Skep is released under the **BSD 2-Clause License** ([LICENSE](../LICENSE)),
SPDX identifier `BSD-2-Clause`.

This page lists every third-party component built into Skep's programs,
with its license, and states which are only used to build or test it. It
was checked on 2026-11-22 against `go.mod` and `web/package-lock.json` at
the commit that added it. Update it whenever a dependency is added,
removed, or upgraded across a license change.

## Conclusion

Every component that ends up in a Skep binary is under a permissive
license (BSD-3-Clause, MIT, ISC, Apache-2.0, or public domain), all
compatible with distributing Skep under BSD-2-Clause. No copyleft
(GPL, LGPL, AGPL, MPL) code is linked or bundled. The MPL-2.0, Python-2.0,
and CC-BY-4.0 packages in the list below are build or test tools that
are not distributed.

**What a binary distribution owes.** BSD, MIT, ISC, and Apache-2.0 all
require their copyright and license notices to be reproduced with a
binary distribution. The release package (Milestone 10,
[ADR 0012](decisions/0012-installer.md)) therefore needs a
third-party notices file holding the license texts of the components in
the two "built in" tables below. That file is not generated yet; it is
a release task. `class-variance-authority` (Apache-2.0) ships no
`NOTICE` file, so Apache-2.0 section 4(d) adds nothing beyond its
license text.

## Go modules linked into the programs

From `GOOS=freebsd GOARCH=amd64 go list -deps ./cmd/...`: the modules
whose code is compiled into `skep-agent`, `skep-api`, or `skepctl`.

| Module | Version | License | Why |
|---|---|---|---|
| `github.com/coder/websocket` | v1.8.13 | ISC | Serial console WebSocket |
| `golang.org/x/crypto` | v0.59.0 | BSD-3-Clause | Argon2id |
| `golang.org/x/sys` | v0.50.0 | BSD-3-Clause | FreeBSD system calls |
| `modernc.org/sqlite` | v1.62.0 | BSD-3-Clause; SQLite itself public domain | Pure-Go SQLite (ADR 0001) |
| `modernc.org/libc` | v1.80.1 | BSD-3-Clause; includes code from musl libc (MIT), Go (BSD-3-Clause), go-netdb (MIT), nixpkgs (MIT) | Used by the SQLite driver |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | Used by the SQLite driver |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause (plus Go's BSD-3-Clause for derived code) | Used by the SQLite driver |
| `github.com/dustin/go-humanize` | v1.1.0 | MIT | Used by the SQLite driver |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | Used by the SQLite driver |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT | Used by the SQLite driver |
| `github.com/ncruces/go-strftime` | v1.1.0 | MIT | Used by the SQLite driver |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748 | BSD-3-Clause | Used by the SQLite driver |

The Go standard library and runtime (BSD-3-Clause) are in every binary
too. `modernc.org/sqlite` and `modernc.org/libc` carry their own
`LICENSE-3RD-PARTY.md` with the full texts of what they include; those
texts belong in the notices file.

`go list -m all` shows more modules (for example
`github.com/hashicorp/golang-lru/v2`, MPL-2.0, and `github.com/google/pprof`,
Apache-2.0). They are in the module graph only through the dependencies'
own tests and tools and are not compiled into any Skep program.

## npm packages bundled into the web interface

`skep-api` embeds the built web interface, so the production dependencies
in `web/package-lock.json` (those not marked `dev`) are distributed inside
it.

| Package | Version | License |
|---|---|---|
| `react`, `react-dom`, `scheduler` | 19.3.1, 19.3.1, 0.28.0 | MIT |
| `react-router`, `cookie`, `set-cookie-parser` | 7.18.5, 1.1.1, 2.7.3 | MIT |
| `@xterm/xterm`, `@xterm/addon-fit`, `@xterm/addon-webgl` | 5.5.0, 0.10.0, 0.18.0 | MIT |
| `@radix-ui/react-slot`, `@radix-ui/react-compose-refs` | 1.5.0, 1.1.5 | MIT |
| `class-variance-authority` | 0.7.1 | Apache-2.0 |
| `clsx` | 2.1.1 | MIT |
| `tailwind-merge` | 3.7.0 | MIT |
| `@types/react`, `csstype` | 19.3.0, 3.2.3 | MIT (type definitions only; nothing reaches the bundle) |

Also in the bundle, though not as npm runtime packages:

- **Tailwind CSS**'s generated stylesheet, including its base styles
  (MIT). Tailwind itself is a build tool.
- **shadcn/ui**: the components in `web/src/components/ui` follow
  shadcn/ui's patterns (the repository has its `components.json`).
  shadcn/ui is MIT-licensed; its notice belongs in the notices file.

## Build and test tools (not distributed)

Everything else in `web/package-lock.json` is a development dependency:
about 250 packages, used by Vite, TypeScript, Tailwind's compiler,
Vitest, Playwright, and openapi-typescript. By license: MIT (209), MPL-2.0
(12: `lightningcss` and its platform binaries, Tailwind's CSS compiler),
Apache-2.0 (10: TypeScript, Playwright, and others), ISC (9),
BSD-2-Clause (2), BSD-3-Clause (2), 0BSD (1), Python-2.0 (1: `argparse`),
CC-BY-4.0 (1: `caniuse-lite`, browser data), BlueOak-1.0.0 (1),
MIT OR CC0-1.0 (1). Using them to build places no obligation on Skep's
license or binaries.

Go tools used in development (`staticcheck`, `go vet`) are not
dependencies of the module and are not distributed.

## Source files

Skep's own source files carry no per-file license header or SPDX line;
the repository's `LICENSE` covers them. Adding
`SPDX-License-Identifier: BSD-2-Clause` lines to every file is possible
later if packaging or a downstream asks for it.

## How to re-check

```sh
# Go: modules compiled into the FreeBSD programs
GOOS=freebsd GOARCH=amd64 go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./cmd/... | sort -u

# npm: the license of every package, production and development
cd web && npm ls --all --omit=dev   # the production tree
```

Then read each new module's or package's license file.
