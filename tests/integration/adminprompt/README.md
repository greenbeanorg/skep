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

**Pending FreeBSD validation.** This section is replaced by the recorded
run.
