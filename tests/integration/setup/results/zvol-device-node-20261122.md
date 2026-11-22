# zvol device-node fix (ADR 0010 amendment z): lab run, 2026-11-22

Branch `fix/zvol-device-node` from `origin/main` 600cd86. Hosts: smith
(10.79.20.81) and langrishe (10.79.20.82), which should have been rolled
back to Proxmox snapshot `fresh-151`, and hearst (10.79.20.170) for the
miss-rate probe only. Andy away; run unattended. Logs are in
`~/skep-test-logs/` on merrick. Nothing merged.

**Result: the code is built and unit tested; nothing ran on FreeBSD.**
Both lab VMs failed the clean check, and hearst was unreachable for the
whole run. Everything below the build is still **pending FreeBSD
validation** (see the backlog's "Before 0.1" entry).

## Decisions taken unattended

1. **Both VMs skipped.** The clean check found the previous run's
   leftovers (below). The instruction was to skip a VM that isn't clean,
   so the probe (Part 3) and the agent validation (Part 5) did not run.
   Nothing was changed on either VM: no package, no pool, no scratch
   dataset.
2. **Built without the probe.** The rule "stop the build if none→dev
   fails for a clone, rollback, encrypted zvol, or the GPT check" could
   not be applied, since nothing was probed. The fix was built anyway
   and its ADR amendment stays Proposed. The PR is not merged without
   Andy, and the code refuses rather than guesses outside the case that
   was observed (plain zvols, 5/5).
3. **Post-operation checks don't fail the task.** See ADR 0010 amendment
   z, question 1.
4. **Stricter than asked:** owner properties checked, no vmm instance
   required, and `volmode=dev` attempted after any attempt of
   `volmode=none` (amendment z, question 2).

## Part 1: clean?

| Check | smith | langrishe |
|---|---|---|
| /var/db/skep | **present**: `setup/` journals 20261122T213041Z, …213117Z, …213136Z, …213205Z | **present**: `setup/` journals 20261122T213307Z, …213329Z, …213342Z, …213352Z |
| skep account | absent | absent |
| skep package | absent | absent |
| Skep bridge | absent (vtnet0 vtnet1 lo0) | absent (vtnet0 vtnet1 lo0) |
| pools | **tank** (`zpool create tank da1`, 2026-11-22 16:30 local); `zfs_enable="YES"` | zroot only |
| uptime / last logins | 1:12; previous run's sessions 16:31–16:32 | 1:12; previous run's sessions 16:33 |
| Result | **not clean, skipped** | **not clean, skipped** |

This is exactly the end state recorded in
[lab-vm-clean-install-20261122.md](lab-vm-clean-install-20261122.md), so
the rollback to `fresh-151` had not happened (or the snapshot was taken
after that run).

## Part 3: raw zfs probe

Not run (Part 1). No numbers.

## Part 4: build (merrick)

| Test | Result | Log |
|---|---|---|
| `go test -race ./internal/hostops -run 'EnsureVolumeDevice\|EnsureDevice' -v`: every refusal of rules 3 and 5 with no `zfs set`, rounds 1 and 2, the two-round limit, the forced `volmode=dev` after cancellation, restore failure | PASS (25 cases) | 20261122T223609Z-go-test-hostops-zvoldev.log |
| `go test -race ./internal/vm -run DeviceNode -v`: repair before the runner, refusal while running, start fails with `disk_device_missing`, create/rollback/clone/disk-add check without failing | PASS; fails when the start-time call is removed | 20261122T223700Z-go-test-vm-devices.log |
| `make test` | PASS (exit 0) | 20261122T223741Z-make-test.log |
| `make lint`, FreeBSD cross-build, `make web-check` | PASS | 20261122T223932Z-make-lint.log, 20261122T223913Z-make-lint-webcheck.log |

## Part 5: validation through the agent

Not run (Part 1). The VM-running refusal can't be reached through the
API today: `vm.start` of a running VM is refused earlier (`vm_not_stopped`),
and every other caller requires a stopped VM. It is covered by unit tests
only, which is as intended (defence in depth).

## Part 6: hearst

Unreachable for the whole run ("No route to host", ping 100 % loss) at
22:27Z, 22:42Z, 22:48Z, and again before the report. The backup
directory `/var/tmp/skep-backup-20261122T150343Z` was **not** deleted,
and the miss-rate probe in `skeptest/zvolprobe` did not run.
