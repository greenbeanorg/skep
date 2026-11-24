# Proposal: an agent drain mode for `skep-setup upgrade`

Status: proposal, no code.

## Today

`skep-setup upgrade` runs after `pkg upgrade skep` has already replaced the
binaries. To stop new tasks it **stops `skep_api`**, waits up to `-wait`
(60 s) for the agent's pending and running tasks, backs up both databases,
restarts `skep_agent`, then starts `skep_api` ([skep-setup](../installation/skep-setup.md#upgrades)).

Every refusal or failure after the API's stop starts `skep_api` again. That
`skep_api` is already the new build, while the agent is still the old one
if the failure came before its restart. Until a later upgrade completes,
the API answers `503 agent_version_mismatch` to everything that needs the
agent and its health is `unavailable`. Andy accepted this for 0.1
(2026-11-22, seen on both lab VMs); the backlog asks for a drain mode
instead. Stopping the API also takes the web interface and reads away for
the whole wait.

## Options

| Option | How | Size | For | Against |
|---|---|---|---|---|
| **A. Leave it** | As today | 0 | Validated on both lab VMs | The 503 window after a late refusal; no interface during the wait |
| **B. Drain over the agent socket** | A new internal operation, allowed only for a root peer (setup runs as root; the socket already checks peer credentials): the agent refuses new task submissions with a new error (`503 agent_draining`) and keeps serving reads and running tasks. Upgrade drains instead of stopping the API, waits for the tasks, backs up, restarts the agent; the API is restarted last. Drain state is in memory only, so any agent restart ends it, and an `undrain` operation ends it on a refusal | About 120-180 lines plus tests, an internal protocol change and a new error code | The API stays up; a refusal before the agent's restart undrains the **old** agent, which keeps working with the **old** API still running: no version mismatch | New error code and internal operation (API contract, Andy's approval); the old API never knows the agent drains, so it must pass the agent's 503 through as is |
| **C. Drain flag file** | Setup creates `/var/run/skep/drain`; the agent checks it on every submission | About 40 lines | No protocol change | Hidden state on disk: a crashed upgrade leaves the host refusing all tasks until someone finds the file; `/var/run` is cleared only at boot |
| **D. Signal** | `SIGUSR1` drains, `SIGUSR2` undrains | About 50 lines | No protocol change; state in memory | No acknowledgement: setup cannot tell that the drain took effect before it counts tasks; signals are easy to send by mistake |

## Recommendation

**B, drain over the socket.** It removes the cause of the 503 window rather
than narrowing it: the API is not restarted until the agent is the new
build, so the old pair keeps working on every refusal path. Its state ends
with the agent process, so a crashed upgrade cannot leave a host stuck.

Keep today's API stop as a fallback when the running agent predates the
drain operation (the first upgrade onto a build that has it).

## Risks of B

- A task submitted between the drain and the count must be refused, not
  half-accepted: the drain must take the same lock as task submission.
- An old API in front of a draining agent shows `503 agent_draining` to a
  user; the message must say an upgrade is running.
- Scheduled internal tasks (retention, for example) must also wait or be
  skipped while draining, or the wait never ends.
- The peer check must admit only uid 0 for drain and undrain; the API's
  account must not be able to drain the agent.
- A second upgrade started while one runs: the drain must report who holds
  it, or refuse.

## Questions for Andy

1. Accept B, with the API stop kept as a fallback for an agent without the
   operation?
2. Is a new error code `agent_draining` (503) acceptable, and should reads
   stay available during the drain?
