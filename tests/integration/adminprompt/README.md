# Password prompt integration test

`adminprompt-test` runs the real `skep-api admin create` and
`skep-api admin reset-password` on a pseudo-terminal, as an ordinary user
in a scratch directory, and types the password the way a person would
(ADR 0006 decision 8). It also runs `skep-api admin hash-benchmark` to
measure Argon2id on the host (decision 2).

| Part | What it checks |
|---|---|
| Prompt | Both prompts appear; `ECHO` is off in the terminal settings while the password is typed and on again afterwards; nothing the terminal showed contains the password |
| Result | The administrator exists; a second create of the same name is refused |
| Mismatch | Two different passwords change nothing, and echo is restored |
| Ctrl-C | Interrupting at the prompt stops the command and restores echo |
| `reset-password` | Same prompt behaviour; the password is replaced |
| No terminal | Without a controlling terminal the command refuses and names `--password-stdin`; it does not read the password from a pipe by accident |
| `--password-stdin` | Works; a password as an argument is not accepted |
| Timing | Ten Argon2id hashes with the default parameters; the mean must be within 100 to 500 ms |

## Run it

```sh
# On merrick
make build-freebsd build-freebsd-test
scp bin/freebsd-amd64/skep-api bin/freebsd-amd64-test/adminprompt-test hearst:skep-m4/

# On hearst, as an ordinary user
~/skep-m4/adminprompt-test -bin ~/skep-m4/skep-api
```

## Result

FreeBSD 15.1-RELEASE-p6 (hearst, 2 cores, 2 GB; 2026-11-12), `skep-api` at
`94042c7`, as the ordinary user `aba`: **PASS**, 24 checks. Excerpt:

```
ok     echo is off while the password is typed
ok     echo is off for the repeat
ok     the terminal never showed the password
ok     echo is back on afterwards
       the terminal showed: "New password for andy: \r\nRepeat the password: \r\nAdministrator \"andy\" created (user ID ...).\r\n"
ok     echo is back on after Ctrl-C
ok     refused, and it names --password-stdin: "skep-api: no terminal to read the password from: open /dev/tty: device not configured; ..."
ok     a password as an argument is not accepted
       argon2id, 64 MiB, 3 passes, 1 lane
       mean 130 ms, fastest 121 ms, slowest 151 ms
       Within the 100 to 500 ms target.
```

Argon2id on hearst: 121 to 151 ms per hash over three runs of ten (means
130, 135, 137 ms). The default of 3 passes stays. `/usr/bin/time -l` on a
run of four sequential hashes reported a maximum resident set size of
147 MiB: the Go runtime keeps the previous 64 MiB block until it is
collected, so the memory in use is above the nominal 64 MiB per hash.

Also checked by hand, over a real SSH session (`ssh -tt hearst`, driven
from a pseudo-terminal on the development machine): both prompts appear,
the administrator is created, and nothing the SSH terminal showed contains
the password.

Not covered:

- The FreeBSD system console (`ttyv0`). The test and the SSH check both
  use pseudo-terminals, which share the terminal layer with the console
  but are not the console.
- The command started as root, giving up root for the `skep` account.
  That is checked by `rcd-test api`.
