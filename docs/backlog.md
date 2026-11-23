# Backlog

Work that has been decided on in principle but is deliberately not
scheduled. An entry here is not a commitment to a release; each needs its
own design (and, where it changes behaviour, Andy's approval) before it is
built. Entries say when they may be picked up.

| Item | When | Where it came from |
|---|---|---|
| Skep logs to syslog itself (a `slog` handler sending one record per message to `/var/run/log`), instead of daemon(8)'s `-S` pipe, so records are never joined on one `daemon.log` line | After 0.1 | [daemon-log-joined-records.md](../tests/integration/rcd/daemon-log-joined-records.md), option B (Andy, 2026-11-22) |
| Agent drain mode for `skep-setup upgrade`: the agent stops accepting new tasks and finishes or refuses the queued ones, instead of setup stopping `skep_api`, so a late refusal no longer leaves the new API answering `503 agent_version_mismatch` next to the old agent | After 0.1 | Lab-VM validation of PR #52 (2026-11-22); Andy accepted the 503-after-late-refusal behaviour for 0.1 |
| The agent and the API back up their database before any schema migration, not only `skep-setup upgrade` | After 0.1 | PR #52 fix round (2026-11-22) |
| Start-up sweeps write audit entries, actor `system`: the half-started sweep (amendment i), the quarantine sweep (amendment n), and the stop completion (amendment aa) | After 0.1 | ADR 0010 amendment aa, Andy's answer 3 (2026-11-23) |
| `skep-setup remove -revert-host` (and its plan) ignores journals of an earlier install: today the journals survive `pkg delete` and `-purge-data`, so after a reinstall the revert plan also lists lines an earlier install added (the lines seen are in the clean-install check's results on PR #54) | After 0.1 | Finding 1 of PR #52 (2026-11-22, backlog only by Andy's instruction) |
| Decided (Andy, 2026-11-23): **yes**, the skep package depends on `edk2-bhyve`, so a package install has the bhyve UEFI firmware (jails-only hosts get it too). Built in PR #61 (`fix/setup-live-modules`); `skep-setup check` and `plan` keep the warning for a host where the firmware is missing anyway | Decided | Setup readiness (2026-11-22, unattended run) |
| File the OpenZFS report drafted in [upstream/openzfs-zvol-volmode-dev-node.md](upstream/openzfs-zvol-volmode-dev-node.md) | Andy | Finding 5 of the lab-VM validation (2026-11-22) |
| A health check that reports Skep zvols at `volmode=none` (for example after the agent died between the two sets of a repair, ADR 0010 amendment z) | After 0.1, unless Andy wants it now (question 3 of amendment z) | `fix/zvol-device-node`, 2026-11-22 |
| Measure the zvol device-node miss rate (a zvol created with no `/dev/zvol` node 1 s later) on a file-backed pool, hearst's `skeptest`: so far measured only on the lab VMs' disk-backed pools (45/50 on smith, 43/50 on langrishe, [results Part 2](../tests/integration/setup/results/zvol-device-node-20261122.md)). The amendment z repair covers any rate; this records whether file-backed pools behave the same | When hearst is back on | zvol device-node validation (2026-11-22, open item); added to the backlog on Andy's instruction (2026-11-23 run) |
| `skep-setup check` reports the paths the agent's start check refuses (binary, `agent.json`, `/var/db/skep`, `/var/run/skep` and parents, with ACLs), so the operator sees a refusal before an upgrade restarts the agent | After 0.1 | ADR 0004 amendment of 2026-11-23 (PR #63) |
| The data-directory check in `skep-setup` reads mode bits only, not ACLs: align it with ADR 0004's ACL amendment (PR #63) | After 0.1 | Fable review of PR #58 |
| The manual-upgrade pointer printed by `skep-setup upgrade` is a URL to `main`: ship the document with the package or link the release tag | After 0.1 | Fable review of PR #58 |
| An optional pinned IPv4 address for VM NICs (anti-spoofing beyond MAC) | Not scheduled | [ADR 0011](decisions/0011-guest-port-anti-spoofing.md) decision 7 |
| Default jail limits, or limits required per role | Not scheduled | [Jails](administration/jails.md#limits) |
| An option to reset a jail's SSH host keys on clone | Not scheduled | [Jails](administration/jails.md) |

Not planned for now: FreeBSD problem reports for the daemon(8) joined
records and the `rc.d/routing` "File exists" lines at jail start (Andy,
2026-11-22). Both are documented as harmless base-system behaviour.
