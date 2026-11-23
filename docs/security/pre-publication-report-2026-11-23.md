# Pre-publication report — 2026-11-23

> **The scan is rerun at the tag commit.** Counts here are as of
> 2026-11-23 02:50Z.

What would become public if the GitHub repository `greenbeanorg/skep` were
made public today. Written unattended; no value found by the scans is
reproduced here (file, commit and kind only). Logs on merrick:
`/home/aba/skep-test-logs/20261123T025040Z-job6b-gitleaks/`.

## Scope

- Every ref of the local clone after `git fetch --all` (main, the open
  branches) **and every pull request's original commits**
  (`refs/pull/*/head`, 63 refs, fetched for the scan and removed again):
  GitHub serves those to anyone once the repository is public, even for
  squash-merged and deleted branches. 454 commits.
- PR text is covered separately below.
- Not covered: issue bodies (the token used gets 403 on the issues list),
  Actions logs and artifacts. The repository has no wiki. Actions logs in
  particular contain test output; review them, or delete old runs, before
  publishing.

## Secret scan

Scanner: gitleaks v8.21.2 (installed under `~/go/bin` on merrick), default
rules, `--log-opts=--all`, `--redact`.

| File | Commit(s) | Kind | Verdict |
|---|---|---|---|
| `internal/config/api.go` (lines 225 and 268 in two versions) | 129bf8436d, 83ade69d53 | generic-api-key | **False positive**: an assignment from the constant `DefaultArgon2Passes` |
| `tests/integration/jails/results/step10-hearst-run4.txt` (lines 296, 352, 363) | 3a104038fe, 3c04b40b70 | generic-api-key | **False positive**: "trusting skep-api's key sha256:…", the SHA-256 pin of hearst's API **public** key |

A second sweep with patterns gitleaks' defaults can miss, over the same 454
commits (`git grep` per revision):

| Pattern | Hits | Verdict |
|---|---|---|
| `BEGIN … PRIVATE KEY` | none | |
| `Authorization: Bearer <token>` | none | |
| JSON `"token": "<20+ chars>"` (session files) | none | |
| AWS access key IDs, GitHub tokens | none | |
| `$argon2id$v=19$…` | `internal/auth/argon2.go` (format comment), `internal/auth/argon2_test.go`, `internal/database/database_test.go` | Test vectors and a format description; no hash of a real account |
| `password = "…"` style assignments | `internal/api/rbac_test.go`, `cmd/skep-setup/main_test.go`, `internal/setup/plan.go` (an `/etc/passwd` message), `tests/integration/adminprompt/main.go`, `tests/integration/skepctl/main.go` | Unit-test fixtures. The two integration programs hard-code one test password for the temporary users they create and delete on the lab host; harmless once public **only if no account with it is ever left behind** (both programs delete their users) |
| `ssh-ed25519 AAAA…` | `tests/integration/jails/results/step7/ssh-before-start.txt`, `step8/dhcp-jail.txt`, `step10-hearst-run3.txt` | Public host keys of deleted test jails |

Nothing needs removing from history for secrets.

## PR titles, bodies and comments

Dumped with `gh api` on 2026-11-23 and scanned with gitleaks v8.21.2, default
rules, `--redact`: 67 pull requests (titles and bodies), 14 conversation
comments, 0 review comments, 0 reviews. **No findings.** Issue bodies could
not be read (403, above). Log on merrick:
`/home/aba/skep-test-logs/20261123T205012Z-jobC-pr-text-gitleaks/`.

## The hard-coded integration-test password

`tests/integration/adminprompt/main.go` and `tests/integration/skepctl/main.go`
hard-code one password. Checked 2026-11-23:

- **smith and langrishe: not in use.** Each has one account, `admin`, and its
  working password differs from the hard-coded one (compared locally, no
  value printed or logged).
- **hearst: not checked, powered off: s7admin, s7oper, s7viewer.**

## Identities that would become public

Three author/committer identities appear (the list with addresses is in the
local stop report of this run, not here, so that a later history rewrite
would not leave them behind in a file):

- the project account's GitHub **noreply** address: 434 authored commits
  (every branch commit);
- **one ordinary mailbox address of the project account: the author of 54
  commits** — every squash merge on `main` (GitHub uses the account's primary
  address for merges made in the web interface or with `gh pr merge`);
- `noreply@github.com` as the committer of those 54 merges.

Co-author trailers name only `noreply@anthropic.com`. No other person's
address appears.

To keep the mailbox address out of future merges: in the GitHub account's
email settings, keep the address private and choose the noreply address for
web-based Git operations (GitHub then authors squash merges with it). The 54
existing commits keep the address unless history is rewritten, which is
Andy's decision (not done; rewriting history is not approved).

## Also worth a look before publishing (not secrets)

- Lab host names and private addresses (10.79.20.x, VLAN 60) are throughout
  the docs and results by design (CLAUDE.md allows them).
- `CLAUDE.md` and the results files describe the lab and the working
  method; decide whether they stay.
- The trademark notices for FreeBSD® are in the README.
