# The web interface

Skep's web interface is served by `skep-api` on the same address as the
API, from files embedded in the binary
([ADR 0008](../decisions/0008-web-ui.md)). It is a client of `/api/v1` and
nothing more: everything it shows or does goes through the API with the
same login, permissions, and audit log as `skepctl`. There is no separate
web server, no separate port, and no setting to turn on.

## Opening it

With the services running ([the service guide](../installation/freebsd-service.md)),
open `https://` plus `skep-api`'s listen address in a browser. By default
that address is loopback (`https://127.0.0.1:8443/`) and the interface is
reachable only on the host itself; to reach it from the management network,
set `listen_address` in `api.json` first ("Opening the API to the network"
in the service guide).

There are no default credentials. Log in with the administrator created by
`skep-api admin create`; until one exists the login page says so.

### The first visit: the certificate warning

Unless an operator-supplied certificate is configured, `skep-api` serves a
self-signed certificate, and the browser warns that the site is not
trusted. The interface cannot help with that warning: it is not loaded
until you accept it. Before accepting, check that the certificate is the
one `skep-api` serves and not someone else's:

1. On the host, as root or `skep`, print the public key pin:

   ```sh
   skep-api tls fingerprint
   ```

2. In the browser's warning page, open the certificate details and find
   the SHA-256 of the public key, or compare the certificate's SHA-256
   fingerprint with the certificate on the host:

   ```sh
   openssl x509 -in /var/db/skep/api/tls/cert.pem -noout -fingerprint -sha256
   ```

   Browsers differ in what they show. Firefox shows both the certificate
   fingerprint and the public key under "View certificate"; Safari and
   Chrome show the certificate fingerprint ("Show certificate" or "Details"
   in the warning page). Either value, compared with the host, proves the
   same thing: that you are talking to this `skep-api`.

3. If the values match, accept the certificate (Safari: "Visit this
   website"; Chrome: "Advanced", then "Proceed"; Firefox: "Accept the risk
   and continue"). The browser remembers the exception for this address.

The Settings page shows the same pin and expiry for reference. That cannot
prove anything by itself: a man in the middle would serve its own page with
its own values. Compare on the host.

To avoid the warning for every browser on the network, install an operator
certificate from your own CA (`tls.cert_file` and `tls.key_file` in
`api.json`). With an operator certificate `skep-api` also sends
`Strict-Transport-Security`.

## What it does in this release

The twelve sections of the product are in the navigation. Only those the
API can fill have content; the others say which milestone brings them and
show no data.

| Section | Content |
|---|---|
| Dashboard | The node, its platform and version, uptime, every health check (degraded ones highlighted), capabilities, the five newest tasks |
| Nodes | The one node: processor, memory, bhyve, QEMU, OpenZFS pools, discovery warnings |
| Virtual machines | The VM list (state, drift, provisioning); create (one disk, install media, one NIC); a VM's details, power actions, media attach and detach, delete, its serial console, and its snapshots (take, roll back, delete, clone, with the note that a clone is the same guest). See [Virtual machines](virtual-machines.md) |
| Containers | The jail list (state, template, addressing); create from a built template (name, hostname, NICs with DHCP or a static IPv4 address and gateway, nameservers when no NIC uses DHCP, root's SSH key (only for a template with `has_sshd`), the System V IPC and lock-memory options, the limits); a jail's details, start, stop, restart, edit (a stopped jail, `container:write`: hostname, options, and limits at any time; before the first start also NICs, nameservers, and the SSH key; after it only each NIC's bridge), delete, the task's progress and its error with a hint, the console log, its limits, its current use from `/metrics`, and its snapshots (take, roll back with the confirmation to destroy newer ones, delete) and clones. Each limit says what it does on FreeBSD: memory is soft (a resident-memory target, not a cap), CPU is approximate, the file-table limit counts descriptor-table slots, locked memory is what `mlock` may lock. Without RACCT (`containers.rctl` false in `GET /system`) the page says so and the RCTL limits are disabled. A clone of a jail with a static address asks for new addressing; one that has started cannot be cloned, and the page says why. See [Jails](jails.md) |
| Storage | Each allow-listed pool with its capacity against the `storage_capacity` thresholds and any locked datasets; the current disk, CPU, and memory use of each VM; the dataset inventory with owners. See [Storage](storage.md) |
| Tasks (under Monitoring) | Task list with a status filter and paging; task detail with its events; cancel (`task:cancel`) |
| Users and permissions | Users (create, edit, enable and disable, set password, delete), roles and their permissions, the access audit log |
| Settings | About this node: version and build, the API certificate, session lifetimes, the theme |
| Account menu | Your account: identity, the session in use, change your password, log out |

Entries the role cannot read are hidden. That is a convenience: the server
decides, and a page reached by its address shows "Your role does not allow
this" instead of calling the API.

Changing a user's role or password, disabling, and deleting each end that
user's sessions, and the confirmation says so. Deleting asks for the
username to be typed. The last enabled administrator cannot be disabled,
deleted, or given another role; the refusal is shown as such. Changing your
own password ends every session of your account, this one included, and
returns you to the login page.

## Sessions

The interface uses a cookie session (`__Host-skep_session`; `Secure`,
`HttpOnly`, `SameSite=Strict`). The token never reaches the page's
JavaScript and nothing about the session is kept in the browser's storage;
the only thing stored is the theme choice.

Sessions end after 30 minutes without activity and 12 hours after the
login (`auth.session` in `api.json`). The interface refreshes its pages by
itself, but those refreshes do not count as activity: they carry the
`X-Skep-Background` header, which `skep-api` authenticates like any request
and does not count as use. An unattended page therefore logs itself out.
Two minutes before the idle timeout a notice offers "Stay logged in"; five
minutes before the absolute limit it says the session cannot be extended.

When the session has ended, for whatever reason (timeout, a password or
role change, an administrator disabling the account, a logout in another
tab), the login page appears at the address you were at, says why, and
returns you there after the login. A form that was being submitted when
that happened is not resubmitted: it is shown again with what was entered,
except passwords.

## Development

See [web/README.md](../../web/README.md): the interface is built with
Node.js, served by Vite over HTTPS with the development certificate while
working on it, and embedded in `skep-api` by `make build` and
`make build-freebsd`. A `skep-api` built without it (`go build ./...`,
`make build-go`) serves a page that says so; the API is unaffected.
