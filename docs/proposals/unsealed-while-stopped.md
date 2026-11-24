# Proposal: reading unsealed agent audit entries while the agent is stopped

Status: proposal, no code.

## Today

`skep-agent audit export --unsealed` (PR #88) prints the agent's audit
entries newer than the last sealed segment. It is root only and opens the
database read-only. It refuses while the agent is stopped: a stopped agent
leaves no `-wal` or `-shm` next to `agent.db`, and SQLite creates both even
for a read-only connection and leaves them behind (checked on langrishe,
2026-11-23 and 2026-11-24). The command must change nothing next to a
database it does not own, so it refuses with "no service has this database
open".

That is the moment an operator may most want the log: the agent is down
after a refusal, a crash or an upgrade that went wrong. Today the answer is
to start the agent, which a refusal may prevent.

## Options

| Option | How | Size | For | Against |
|---|---|---|---|---|
| **A. Leave it** | Keep the refusal; the docs say to start the agent, or to copy the files by hand | 0 | Nothing new | No supported reader while the agent cannot start |
| **B. Read a private copy** | With no `-wal` present, copy `agent.db` into a new `0700` directory under `$TMPDIR`, check that the original's inode, size and mtime are unchanged and that no `-wal` appeared during the copy, open the copy read-only, export, delete the copy | About 40 lines | Nothing is created next to the original; works on any database a clean stop left; the copy is checked, not trusted | A copy of the audit log exists briefly in `$TMPDIR` (root only, `0600`, removed afterwards, also on error); a database left by an unclean stop (with a `-wal`) is still refused, because its newest entries are in the `-wal` |
| **C. SQLite `immutable=1`** | Open the original with `immutable=1`, which takes no locks and creates no files; buffer the output, and print it only if no `-wal` appeared and the agent did not start during the read | About 30 lines | No copy; nothing created | If the agent starts during the read, SQLite gives no error, only possibly wrong results: correctness rests entirely on the after-check. Buffers the whole output in memory |
| **D. Create, then remove, `-wal`/`-shm`** | Open as today and delete the two files afterwards | About 15 lines | Simple | Racing an agent start, removing `-shm` under a live writer can corrupt the database. Reject |
| **E. Serve the log through the API** | | | | Ruled out: the agent's audit log is read on the host only, never through the API or the socket (Andy, 2026-11-23) |

## Recommendation

**B, the private copy.** It is the only option that writes nothing next to
the original and does not depend on SQLite behaving well against a file
that might change underneath it. The before/after comparison turns a
racing agent start into a refusal ("the database changed while it was
copied; run it again") instead of wrong output.

Keep A's refusal for a database with a `-wal` (an unclean stop): reading
the copy without it would silently omit the newest entries, and copying the
`-wal` too gives SQLite a recovery to do on the copy, which is safe but
needs its own test. That case can follow later if it is wanted.

## Risks of B

- The copy holds audit data. It must be created with `0600` in a `0700`
  directory made with `os.MkdirTemp`, owned by root, and removed in every
  path, including a failed export and SIGINT.
- `$TMPDIR` may be small (a tmpfs). The command should check free space
  against the database size first and refuse with a clear message.
- The comparison must use the inode as well as size and mtime: an agent
  that starts, writes and checkpoints within the same second could keep
  the size.
- Like today's `--unsealed`, the output is not covered by a segment hash;
  the stderr line says so.

## Questions for Andy

1. Accept B, with A's refusal kept for a database that has a `-wal`?
2. Is a short-lived root-only copy of the audit log in `$TMPDIR` acceptable,
   or should the copy go under `/var/db/skep` (a directory the agent owns,
   which then needs its own cleanup rule)?
