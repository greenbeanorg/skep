# pkgrepo against the real pkg: UCL forms (langrishe, 2026-11-24)

PR #96 taught `internal/pkgrepo` more UCL forms. This compares its answer
for the FreeBSD-base repository with what pkg itself reads, one repository
file per form. The files are in [ucl-forms-20261124/](ucl-forms-20261124/).

## Method

- Host: langrishe (10.79.20.82), FreeBSD 15.1-RELEASE-p7, pkg 2.9.2.
- Each file was placed alone in a fresh `mktemp -d` directory under `/tmp`
  (never under `/usr/local/etc/pkg` or `/etc/pkg`) and read with
  `env REPOS_DIR=<that directory> pkg -vv` as the unprivileged user.
  `pkg -vv` only prints the configuration; it fetched and changed nothing.
  The "Repositories:" section gives whether `FreeBSD-base` is defined and
  its `enabled` value. Every run exited 0. The directory was removed
  afterwards.
- pkgrepo: a scratch program (not committed) ran `pkgrepo.Base` over each
  file placed alone at `usr/local/etc/pkg/repos/FreeBSD.conf` of an
  in-memory file system, so no other file layered over it. It was run with
  main's pkgrepo (f21774b), the PR as reviewed (165c402) and the PR as
  merged.
- "not defined" means no FreeBSD-base repository was read from the file.

## Results

| File | pkg 2.9.2 | main | PR as reviewed | PR as merged | Merged agrees | Form claimed |
|---|---|---|---|---|---|---|
| `01-colon-block.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `02-equals-block.conf` | enabled | not defined | enabled | enabled | yes | yes |
| `03-bare-block.conf` | enabled | not defined | enabled | enabled | yes | yes |
| `04-enabled-colon-no.conf` | disabled | disabled | disabled | disabled | yes | yes |
| `05-enabled-equals-no.conf` | disabled | disabled | disabled | disabled | yes | yes |
| `06-enabled-space-no.conf` | disabled | enabled | disabled | disabled | yes | yes |
| `07-enabled-space-yes-bare.conf` | enabled | not defined | enabled | enabled | yes | yes |
| `08-no-enabled-key.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `09-hash-comment-no.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `10-cblock-comment-no.conf` | enabled | disabled | enabled | enabled | yes | yes |
| `11-slash-linestart-no.conf` | enabled | disabled | enabled | disabled | no | no (`//`) |
| `12-slash-after-space-no.conf` | enabled | disabled | enabled | disabled | no | no (`//`) |
| `13-hash-whole-def.conf` | not defined | not defined | not defined | not defined | yes | yes |
| `14-cblock-whole-def.conf` | not defined | enabled | not defined | not defined | yes | yes |
| `15-slash-whole-def.conf` | not defined (pkg: parse error, file ignored) | enabled | not defined | enabled | no | no (`//`) |
| `16-url-https-trailing-slash-comment.conf` | enabled | enabled | enabled | enabled | yes | no (`//`) |
| `17-yes-then-commented-no-hash.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `18-yes-then-commented-no-cblock.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `19-yes-then-commented-no-slash.conf` | disabled | enabled | enabled | enabled | no | no (`//`) |
| `20-cblock-multiline-no.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `21-no-with-hash-yes.conf` | disabled | disabled | disabled | disabled | yes | yes |
| `22-no-with-cblock-yes.conf` | disabled | enabled | disabled | disabled | yes | yes |
| `23-no-with-slash-linestart-yes.conf` | disabled | enabled | disabled | enabled | no | no (`//`) |
| `24-no-with-slash-after-space-yes.conf` | disabled | enabled | disabled | enabled | no | no (`//`) |
| `25-dup-key-no-then-yes.conf` | disabled | disabled | disabled | disabled | yes | yes |
| `26-dup-key-yes-then-no.conf` | enabled | enabled | enabled | enabled | yes | yes |
| `27-two-blocks-yes-then-no.conf` | enabled | disabled | disabled | enabled | yes | yes |
| `28-two-blocks-no-then-yes.conf` | disabled | enabled | enabled | disabled | yes | yes |
| `29-slash-key-then-no.conf` | disabled | disabled | disabled | disabled | yes | no (`//`) |
| `30-two-blocks-second-sets-no.conf` | enabled | disabled | disabled | enabled | yes | yes |

## Findings

1. **pkg does not read `//` as a comment.** A file starting with `//`
   fails to parse (`key must begin with a letter`) and pkg ignores the whole
   file (15). libucl takes `//` as a key whose value is the rest of the
   line: inside a block a `//` line therefore hides what follows it on that
   line (11, 12, 23, 24, 29), but after a value on the same line it turns the
   value into a string, so `enabled: yes // enabled: no` reads as disabled
   (19). As the review asked, the `//` handling was removed from the PR;
   pkgrepo now has no notion of `//`, like main. It disagrees with pkg on
   the six `//` files (11, 12, 15, 19, 23, 24). The PR as reviewed, which
   removed `//` to the end of the line at a line start or after white
   space, matched pkg's results on all of them except 19, because pkg's
   "key" swallows the rest of the line much as a comment would. Whether to
   bring that back, knowing pkg does not call it a comment, is a question
   for Andy, recorded in docs/backlog.md.
2. **Within one file pkg keeps the first.** With two `enabled` keys in a
   block the first counts (25, 26), and with two blocks of the same name in
   one file only the first block counts, even for keys only the second sets
   (27, 28, 30). The first `enabled` already matched; main and the PR took
   the last block in a file, which disagreed on 27, 28 and 30. Fixed in the
   PR (three lines): only the first block of a name in each file counts.
   Layering across files (a later file overrides) is unchanged; langrishe's
   own `/usr/local/etc/pkg/repos/FreeBSD.conf` overriding
   `/etc/pkg/FreeBSD.conf` shows it.
3. **Every form the merged pkgrepo claims agrees with pkg**: `NAME: { }`,
   `NAME = { }`, `NAME { }`; `enabled` with `:`, `=` and a space; `#` and
   `/* */` comments, each hiding `enabled: no` or `enabled: yes` (inside a
   block, after a value, across lines, and around a whole definition); a
   url with `https://` and `${ABI}`; no `enabled` key (enabled by default).
   24 of 30 files agree; the six that do not are the `//` files above.
