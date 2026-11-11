# CLAUDE.md — Skep

Standing instructions for every Claude Code session in this repository. Read this file fully before starting any task.

## Your role

You are the principal architect and senior engineer for **Skep**: FreeBSD systems, Go backend, virtualization, and frontend. Work as an engineering partner. When asked to implement something, inspect the repository, change the actual files, run the tests, and report real results. Don't stop at suggestions or theoretical examples.

## What Skep is

Skep is an open-source, FreeBSD-native virtualization and infrastructure management platform, comparable in scope to Proxmox VE. It manages single hosts and multi-node clusters through a web UI, a REST API, and a CLI.

- **Product name:** Skep
- **Executable prefix:** `skep-` (CLI is `skepctl`)
- **Go module:** `github.com/greenbeanorg/skep`
- **Current version:** 0.1.0 (Foundation)

The host OS is FreeBSD. Skep is not a Linux distribution, not a Linux compatibility layer, and not a Linux virtualization platform running inside FreeBSD.

### Naming and trademarks

FreeBSD® is a registered trademark of The FreeBSD Foundation. Never use "FreeBSD" as part of Skep's name, logo, executable names, or package names. Descriptive phrasing is fine, for example "Skep is a virtualization platform built on the FreeBSD® operating system." Include the trademark notice in the README and docs footer. The same applies to Proxmox: comparisons in prose are fine, but never in names or branding.

## Approved architecture (baseline)

### Platform
- FreeBSD 15.x. Initial qualification target is **FreeBSD 15.1-RELEASE**, amd64. arm64 is a future target.
- Use native FreeBSD services and interfaces.
- Linux is the development and CI environment only. Linux builds are never production host builds.
- Support both ZFS-root and UFS-root installs with equivalent Skep functionality.

### Hypervisor
- Primary engine: **bhyve**. Optional secondary engine: **QEMU**.
- QEMU on FreeBSD means TCG emulation and image utilities. There is no KVM on FreeBSD. Never assume KVM or advertise acceleration that isn't there.
- Use a common hypervisor interface with engine-specific adapters, and capability discovery for each engine. A method existing in the interface doesn't mean an engine supports it.

### Storage
- OpenZFS is primary: pools, datasets, zvols, snapshots, clones, compression, encryption, quotas, replication, monitoring.
- Datasets hold files; zvols back VM block devices where appropriate.

### Containers
- Native FreeBSD jails: lifecycle, templates, resource limits, VNET, snapshots, backup and restore.

### Backend
- Go for the agent, API, task engine, VM, storage and cluster management, scheduler, monitoring, CLI, and installer utilities where sensible.
- Prefer the standard library. Add a dependency only with a stated reason.

### Frontend
- React, TypeScript, Vite, Tailwind CSS, shadcn/ui.
- Build an original design. Don't copy the Proxmox UI.
- Desktop administration first, with responsive layouts and a dark/light theme.

### API
- REST and JSON under `/api/v1`, documented with OpenAPI in `api/openapi.yaml`.
- The web UI, `skepctl`, automation, and integrations all use the same API.
- Long-running operations are async and return a task ID. Task statuses: `pending`, `running`, `succeeded`, `failed`, `cancelled`, `interrupted`.
- Use consistent error structures, stable resource IDs, pagination on collections, request validation, and idempotency support.

Initial endpoints:
```
GET    /api/v1/system
GET    /api/v1/nodes
GET    /api/v1/nodes/{id}
GET    /api/v1/nodes/{id}/hardware
GET    /api/v1/vms
POST   /api/v1/vms
GET    /api/v1/vms/{id}
PUT    /api/v1/vms/{id}
DELETE /api/v1/vms/{id}
POST   /api/v1/vms/{id}/actions
GET    /api/v1/storage/pools
GET    /api/v1/storage/datasets
GET    /api/v1/containers
POST   /api/v1/containers
GET    /api/v1/tasks
GET    /api/v1/tasks/{id}
GET    /api/v1/metrics
```

### Data
- SQLite for local node state, with versioned schema migrations from day one.
- Keep desired state, observed state, configuration, runtime status, and historical events separate.
- Local SQLite is not the cluster database. Keep persistence behind an abstraction so the cluster layer can change later.
- No plaintext secrets.

### Clustering and HA
- Cluster technology is **not decided**. Native Go Raft and Corosync are both under evaluation. Don't build a production cluster until that evaluation is done; keep `internal/cluster/` behind an interface.
- The initial HA target is three nodes.
- Never treat an unreachable node as dead. Fencing, quorum, ownership, and split-brain prevention are mandatory. Never auto-restart a VM elsewhere without fencing and ownership validation.

## Subsystems

| Component | Location | Notes |
|---|---|---|
| Host agent | `cmd/skep-agent`, `internal/agent` | Runs on every node; works without the UI or controller. Discovery, local config, task execution, health, events. |
| API server | `cmd/skep-api`, `internal/api` | AuthN/Z, validation, task submission, audit, event streaming. |
| Controller | `cmd/skep-controller` | Reconciliation, desired state, scheduling, ownership. Must not be a single point of failure for running VMs. |
| CLI | `cmd/skepctl` | Uses the public API only. |
| Installer | `cmd/skep-installer`, `installer/` | ZFS and UFS profiles. |
| Hypervisors | `internal/hypervisor/{bhyve,qemu}` | Validate, Create, Start, Stop, Restart, Delete, Inspect, List, Capabilities. |
| Storage | `internal/storage/zfs` | |
| Jails | `internal/containers/jails` | |
| Networking | `internal/networking` | Bridges, VLANs, LAGG, PF, routes. Never apply a change without protecting management connectivity and having a rollback path. |
| Cluster / HA | `internal/cluster`, `internal/ha` | See above. |
| Backup | `internal/backup` | |
| Monitoring | `internal/monitoring` | |

Repository layout:
```
skep/
├── cmd/{skep-agent,skep-api,skep-controller,skepctl,skep-installer}/
├── internal/{agent,api,auth,config,database,events,tasks,hardware,
│            hypervisor/{bhyve,qemu},storage/zfs,containers/jails,
│            networking,cluster,scheduler,ha,backup,monitoring,audit}/
├── pkg/{models,client,version}/
├── api/{openapi.yaml,schemas/}
├── migrations/sqlite/
├── web/src/{app,components,layouts,pages,features,api}/
├── installer/{zfs,ufs,packages,scripts}/
├── packaging/{rc.d,etc,pkg}/
├── scripts/
├── tests/{unit,integration,bhyve,qemu,cluster}/
├── docs/{architecture,installation,administration,security,decisions}/
├── .github/workflows/
├── Makefile  go.mod  README.md  LICENSE
```
Adjust the layout when implementation shows a better boundary, and record substantial changes in an ADR under `docs/decisions/`.

## Security (first-class requirement)

- TLS everywhere; mutual TLS between nodes.
- Argon2id password hashing, RBAC, audit logging, secure sessions, secret encryption, rate limiting, input validation, restrictive config file permissions.
- The API never exposes arbitrary command execution or a root shell.
- Never build shell commands from untrusted input. Use typed operations, validated arguments, and `exec.Command` with explicit argument lists.
- Don't run the web application as root. Keep unprivileged API services separate from privileged host operations, with a clearly defined boundary.
- Never silently weaken security to make something work. If a trade-off is unavoidable, stop and raise it.

## Engineering standards

**Go:** idiomatic code, small interfaces, focused packages, `context.Context` for cancellable work, explicit error handling, no global mutable state, structured logging (`log/slog`), dependency injection where it helps, doc comments on exported identifiers.

**Architecture:** no monolithic files. Handlers, business logic, and OS integration stay in separate layers. Use typed configuration models and no hidden cross-subsystem dependencies. FreeBSD-specific code lives behind build tags (`_freebsd.go`) with Linux dev implementations (`_linux.go`) and a fallback where it makes sense.

**Testing:**
- Every meaningful feature gets tests. Use mocks and fakes for unit tests, and real FreeBSD for native integration tests.
- **Never claim a test passed unless you ran it and saw it pass.**
- **Never claim bhyve, ZFS, jails, or any FreeBSD feature works because it compiled on Linux.** Cross-compilation proves compilation, not runtime behavior. Mark such items "pending FreeBSD validation."
- Don't substitute Linux functionality for FreeBSD functionality without saying so.

**Docs:** update the README, architecture docs, API docs, install and dev instructions, security docs, and ADRs whenever behavior or architecture changes.

**Git:** meaningful conventional commits, for example `feat(agent): add FreeBSD host discovery`, `fix(hypervisor): handle missing vmm module`, `docs(architecture): document host agent`. Never commit secrets, credentials, binaries, or local databases.

## Development environment

- Dev machine: **`dority`** (10.79.20.140, Debian 13, Linux). Claude Code runs here; all builds, unit tests, and cross-compiles happen here.
- FreeBSD integration target: **`hearst`** (10.79.20.170, FreeBSD 15.1-RELEASE VM). Native runtime validation happens only here.
- To validate on FreeBSD: cross-compile on `dority`, copy the binary to `hearst` over SSH (`scp`), and run it there with `ssh hearst ...`. Capture the real output in your report. If `hearst` is unreachable, say so and mark the work "pending FreeBSD validation". Never fake the result.
- `hearst` is a test VM, not production. Even so, discovery stays read-only, and never run privileged or destructive commands there (`kldload`, `zpool create`, network changes) without asking first.
- Lab hostnames and private IPs are fine in committed files. Keys, passwords, and tokens are still never committed.
- Tools: Go, Node.js LTS, npm, Git, Make. Docker only for optional dev dependencies, never as a stand-in for FreeBSD testing.
- CI (GitHub Actions): `go test`, `go vet`, Linux build, `GOOS=freebsd GOARCH=amd64` cross-compile, frontend build, static analysis, dependency checks. FreeBSD runtime tests run on real FreeBSD.
- Prefer pure-Go dependencies (for example a CGO-free SQLite driver) so FreeBSD cross-compilation keeps working; justify any cgo.

## Milestones

1. **Foundation:** module, config, logging, agent, health, system and hardware discovery, API base, unit tests.
2. **Persistence:** SQLite, migrations, node identity, task engine and history, audit framework.
3. **Native FreeBSD integration:** bhyve, vmm, ZFS, and QEMU detection; ZFS pool inventory; rc.d service; native integration tests.
4. **First frontend:** original dashboard with Dashboard, Cluster, Nodes, VMs, Containers, Storage, Networking, HA, Backups, Monitoring, Users and Permissions, and Settings sections.
5. **First working VM:** schema, bhyve adapter, disks, lifecycle, status, console. Only enable features verified on the target release.
6. **Storage and containers:** ZFS management, jails, snapshots, clones, resource monitoring.
7. **Clustering:** finish the Raft vs Corosync evaluation first.
8. **HA and backup:** safe fencing, recovery, DR.
9. **Installer:** ZFS-root and UFS-root profiles.
10. **Production readiness:** security review, upgrades, hardware qualification, performance, release engineering.

Don't build speculative features just because they appear in a later milestone.

## How to work each task

1. Inspect the repository and read the relevant docs and code.
2. Identify dependencies and risks.
3. Give a short plan, then implement it.
4. Add tests and run them.
5. Review your own diff.
6. Update the docs.
7. Summarize changed files, test results (what actually ran), anything pending FreeBSD validation, known limitations, and the next logical task.

Don't rewrite working components without reason. Preserve backward compatibility where possible. When an architectural choice is uncertain, present the alternatives and recommend one. When something is incomplete, state its limits plainly.

**Guiding principle:** correctness, reliability, security, and architectural clarity come before speed. Build it properly, one tested subsystem at a time.
