# Proposal: `skep-setup check` reports the agent's start check

Status: built as option A in PR #74.

## Problem

`skep-setup check` prints the same output whether the agent is healthy or
refused by its start check. The operator learns about a refusal only from
`daemon.log`, or when an upgrade restarts the agent.

## Options

**A. Share the target list and run the check inside `skep-setup`
(recommended).** Move the agent's target list into `internal/startcheck`,
read the same rc.conf values the rc.d script uses (`skep_agent_bin`,
`skep_agent_config`, `skep_agent_data_dir`), and print the running state,
the checked paths and the first refusal with its fix. A refusal is a
`manual` item, so `check` exits 1. A prototype of this measured 212 added
and 30 removed non-test lines. Its unit tests passed on Linux with a fake
filesystem; it has not run on FreeBSD.

**B. The same, default paths only.** About 100 lines fewer, because it
skips the rc.conf overrides. On a host that overrides a path, `check` could
pass while the agent refuses. That false pass is the failure this work is
meant to remove.

**C. Add a `skep-agent check-start` command and have setup run it.** The
smallest change, and the code cannot diverge. But root would execute the
binary whose ownership and mode are being checked, before anything has
checked it. A tampered binary would run as root and could report success.

## Recommendation

Option A. It is larger, but it never runs the binary under check and it
checks the paths the service really uses.

## Risks of option A

- `skep_agent_args` can change the paths and is not parsed. `check` says so
  when the variable is set.
- Without root, or with `-root`, the check does not run and says so,
  because `agent.json` is `0600` and unprivileged `service status` is
  unreliable.
- The refactor touches the agent's start check, which is root-running,
  security-relevant code. Its behaviour must not change.

## Answered questions

1. Build option A at about 210 added lines? Yes, built in PR #74.
2. Should a check that could not run also be exit 1? Yes: it is a "not
   confirmed" item, and `check` exits 1. A malformed `agent.json` stays
   exit 3, because the plan refuses that file first.
