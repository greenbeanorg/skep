# "route: ... File exists" at every jail start (backlog item, 2026-11-22)

Backlog item from step 10 run 3 (`step10-progress.md` section 26.4
item 10; ADR 0010 amendment w backlog), first noted in the step 7 probe
(`step7-probe.txt` line 34).

## The observation

Every jail start's console log (`GET /api/v1/containers/{id}/console-log`)
has, right after `Starting Network: lo0.` and `ifconfig lo0`'s output
(`step10-hearst-run3.txt` lines 834–837, `step8-probe.txt` lines 64–66):

```
route: message indicates error: File exists
add host 127.0.0.1: gateway lo0 fib 0: route already in table
route: message indicates error: File exists
add host ::1: gateway lo0 fib 0: route already in table
```

## Cause (confirmed by reading hearst's FreeBSD 15.1-RELEASE-p7 base, read-only)

The jail runs the template's stock `/etc/rc` (Skep writes only
`rc.conf` lines for its NICs, `resolv.conf`, and root's SSH key before the
first start). In a VNET jail, `/etc/rc.d/netif` brings up the jail's own
`lo0` with `127.0.0.1/8` and `::1`; the kernel then installs the host
routes for those addresses itself (`netstat -rn` on any host shows
`127.0.0.1  link#N  UH  lo0`). `/etc/rc.d/routing` runs afterwards and adds
them again, unconditionally:

- `static_inet()`: `static_routes="_loopback ${static_routes}"`,
  `route__loopback="-inet 127.0.0.1 -iface lo0 ${_fibmod}"`
  (`/etc/rc.d/routing` line 151–152);
- `static_inet6()`: `ipv6_route__loopback="::1 -prefixlen 128 -iface lo0 ${fibmod}"`
  (line 221).

`route add` gets `EEXIST` from the routing socket and prints
`route: message indicates error: File exists` and
`add host …: route already in table` (`sbin/route/route.c` line 1134).
The route that exists is the right one, so loopback works; the remaining
IPv6 reject routes (`fe80::`, `ff02::`) are added normally, as the next
lines of the console log show.

Nothing about this is specific to Skep: any VNET jail running the stock
rc prints it, and so may a host whose rc output is visible.

## Options

| Option | Effect | Verdict |
|---|---|---|
| A. Document it (this change) | `jails.md`, "The console log", shows the four lines and says they are harmless | Done |
| B. Filter the lines from the console log | Hides base-system output in a log whose content is the jail's | Rejected: Skep never edits rc's output (only the API's display cleanup of control characters, amendment r) |
| C. Write something into the jail's `rc.conf` to skip the routes | No knob exists: `_loopback` is prepended unconditionally | Not possible without editing the jail's `/etc/rc.d/routing`, which Skep does not do |
| D. Upstream: make `rc.d/routing` skip a loopback route that exists (or add it with `route -q`) | Removes the noise for every VNET jail | A FreeBSD problem report, if Andy wants one |

**Recommendation:** A (done), and optionally D. No Skep code change.

**Decided (Andy, 2026-11-22):** A; no FreeBSD problem report (D) for now.

Pending FreeBSD validation: nothing new to validate; the lines and the
script were read on hearst (read-only) in this run, and the jail-start
logs above are from earlier runs.
