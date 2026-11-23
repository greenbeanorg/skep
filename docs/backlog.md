# Backlog

Work that has been decided on in principle but is deliberately not
scheduled. An entry here is not a commitment to a release; each needs its
own design (and, where it changes behaviour, Andy's approval) before it is
built. Entries say when they may be picked up.

| Item | When | Where it came from |
|---|---|---|
| Skep logs to syslog itself (a `slog` handler sending one record per message to `/var/run/log`), instead of daemon(8)'s `-S` pipe, so records are never joined on one `daemon.log` line | After 0.1 | [daemon-log-joined-records.md](../tests/integration/rcd/daemon-log-joined-records.md), option B (Andy, 2026-11-22) |
| Agent drain mode for `skep-setup upgrade`: the agent stops accepting new tasks and finishes or refuses the queued ones, instead of setup stopping `skep_api`, so a late refusal no longer leaves the new API answering `503 agent_version_mismatch` next to the old agent | After 0.1 | Lab-VM validation of PR #52 (2026-11-22); Andy accepted the 503-after-late-refusal behaviour for 0.1 |
| `skep-setup`'s prober reads the live bridge members and `vlanfilter` state (`ifconfig BRIDGE`), not only `rc.conf`, so a bridge changed at run time is judged as it is | After 0.1 | PR #52 fix round (Fable review, 2026-11-22) |
| The agent and the API back up their database before any schema migration, not only `skep-setup upgrade` | After 0.1 | PR #52 fix round (2026-11-22) |
| The agent finishes a half-stopped jail at start (a jail left between stop steps by a crash or an upgrade restart) | After 0.1 | PR #52 fix round (2026-11-22) |
| `skep-setup` and the agent agree on whether the FreeBSD-base (pkgbase) repository is usable: setup reads the repo files, the agent probes differently | After 0.1 | PR #52 fix round (2026-11-22) |
| `skep-setup` checks that `/var/db/skep` (and its parents) are root-owned and not group or other writable before using it | After 0.1 | PR #52 fix round (2026-11-22) |
| `skep-setup`: a journal with an unknown format or a wrong ID no longer makes every command fail; it is reported and set aside like an unreadable one, with a way to clear it | After 0.1 | PR #52 lab re-check (2026-11-22) |
| `skep-setup remove -revert-host` (and its plan) ignores journals of an earlier install: today the journals survive `pkg delete` and `-purge-data`, so after a reinstall the revert plan also lists lines an earlier install added (the lines seen are in the clean-install check's results on PR #54) | After 0.1 | Finding 1 of PR #52 (2026-11-22, backlog only by Andy's instruction) |
| `skep-setup` passes the rc.d script's `-data-dir` and `-config` (`skep_api_data_dir`, `skep_api_config`) to `skep-api admin create` and `admin exists`, which today use skep-api's defaults while setup locates `api.db` from `rc.conf` | After 0.1 | Finding 1 fix (2026-11-22) |
| `skep-setup apply -admin NAME` where NAME exists but is disabled or not an admin is a printed warning, not `ok` | After 0.1 | Andy, 2026-11-22 (review of the finding-1 fix, PR #52) |
| File the OpenZFS report drafted in [upstream/openzfs-zvol-volmode-dev-node.md](upstream/openzfs-zvol-volmode-dev-node.md) | Andy | Finding 5 of the lab-VM validation (2026-11-22) |
| A health check that reports Skep zvols at `volmode=none` (for example after the agent died between the two sets of a repair, ADR 0010 amendment z) | After 0.1, unless Andy wants it now (question 3 of amendment z) | `fix/zvol-device-node`, 2026-11-22 |
| An optional pinned IPv4 address for VM NICs (anti-spoofing beyond MAC) | Not scheduled | [ADR 0011](decisions/0011-guest-port-anti-spoofing.md) decision 7 |
| Default jail limits, or limits required per role | Not scheduled | [Jails](administration/jails.md#limits) |
| An option to reset a jail's SSH host keys on clone | Not scheduled | [Jails](administration/jails.md) |

Not planned for now: FreeBSD problem reports for the daemon(8) joined
records and the `rc.d/routing` "File exists" lines at jail start (Andy,
2026-11-22). Both are documented as harmless base-system behaviour.
