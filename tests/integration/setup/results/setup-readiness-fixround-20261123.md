# Setup readiness (#56): Fable review fix round, live on smith and langrishe — 2026-11-23

Unattended run; Andy away; hearst off. Both VMs ran this branch's build
`0.1.0+2c3040c`, installed with `pkg add -f` and `skep-setup upgrade -yes`
(health ok, agent socket answered as the new build, exit 0 on both). No
guests on either. Logs and scripts in
[setup-readiness-fixround-20261123/](setup-readiness-fixround-20261123/).

## Part 1: FreeBSD-base, setup and the agent

`-root` defaults to `/`, and `ValidateOptions` refuses an empty root before
anything is read, so `os.DirFS("")` is never reached (tests:
`TestRootDefault`, `TestBaseRepoEnabledRoot`).

| Host | `skep-setup plan -mgmt vtnet0` | `GET /api/v1/storage/template-sources` |
|---|---|---|
| langrishe (`/usr/local/etc/pkg/repos/FreeBSD.conf` enables it) | `ok packages FreeBSD-base repository: enabled` | `"pkgbase":{"available":true}` |
| smith (stock files) | `manual … disabled`, `$ mkdir -p /usr/local/etc/pkg/repos && printf 'FreeBSD-base: { enabled: yes }\n' >> /usr/local/etc/pkg/repos/FreeBSD.conf` | `available:false`, "the FreeBSD-base repository is not enabled (/etc/pkg/FreeBSD.conf); enable it with mkdir -p … , or use the dist source" |

## Part 2: `ifconfig -g bridge` with no bridges (smith)

`ifconfig -l`: `vtnet0 vtnet1 lo0`. `ifconfig -g bridge`: **exit 0**, 0
bytes on stdout, 0 on stderr. So any error from it now fails the probe,
like a failed `ifconfig BRIDGE` (`readBridges`, `TestReadBridges`).
`skep-setup check` probed normally (exit 1, pending items).

## Part 3: steps for a bridge without vlanfilter (smith)

`bridge9` created on `vtnet1` with a hand-made `epair0a` as a second member
(in place of a jail's guest port; smith has no template). The real
`/etc/rc.conf` was not edited: the rc.conf variants were appended to a copy
of `/etc` in a `0700` root under `/tmp`, used with `-root`.

| Case | Steps printed |
|---|---|
| (b)+(c) run-time bridge, real `/` | live steps for `epair0a` and `vtnet1`, `defuntagged 1`, `vlanfilter`; `sysrc cloned_interfaces+="bridge9"`; `sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`; detail "The rc.conf line leaves out [epair0a]" |
| (b) `cloned_interfaces="bridge9"` already | the same without the `cloned_interfaces` step |
| (c) `ifconfig_bridge9="addm vtnet1 up"`, `epair0a` live | live steps for both; the line names only `vtnet1` |
| (a) line has `vlanfilter`, off now | own item "in ifconfig_bridge9 in rc.conf but off now: it takes effect at the next boot, or run these steps … rc.conf needs no change"; live steps only, no `sysrc` |

Cleanup: `epair0a` and `bridge9` destroyed, `vtnet1` flags as before
(`1008802`, not UP), `ifconfig -l` as before, the copied root removed,
`/etc/rc.conf` sha256 `b8c48f27…6e39` before and after.

## Part 4: the printed command, run as written (smith)

Before: no `/usr/local/etc/pkg`. The command taken from `skep-setup check`
output and run with `sh -c`: exit 0; `pkg -vv` then `enabled: yes`;
`skep-setup plan`: `ok … enabled`; agent: `"pkgbase":{"available":true}`.
Restored by removing the file and both directories (after checking they
held nothing else): no `/usr/local/etc/pkg`, `pkg -vv` `enabled: no`,
setup `disabled`, agent `available:false`.

End state: both VMs on `0.1.0+2c3040c`, health ok, no guests, no bridges.
