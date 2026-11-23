# Setup stack, second fix round: B1 and B2 live on smith — 2026-11-23

Unattended; hearst off. `skep-setup` from this branch, run from a `0700`
root directory on smith. Logs and scripts in
[setup-fixround2-20261123/](setup-fixround2-20261123/).

## B1: a bridge with a live guest port

`bridge9` on `vtnet1` with a hand-made `epair0a` as stand-in guest port.
`plan` printed no live step and no `sysrc` step, and said to stop the Skep
guests on this bridge first and run plan again.

## B2: the printed steps, run as printed

**What was wrong.** The first run's steps all exited 0 and `plan` reported
the bridge ok, but `vtnet1` stayed down: `addm` does not bring a member up
and no step did. The bridge passed nothing (0 packets on `vtnet1` in 90 s),
and the `rc.conf` lines would have left it down after a boot too. The steps
now include `ifconfig MEMBER up` after `vlanfilter`, and
`sysrc ifconfig_UPLINK="up"` for an uplink with no `ifconfig_` line. An
existing line is never replaced. Unit-tested in `TestPlanVLANFilterSteps`.

With the fixed steps:

| Check | Result |
|---|---|
| Every printed step, run with `sh -c` | exit 0 |
| Before the reboot | `VLANFILTER`, `defuntagged=1`, `vtnet1` PVID 1 and UP, one address learned from the uplink |
| `hw.vmm.vmx.cap.virtual_interrupt_delivery` after the reboot, read first | 0 |
| After the one reboot | `bridge9` back with member `vtnet1`, PVID 1, `VLANFILTER`, `vtnet1` UP; `plan` reports both items ok |
| Cleanup | bridge removed, `vtnet1` flags as before, `/etc/rc.conf` sha256 `b8c48f27…6e39` as before |

**Limit.** The link was quiet: no address was learned in the 30 s after the
reboot, so forwarding was seen only before it. After the reboot the
configuration and link state were checked, not traffic.
