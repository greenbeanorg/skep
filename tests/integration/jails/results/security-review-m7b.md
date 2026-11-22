# Security review: feat/m7b-templates (jails, templates, snapshots)

- Date: 2026-11-18
- Branch head reviewed: `8effd84`
- Method: I read the code only. I changed no code, did not touch hearst, and made no API calls. ADR 0010 was used only for intent; every finding below comes from the code.
- Threat actors: (A) root inside a Skep jail; (B) an API user with less than admin (any of `container:*`, `storage:*`).
- Confidence: **confirmed by reading** means the code path is certain from the source, though the runtime effect may still need hearst. **likely** means the code path is certain and the effect depends on FreeBSD behaviour I have not run. **speculative** means it needs a precondition or kernel behaviour I could not establish.
- Nothing here has been validated on FreeBSD. Every "proof on hearst" item is a proposed test and still needs Andy's approval before it runs.

## Count by severity

| Severity | Count |
|---|---|
| critical | 0 |
| high | 1 |
| medium | 4 |
| low | 6 |
| info | 6 |

---

## High

### SR-01 (high): jail processes keep a write descriptor to the host's console.log, so they can fill the host's /var/run without limit

- **Where:** `internal/hostops/jails_host.go:531-537` (jexec of `/etc/rc` with `Output: logPath`), `internal/hostops/exec_freebsd.go:89-103` (`runToFile` passes the host `*os.File` as the child's stdout and stderr), `internal/hostops/templates_pkg.go:58-62` (the comment says daemons may keep it open), `internal/config/confinement.go:329-332` (`JailRunDir` = `/var/run/skep/jails`, next to the socket).
- **Trigger (A):** jail root adds a background writer to its own `/etc/rc.local` or to any rc.d script, for example `(while :; do cat /dev/zero; done) &`, or simply leaves a daemon that writes to stdout. The tree persists, so this runs on the next start (restart, or a start after a host reboot). `/etc/rc` returns at once, the start succeeds, and the background process keeps fd 1 and fd 2. Both point at `/var/run/skep/jails/<id>/console.log`, a root-owned file on the host opened `O_APPEND`.
- **Gain:** the host's file system fills at disk speed. On hearst (UFS root) `/var/run` is on `/`, so the root file system fills too. That also hits `/var/db/skep` (agent and API SQLite, audit log), syslog, and anything else on `/`. Nothing caps or rotates the file. The jail's RCTL limits don't apply, because the file is a host file, and no `writebps` rule is set. The file also only ever grows: it is opened `O_APPEND` on every start and never truncated, so it grows slowly even for honest jails. The writer also controls the last 64 KiB that `GET /containers/{id}/console-log` shows, so it can forge "Skep" lines there (see SR-I2). With the descriptor, a jailed root can also `fchmod` and `ftruncate` the file, since it owns it as uid 0. It cannot use the descriptor to reach any other host path.
- **Confidence:** the descriptor inheritance is confirmed by reading. The disk-fill effect is likely.
- **Proof on hearst (bounded, needs approval):** in a test jail put `( dd if=/dev/zero bs=1m count=50 2>/dev/null; sleep 600 ) &` in `/etc/rc.local` and start it. After the start task has succeeded, `sudo ls -l /var/run/skep/jails/<id>/console.log` should show about 50 MiB. `sudo procstat -f <pid of the sleeping sh>` should show fd 1 on that host path. `df /` shows the cost. Stop the jail and delete the file afterwards.
- **Direction (not a fix):** give jexec a pipe the agent drains into a size-capped file and closes once `/etc/rc` exits, or start `/etc/rc` with stdout and stderr on `/dev/null` (inside the jail) and capture only the first N bytes. Rotate or truncate on each start.

---

## Medium

### SR-02 (medium): no layer-2 or layer-3 anti-spoofing between jails, the host and the LAN on a bridge

- **Where:** `internal/hostops/jailnet.go:249-276` (an epair host end is added to the bridge with `addm` and nothing else: no `sticky`, no `private`, no PF or `ipfw` on the member, no `net.link.bridge.pfil_member`). `internal/containers/network.go:251-282` (`checkAddressFree` compares configured addresses only). `jailnet.go:40-47` (ruleset 6001 unhides `bpf*` for DHCP jails).
- **Trigger (A):** inside its VNET, jail root runs `ifconfig skep0 ether <victim MAC>` or `ifconfig skep0 inet <victim IP or gateway IP>`, or sends gratuitous ARP. Raw sockets are always available in a VNET jail (amendment f), and DHCP jails also have `/dev/bpf` for raw frame injection.
- **Gain:** the attacker steals or intercepts traffic of other jails, VMs and LAN hosts on the same bridge: ARP-spoofing the default gateway, stealing a static IP, or hijacking a MAC in the bridge's forwarding table. It can also run a rogue DHCP server for every DHCP jail and VM on the bridge. The duplicate-IP refusal is advisory only: it covers the API's configuration, not what the jail does.
- **Confidence:** the absence of a control is confirmed by reading. The effect is likely, as standard if_bridge behaviour. Your notes already list the spoofing tests as deferred, so this records the gap from the code side.
- **Proof on hearst:** the deferred spoofing tests (with Andy at OPNsense). Jail A takes jail B's static IP and MAC, and B's traffic is seen at A with `tcpdump` in A. Then gratuitous ARP for the bridge's gateway.
- **Measured (2026-11-21):** every forged frame passed the host bridge: forged IPv4 sources on- and off-subnet, a changed MAC, and an address claim with gratuitous ARP ([step10-progress.md](step10-progress.md) §31).
- **Decision (2026-11-21, [ADR 0010 amendment y](../../../../docs/decisions/0010-storage-and-jails.md)):** root in a jail is not trusted on the network; jails keep `/dev/bpf`; source enforcement lives on the host per guest port (epair host end, VM tap), not upstream; the API's recorded MAC is authoritative and IP/ARP from any but the configured address is dropped; static NICs get MAC and IP pinning, DHCP NICs MAC pinning only (documented gap), VMs the same rules, IPv6 dropped where none is configured. **Not built in M7b:** its own ADR and work right after the M7b merge, and a **release blocker**. The mechanism (pf `ether` rules vs ipfw on the bridge) waits for a hearst probe. The answers to the seven questions are §32 of the progress log.

### SR-03 (medium): an API user can give a jail NIC any unicast MAC, including the LAN gateway's or the host's

- **Where:** `internal/containers/network.go:157-171` (`normalizeMAC` refuses only multicast and all-zero) and `internal/hostops/jailnet.go:153`. The comment at `jailnet.go:67-68` says "locally administered, unicast", but the locally-administered bit is never checked. `internal/database/containerstore.go:42-48` checks uniqueness only against other Skep jails and VMs.
- **Trigger (B):** a user with `container:write` creates or edits a container with `nics[].mac_address` set to the MAC of the LAN router, the host's bridge or uplink, or another non-Skep machine. Editing the MAC is allowed even after the first start.
- **Gain:** on start, the epair's jail end carries that MAC. The bridge learns it on the epair port, and traffic for the real owner on that bridge goes to the jail. The result is MITM or denial of service of LAN hosts, reached through the API without jail root and without admin.
- **Confidence:** confirmed by reading (validation). The bridge learning effect is likely.
- **Proof on hearst:** create a container on a test bridge with the MAC of a second test interface on that bridge, start it, and check `ifconfig bridgeX addr` for the MAC learnt on the epair port. Do not use a real LAN MAC.
- **Direction:** require the locally-administered bit (`mac[0]&2 != 0`) and refuse MACs present on host interfaces.

### SR-04 (medium): `locked_memory` has no upper bound unless `memory` is also set, so a jail with the mlock opt-in can wire most of the host's RAM

- **Where:** `internal/containers/limits.go:36-39` (mlock requires `LockedMemory != 0` and RACCT), `limits.go:58-59` (`LockedMemory <= Memory` is checked only when `Memory > 0`). No maximum is checked against physical memory or `vm.max_user_wired`.
- **Trigger (B, then A):** a `container:write` user sets `options.mlock=true`, `limits.locked_memory` to, say, 1 TiB, and no `limits.memory`. Jail root then `mlock`s, or `mlockall`s a large allocation.
- **Gain:** wired memory up to the system-wide `vm.max_user_wired`. Wired pages cannot be swapped, so the host, the agent, other jails and bhyve guests lose that memory, and other jails' and VMs' own wiring fails. Amendment j meant to bound this, but the bound is only as tight as the value the user asks for.
- **Confidence:** confirmed by reading (validation). The host effect is likely.
- **Proof on hearst:** unit-level proof needs no hearst. `validLimits` accepts `{LockedMemory: 1<<40}` with `Memory: 0`. On hearst, a bounded run of `tests/integration/jails/mlocktest` with locked_memory at about 2 GiB and no memory limit, watching `sysctl vm.stats.vm.v_wire_count`.
- **Direction:** require `limits.memory` whenever mlock is on, or cap `locked_memory` at a host-derived maximum.

### SR-05 (medium): the RCTL set leaves host-wide resources open even for a fully limited jail; all limits are optional anyway

- **Where:** `internal/hostops/jailrctl.go:35-42` and `72-92`. Only `memoryuse`, `pcpu`, `maxproc`, `openfiles` and `memorylocked` are set. `internal/containers/limits.go:43-63`: every limit may be zero, meaning none.
- **Trigger (A):** jail root uses resources that are not limited:
  - swap, through `memoryuse`: RCTL enforces `memoryuse` by paging the jail out, so a jail can still fill swap (hearst has 4 GiB). Also POSIX `shm_open` objects, and SysV shm when `sysvipc` is set to new. No `swapuse`, `vmemoryuse` or `shmsize` rule is set.
  - threads: no `nthr` rule, so `maxproc` × `kern.threads.max_threads_per_proc` can reach the host-wide thread limit.
  - pseudo-terminals (`pseudoterminals`), and disk I/O (`readbps`, `writebps`, `readiops`, `writeiops`) on the shared pool.
  - kernel memory that RACCT does not count at all: mbufs and socket buffers, pf states in the jail's own pf (`/dev/pf` is unhidden), and sockets.
  
  Separately, a `container:write` user (B) can create a jail with no limits, or remove a jail's limits with `PUT` (limits are replaced wholesale, `pkg/models/containers.go:262-263`; the confirmation is only in the UI, `0e266f4`).
- **Gain:** host-wide denial of service (swap exhaustion, the OOM killer reaching host daemons, thread exhaustion, I/O starvation of other jails and VMs) by a non-admin user or by root in a limited jail.
- **Confidence:** the missing rules are confirmed by reading. The impact of each resource is likely.
- **Proof on hearst:** in a jail with `memory=256M`, allocate and touch 1 GiB and watch `swapinfo` rise. In a jail with `max_processes=50`, spawn threads until `kern.threads` or `vm` limits bite. Both should be bounded and approved first.

---

## Low

### SR-06 (low): if the cleanup after a `/dev` guard refusal fails, the jail stays, is later seen as running, and a stop then runs its `rc.shutdown` under the refused `/dev`

- **Where:** `internal/hostops/jails_host.go:660-668` (`discardAfterFailedStart` logs and returns, leaving the jail and its epairs). `internal/containers/actions.go:137-144` records the container as stopped. `internal/containers/service.go:217-252` and `295-301`: `observe` then marks it running, and the start-up sweep handles only `ContainerStarting`. `jails_host.go:772-789`: `StopJail` rewrites the jail.conf **with** `exec.stop` and runs `jail -r`.
- **Trigger:** the guard or a parameter check refuses the jail (for example because of a host devfs misconfiguration, which is exactly what the guard is there to catch), and the discard then fails (a `JailRunDir` check, `jail -r` timing out, or `ifconfig destroy` failing). Later any `container:power` user, or the UI, stops the "running" jail.
- **Gain:** the first code to run in a jail the guard refused is its own `/etc/rc.shutdown`, with the devices the guard objected to. That is the outcome amendment i exists to prevent. Two failures are needed.
- **Confidence:** the code path is confirmed by reading. The precondition is speculative.
- **Proof:** a unit test with the fake host: make `discardJail` fail after `checkJailDevfs` refuses, then call `StopJail` and assert that exec.stop was written. No hearst run is needed.
- **Direction:** record a "guard_refused" state that `StopJail` and the sweep treat as discard-only.

### SR-07 (low): the duplicate-IP refusal can race, and covers only the same bridge and only static addresses

- **Where:** `internal/containers/network.go:251-282`, called from `actions.go:92` at submission and at run time.
- **Trigger (B):** two `start` tasks for two different containers with the same static IP, submitted together. The engine serialises tasks per resource only, so each check runs before the other jail exists. The same applies to two allow-listed bridges on one L2 segment, or a DHCP jail leased the same address.
- **Gain:** two running jails share an IP address, which disrupts the other tenant's service.
- **Confidence:** confirmed by reading.
- **Proof:** a unit test with the fake host and two concurrent `runStart` calls.

### SR-08 (low): when a jail is removed outside Skep, its interface comes back with a name and description chosen by jail root, which can collide with host interfaces or another jail's epair

- **Where:** `internal/hostops/jailnet.go:328-354` (`DestroyJailInterfaces` lists `ifconfig -g epair`, reads and destroys by **name**), `jailnet.go:59` (`epairHostRE` accepts any `epairNa`), `internal/hostops/net.go:92-107` (`tapDescription` by name).
- **Trigger (A, needs an operator `jail -r` without Skep's jail.conf, or a similar removal outside Skep):** inside the jail, root renames `skep0` to `vtnet0`, or to the name of another jail's host end (for example `epair3a`), and sets its description to `skep:<other-container-id>:0`. When the jail is removed outside Skep, the interface returns to the host vnet under that name.
- **Gain:** duplicate interface names on the host. Name-based operations then become ambiguous for Skep (`ifconfig <name>`, `ifconfig <name> destroy`), for `IsManagement` (`confinement.go:218`), and for rc.d and pf rules naming `vtnet0`. Every Skep removal path destroys the host ends first (`jails_host.go:741-751`), so this can't happen through Skep. I found no cross-tenant destroy, because the read and the destroy resolve to the same first match.
- **Confidence:** speculative (it depends on how `if_vmove` handles name clashes, which amendment k's probe suggests allows duplicates).
- **Proof on hearst:** in a test jail, `ifconfig skep0 name epair999a description skep:00000000-0000-0000-0000-000000000000:0`, then `sudo jail -r skep-<id>` without `-f`, then `ifconfig -g epair` and `ifconfig -a | grep -c '^vtnet0'` for the vtnet0 variant. This needs approval, because it is a removal outside Skep.

### SR-09 (low): the in-jail writer opens a file without `O_NONBLOCK`, so a FIFO in the tree can hang an agent task forever

- **Where:** `internal/hostops/jailwrite_freebsd.go:131-153` (`readSkepFile`: `Openat(O_RDONLY|O_RESOLVE_BENEATH|O_NOFOLLOW)`, and the type is checked only after the open). It is reached for `etc/rc.conf` and `etc/resolv.conf`.
- **Trigger:** a never-started tree whose `etc/rc.conf` or `etc/resolv.conf` is a FIFO. Writes happen only before the first start (`StartedOnce`, including for clones and rollbacks, `snapshots.go:617`, `:821`), so jail root cannot set this up. Only the content of a template, from a pkgbase package or the operator's `base.txz`, can.
- **Gain:** `open(2)` blocks with no writer. The task goroutine never returns (the context cannot interrupt the syscall), and a worker slot is lost until the agent restarts.
- **Confidence:** confirmed by reading. The trigger is unlikely.
- **Direction:** open with `O_NONBLOCK`, or `fstatat(AT_SYMLINK_NOFOLLOW|AT_RESOLVE_BENEATH)` and refuse non-regular files before opening.

### SR-10 (low, out of scope: local host user): the dist source checks the directory, not the archive files, and hashes then extracts by path

- **Where:** `internal/hostops/templates_pkg.go:279-316` and `319-331`. `checkRootOwnedDir` checks only `dist_dir`. `base.txz` and `MANIFEST` are opened by path, following symlinks, with no owner or mode check. The archive is hashed, then `tar` re-opens it by path.
- **Trigger:** a host user who owns `base.txz` or `MANIFEST` (for example after `sudo mv` kept their ownership) or can write to them. They replace both files, or swap the archive between the hash and `tar -x -p`.
- **Gain:** the attacker's userland goes into every jail built from the template, extracted by root with `-p` (setuid bits kept). The MANIFEST hash only shows the two operator files agree; it is not an authenticity check.
- **Confidence:** confirmed by reading.
- **Direction:** open the archive once with `O_NOFOLLOW`, check `fstat` owner 0 and mode, hash it and pass the same descriptor to tar (`-f /dev/fd/N` or stdin).

### SR-11 (low, RBAC design): rollback, which destroys the jail's current data and newer snapshots, needs only `container:snapshot`

- **Where:** `internal/api/containers.go:52` (`POST /containers/{id}/snapshots/{sid}/rollback` → `PermContainerSnapshot`), `internal/containers/snapshots.go:436-475`.
- **Trigger (B):** a "backup operator" role holding `container:snapshot` but not `container:delete` rolls a jail back to its oldest snapshot with `destroy_newer=true`.
- **Gain:** the jail's current contents and all newer snapshots are destroyed, which is effectively a data delete without `container:delete`. It is audited.
- **Confidence:** confirmed by reading. Whether this is intended is a policy question for Andy.

---

## Info

### SR-I1: no per-container ownership; a clone duplicates the source's secrets

The RBAC checks a global permission, not per-resource ownership (`internal/api/containers.go:41-53`). Any `container:write`, `container:delete` or `container:power` user acts on every jail. A clone (`container:write` + `container:snapshot`) copies the source's whole tree, including `/etc/ssh/ssh_host_*` keys generated at its first start, and `SSHAuthorizedKey` (`snapshots.go:617`). The result is two jails with the same SSH host keys, so either can impersonate the other to SSH clients. The cloner also gets the data of any jail. This is not a bug against the current model; it matters once tenants exist.

### SR-I2: console.log content is controlled by the jail

`GET /containers/{id}/console-log` (`container:read`) returns bytes that jail root wrote (SR-01). The web UI renders them as text (I found no `dangerouslySetInnerHTML` or `innerHTML` under `web/src`), and `skepctl` has no console-log command yet. When one is added, it should strip terminal escape sequences.

### SR-I3: ZFS ownership properties assume jail root cannot use `/dev/zfs`

Rulesets 5 and 6001 unhide `zfs`, `fuse`, `crypto` and `pf` (`jails.go:455-459`). The ownership checks (`skep:container_id`, `skep:template_id`, `skep:snapshot_id`, for example `jails_host.go:65-82`) are a security boundary. They hold only if jail root cannot set ZFS user properties or snapshot through `/dev/zfs` on a dataset that is not jailed. I believe that is true (no dataset is `jailed=on`, and `allow.mount.zfs` is off), but it is unverified. **Proof on hearst:** inside a test jail, `zfs list`, `zpool list`, and `zfs set skep:container_id=x <its dataset>` should all fail or show nothing.

### SR-I4: the `/dev` guard checks a single point in time

The guard lists the top level of `/dev` once, before `/etc/rc` (`jails_host.go:552-569`). Devices created later that match an unhide rule appear without a check, for example `bpfN` clones (intended), or `ptyp*` and `ttyp*` from ruleset 3 if `pty(4)` is loaded on the host later. Only the top level is listed, so `fd/` and `pts/` contents are trusted to the ruleset. This is by design (amendment i); recorded for coverage.

### SR-I5: kernel attack surface of a default jail

A default jail has `/dev/zfs`, `/dev/fuse` and `/dev/pf` ioctls, raw sockets (VNET), and in DHCP jails BPF. None is a Skep bug. It is the surface a kernel bug would be reached through.

### SR-I6: the build jail can read the host routing table

The build jail inherits the host's network stack (`vnet=inherit`, `ip4=disable`, `ip6=disable`, `templates_jail.go:93-115`). A route socket (`AF_ROUTE`) is still allowed, so the fixed build commands could read the host's routing table. Only allow-listed base binaries run there, so this is negligible.

---

## Areas checked where I found nothing

1. **Template builder:** `buildJailArgs` sets every allow.* to 0 and sysv* to disable, with no devfs, no IP and securelevel 2. `checkBuildJail` checks every reported `allow.*` is false and that sysv* is `disable` (`templates_jail.go:93-194`). Package code never runs on the host (`RUN_SCRIPTS=false`, `PKG_TRIGGERS_ENABLE=false`, `templates_pkg.go:158-168`). The fixed build commands run through `jexec -l` with fixed argv in the jailed environment. The allowlist is keyed on (package, kind, phase, SHA-256), covers both shell and Lua scripts, and refuses trigger entries that are symlinks or other non-regular files (`allowlist.go:180-242`). `freebsd-version` is read, never run (`templates_pkg.go:239-271`). The order is SSH host key check, catalogue removal, empty cache check, host file comparison, then `@skep-base` before `readonly=on` (`builder.go`, `templates.go:468-490`), and no writer runs between the snapshot and readonly. Clones set `readonly=off` and carry their own ID.
2. **jail.conf and the jail -c parameters:** every value is typed or validated. `confValue` refuses quotes, `$`, `;`, braces and newlines. There is no `exec.start`, `exec.prestart` or `exec.poststart`. `checkJailParams` checks every reported `allow.*` (only suser, reserved_ports and mlock-as-asked are on), and vnet, enforce_statfs=2, children.max=0, securelevel=2, devfs_ruleset, persist and sysv* (`jails.go:396-446`). `allow.sysvipc=0` is set by the shared allow list.
3. **devfs ruleset 6001:** it is checked to match exactly and never overwritten when it differs (`jailnet.go:189-215`). The guard runs with nothing running in the jail. `dev` is opened `O_RESOLVE_BENEATH|O_NOFOLLOW` from an `O_NOFOLLOW` root, and `fstatfs` must show devfs mounted exactly at `<root>/dev` (`jaildev_freebsd.go:19-40`). A refusal removes the jail without exec.stop. The only exception is SR-06.
4. **devfs unmount by fsid:** the mount table is matched on exact `f_mntonname` and `devfs`, and the unmount is `MNT_BYFSID`, so no path is resolved. A zero fsid is refused, and running jails are refused (`jaildevfs.go:73-138`, `jaildevfs_freebsd.go`). Jail root cannot mount (`allow.mount=0`, `enforce_statfs=2`) or redirect the jail's own devfs mount (jail(8) refuses a symlinked mount point, and processes are dead before `jail -r` unmounts). I found no way to point the unmount at a host mount.
5. **/etc/rc through jexec:** the process is jailed after `jail_attach` (NSS and login.conf parsing happen jailed). It cannot signal the agent across the prison boundary. It gets only fixed environment variables. The start timeout kills it, and `jail -r` follows. Apart from SR-01, I found no host effect.
6. **RCTL apply, read-back and removal:** rules are added after `jail -c` and before any code runs. The read-back is an exact sorted comparison. Leftover rules are cleared before every start and after every removal. RACCT being off refuses limits. An agent crash mid-start is discarded by the sweep. I found no path that leaves a jail running without the limits it was given (apart from limits being optional, SR-05).
7. **Interfaces:** the host end is created and described in one ifconfig call. The jail end gets an ID-derived name on the host, a `checkMovedIn` check, and a rename to `skep<slot>` inside the jail before any jail code runs, with the output checked. Every Skep removal destroys the epairs first. Destruction is by description only, and an epair with no description is never destroyed. Interfaces the jail creates itself die with its vnet. The remaining edge case is SR-08.
8. **Clones and static addressing:** a clone keeping the source's static address is refused. New addressing is written only into a never-started tree, and `started_once` is never reset by a rollback or a clone of a started jail. Snapshot and rollback need the jail stopped (the snapshot of a running jail is marked crash-consistent). MAC uniqueness against Skep jails and VMs is enforced in the DB, apart from SR-03.
9. **Edit/PUT:** an edit is refused while the jail runs (checked at submission and in the task). Writes after the first start are refused (`container_started`). Limits and options are re-validated at every start. Limits are replaced wholesale, as documented (SR-05 covers the security side).
10. **RBAC:** each route's permission matches what its handler does. Reads are `container:read`, create and edit are `container:write`, delete is `container:delete`, actions are `container:power`, and snapshot create, delete and rollback are `container:snapshot` (see SR-11). Clone requires both permissions, enforced as "all of" (`api.go:137-145`). Templates are `storage:read` and `storage:write`. Metrics need `system:read` plus `vm:read`, `storage:read` and `container:read`. Idempotency replays are bound to the actor.
11. **The in-jail writer:** every open is relative to the root's descriptor with `O_RESOLVE_BENEATH|O_NOFOLLOW`. A temporary file plus `renameat` replaces a symlink or hard link instead of following it. The directory is `fchown`/`fchmod`ed after `openat`. A file without Skep's header is never replaced or removed. It runs only on trees that have never run code, with no concurrent writer. rc.conf lines are restricted to a strict assignment regex, SSH keys are normalised and options refused, and nameservers are plain IPs. The remaining case is SR-09.
12. **Out of the list:** the host-side readers of jail trees after they have run code are limited to the devfs listing, the mount table and zfs by dataset name. The agent never parses files from a tree that has run code. The jail and template parents are checked root:wheel 0700, so host users cannot reach setuid binaries a jail plants. `JailRunDir` is checked as owned by the agent and not group- or world-writable.

---

## Triage (2026-11-18)

Andy's dispositions, recorded as amendment n of ADR 0010 before any code was
written.

| Finding | Severity | Disposition |
|---|---|---|
| SR-01 console.log descriptor | high | Probe and propose only; not built in this run. The options are compared in step10-progress.md, and Andy decides the fix. |
| SR-02 L2/L3 anti-spoofing | medium | Deferred until the spoofing tests with Andy at OPNsense. No code. **2026-11-21: decided (ADR 0010 amendment y), build pending, release blocker.** |
| SR-03 arbitrary NIC MAC | medium | Fix now: unicast, locally administered, not all zeros (422 in create, edit, clone and the UI); the agent refuses at start a MAC already on the target bridge, its members, or another Skep container. |
| SR-04 unbounded locked memory | medium | Fix now: `locked_memory` required with `mlock`; applied as its own RCTL rule and read back whenever set, with or without `memory`. |
| SR-05 RCTL coverage, optional limits | medium | Documentation now (what the limits do not cover, and that `container:write` can create an unlimited jail). Default limits or limits required per role: backlog. |
| SR-06 failed guard plus failed removal | low | Fix now: a quarantined failed state with its own code and hint; never running, never `exec.stop`/`rc.shutdown`; start and stop refused; delete and the sweep retry removal without `exec.stop`. |
| SR-07 duplicate-IP race | low | Backlog. The race between two starts is closed (2026-11-22, branch `fix/sr07-address-race`): `MarkStarting` repeats the check in its writing transaction. Other bridges on one L2 segment and DHCP leases stay out of scope. |
| SR-08 interface renamed by jail root | low | Backlog. |
| SR-09 FIFO in the in-jail writer | low | Fix now: `O_NONBLOCK` plus the existing flags, `fstat`, refuse anything not a regular file before any read or write. |
| SR-10 dist source archive checks | low | Backlog. |
| SR-11 rollback needs only `container:snapshot` | low | Fix now: rollback needs `container:write` and `container:snapshot`, the same as clone. |
| SR-I1 no per-container ownership; clone secrets | info | Backlog. |
| SR-I2 console.log content is jail-controlled | info | Backlog (strip escapes in any future `skepctl` console-log command). |
| SR-I3 `/dev/zfs` inside a jail | info | Hearst validation list. |
| SR-I4 `/dev` guard is point-in-time | info | Backlog. |
| SR-I5 kernel attack surface | info | Backlog. |
| SR-I6 build jail route socket | info | Backlog. |
