# Proposal: what the supervisor does with a refused agent

Status: proposal, no code.

## Today

`skep_agent` runs under `daemon(8) -R 5`. An agent refused by its start check
exits 1 and is started again every 5 s, forever. On langrishe (2026-11-23,
mode 0705 on `/usr/local/etc/skep`) that was 9 refusals in 41 s, which syslogd
reduced to one line plus "last message repeated". The agent came back by
itself within 5 s of the fix. That self-recovery is the main thing to keep.

Each retry is cheap: the check runs before the database is opened or the
socket created. Running guests are not affected while the agent is down.

## Options

| Option | How | For | Against |
|---|---|---|---|
| **A. Leave it, with a clearer status** | `service skep_agent status` says "refused, retrying every 5 s" and names the log | Smallest change; keeps self-recovery | Log noise stays |
| **B. Slower retry after a refusal** | The agent waits, for example 30 s, before exiting on a refusal. `daemon(8)` has only a fixed delay, so the wait must be in the agent | Far fewer retries; keeps self-recovery | A fix takes up to 30 s to be noticed; `service stop` must interrupt the wait |
| **C. Retry cap** | `daemon -C N` (at most 128). The supervisor exits after N restarts | One line in the rc.d script | **Counts every restart, not only refusals**, and is never reset: a long-lived agent that crashed N times over months would stop being supervised. After the cap nothing brings the agent back |
| **D. Stop after N identical refusals** | The agent records the refusal and tells the supervisor to stop | Stops only for a persistent, identical cause | New persistent state in a root process before its checks have passed. Needs a way to stop `daemon(8)` from its child. A fix then needs a manual start |

## Recommendation

**A and B together.** Keep retrying forever, at a slower pace after a refusal,
and make the status say what is happening. A host recovers without an operator
once the cause is fixed, which matters most after a reboot where a filesystem
or permission problem clears up.

Reject C: its counter would eventually disable supervision on a healthy
long-running host. Reject D unless log volume proves to be a real problem: it
is the most code, in the most sensitive place.

## Risks of A and B

- The agent must tell a refusal from other failures, so only refusals wait.
- The wait must end immediately on SIGTERM, or `service skep_agent stop` and
  `skep-setup upgrade` slow down.
- `skep-setup upgrade` waits for the new agent. A refused agent is reported
  after its wait either way, but the timing needs a test.
- `rc.d` status parsing must stay correct when run without root, which is
  already unreliable (see the service docs).
- An agent that waits before exiting on a refusal is a live process, so
  `service skep_agent start`, `service skep_agent status` and `skep-setup
  upgrade`'s pid check may read a refused agent as running. B must not be
  built until that is solved.

## Questions for Andy

1. Accept A and B?
2. Is 30 s the right wait after a refusal?
