# Peer credential probe

A manual integration probe for the mechanism ADR 0004 depends on: the agent
learns the UID of whoever connects to its Unix socket from the kernel
(`LOCAL_PEERCRED` / `struct xucred` on FreeBSD) and refuses UIDs that are
not allow-listed. It is a test tool and is not installed with Skep.

## Run it

```sh
# On merrick
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/peercred.test ./internal/peercred
GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/peercred-probe ./tests/integration/peercred
scp /tmp/peercred.test /tmp/peercred-probe hearst:/tmp/

# On hearst
/tmp/peercred.test -test.v
sudo /tmp/peercred-probe serve -socket DIR/agent.sock -mode 0666 -allow 0,1001 &
/tmp/peercred-probe dial -socket DIR/agent.sock
sudo -u nobody /tmp/peercred-probe dial -socket DIR/agent.sock
```

## Result on FreeBSD 15.1-RELEASE-p6 (hearst, 2026-11-11)

Native unit test: passed as `aba` and as `root`.

UID allow-list, server running as root, socket mode `0666` so that only the
kernel-reported UID decides, allow-list `0,1001`:

```
connection: kernel reports uid=1001 gid=1001 -> allowed
connection: kernel reports uid=0 gid=0 -> allowed
connection: kernel reports uid=65534 gid=65534 -> denied
```

Filesystem permissions, directory `0750 root:wheel`, socket `0660 root:wheel`
(`aba` is in `wheel`, `nobody` is not):

```
client uid=1001: server replied allowed
peercred: dial unix .../agent.sock: connect: permission denied     (as nobody)
```

Both checks in ADR 0004 behave as designed: the filesystem refuses a user
outside the socket's group before a connection exists, and the kernel UID
check refuses a user who does get a connection.

Not covered: a dedicated `skep` account and group (not created on the lab
VM), a socket under `/var/run`, and behaviour across a reboot.
