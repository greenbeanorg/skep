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
- **hearst: 3 of its 12 API users checked (2026-11-23).** The API refused the
  hard-coded password for all three (s7admin, s7oper and s7viewer), with one
  login attempt per user. The other nine are lab test accounts, not checked,
  that Andy will reset or remove.

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

## Refresh — 2026-11-24

Everything since the scan above (2026-11-23 02:50Z), written unattended;
as above, no value is reproduced (file, commit and kind only). Logs on
merrick: `/home/aba/skep-test-logs/20261124T072543Z-overnight-job4-scan/`.

**Scope.** The 34 commits on `main` after `683a860` (the last commit before
the first scan) up to `a46d8ed`, and the original commits of every pull
request from #55 on (`refs/pull/*/head`, 37 refs, fetched for the scan:
148 commits not on `main` before `683a860`); the titles, bodies and
comments of pull requests #71 to #91. Not covered, as before: issue
bodies, Actions logs and artifacts.

**Secret scan.** gitleaks v8.21.2, default rules, `--redact`: main range
34 commits, pull request commits 148 commits — **no findings**. The
second sweep (private keys, bearer tokens, JSON session tokens, AWS and
GitHub tokens, `$argon2id$` hashes, `password = "…"` assignments, SSH
public keys) over the lines those 150 commits add found only
`docs/security/pre-publication-report-2026-11-23.md` (commits ce1d910128,
d7d2b32bd0), whose `$argon2id$…` and `ssh-ed25519 AAAA…` are this
report's own pattern descriptions. Nothing needs removing.

**Pull request text.** 21 pull requests (#71–#91: titles and bodies), 38
conversation comments, 0 review comments, 0 reviews; gitleaks, default
rules, and the same patterns plus ordinary mailbox addresses: **no
findings**.

**Identities.** All 34 new `main` commits are authored by the project
account's GitHub noreply address, with `noreply@github.com` as committer:
**squash merges no longer use the ordinary mailbox address.** The 159
pull request commits use the noreply address as author and committer.
Co-author trailers name only `noreply@anthropic.com`. The 54 older merges
still carry the mailbox address (unchanged; a history rewrite remains
Andy's decision).

## Second refresh — 2026-11-24 (release preparation)

Rerun before publishing the repository, written unattended with the same
method as the two scans above; no value is reproduced. Logs on merrick:
`/home/aba/skep-test-logs/20261124T144112Z-release-prep-job2-scan/` and
`/home/aba/skep-test-logs/20261124T143845Z-release-prep-job2-actions/`.

**Scope.** `main` at `7206264` (after PR #104): the 13 commits after
`a46d8ed`, and also the whole of `main` (105 commits). The original commits
of pull requests #92 to #104 (`refs/pull/*/head`, 13 refs, fetched for the
scan and removed again): 40 commits not on `main` before `a46d8ed`, 9 of
them merges. The titles, bodies and comments of the same 13 pull requests.
**For the first time, the GitHub Actions logs**: every run the token lists.
The repository has no issues, so there are no issue bodies to scan.

**Secret scan.** gitleaks v8.21.2, default rules, `--redact`:

| Scope | Commits | Findings |
|---|---|---|
| `main`, `a46d8ed..7206264` | 13 | none |
| `main`, whole history | 105 | 4, all the known false positives above: `internal/config/api.go` line 268 (83ade69d53) and `tests/integration/jails/results/step10-hearst-run4.txt` lines 296, 352 and 363 (3c04b40b70) |
| PR commits #92–#104 | 31 diffed by gitleaks, plus 9 merges | none |

gitleaks does not diff merge commits. Eight of the nine show no
combined diff of their own, and the ninth (84f4723640, PR #94's branch),
which resolves conflicts in four files, was scanned separately: no
findings and no pattern hits.

**Second sweep** (private keys, bearer tokens, JSON session tokens, AWS and
GitHub tokens, `$argon2id$` hashes, `password = "…"` assignments, SSH public
keys, ordinary mailbox addresses) over the lines the 41 commits add, i.e.
`main` after `a46d8ed` plus the PR commits:
only `docs/security/pre-publication-report-2026-11-23.md` (b9e0f50065,
cc1aa8a10a), where the text is this report's own pattern description. Nothing needs removing.

**Pull request text.** 13 pull requests (#92–#104: titles and bodies), 22
conversation comments, 0 review comments, 0 reviews; gitleaks and the
same patterns: **no findings**.

**Actions logs.** The token lists runs (352) and downloads their log
archives; nothing was refused except one run's archive, run 43097835292
(2026-11-11), which answers 404. 153 archives are empty: runs that never got
a runner, such as those after the Actions minutes ran out. A job log of such a
run answers 404 (`BlobNotFound`). Check-run annotations answer 403
("Resource not accessible by personal access token"), as before. The 198
non-empty archives (1006 log files) were scanned:

- gitleaks: 104 `generic-api-key` hits, one in the "Release build with the
  web interface" job log of each run that got that far. All are the same
  kind: `web-e2e: trusting skep-api's key sha256:…`, printed by
  `scripts/web-e2e.sh` line 48. That is the SHA-256 pin of the **public**
  key of a throwaway API started on the CI runner for the browser tests:
  **false positive**.
- The pattern sweep found nothing. No lab address (10.79.20.x), no lab
  account name and no mailbox address appear in the logs.

**The one Actions artifact**, scanned in a follow-up run the same day
(logs: `/home/aba/skep-test-logs/20261124T152812Z-release-prep2-job1-artifact/`).
It is `playwright-report` (artifact 14763040320, 224,881 bytes, from run
45042859721 on PR #52's branch, created 2026-11-22, expiring 2026-11-29).
The token downloaded it without refusal, and its SHA-256 matches the digest
GitHub lists. It holds one file, the Playwright HTML report, whose embedded
zip holds `report.json`; both were extracted and scanned with the same
method as the logs:

- gitleaks: no findings.
- Pattern sweep (the patterns above plus lab addresses and lab host names):
  two hits, both in the report's metadata. Playwright records the tested
  commit (GitHub's merge commit for PR #52), and its author address is the
  project account's mailbox address, the one on the 54 older commits that
  Andy has accepted as public. Playwright also records the first 100,000
  characters of that PR's diff, which name `hearst`. That is text already
  in the repository, and lab host names are allowed there.
- No private key, token, password, hash or lab address appears.

Once the repository is public, anyone signed in to GitHub can download the
artifact until it expires on 2026-11-29. Nothing in it needs removing.

**Identities on `main`.** Two author addresses: the project account's
ordinary mailbox address on 54 commits (the older squash merges; Andy
has accepted it as public) and its GitHub noreply address on 51.
Committers: `noreply@github.com` (101) and the project noreply address (4).
Co-author trailers name `noreply@anthropic.com` and the project noreply
address only. No other personal address appears.
