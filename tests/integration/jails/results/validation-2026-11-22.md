# hearst validation, 2026-11-22 (runbook `tests/integration/validation-2026-11-22.md`)

Run unattended by Claude Code on 2026-11-22 (~15:02–15:12Z), after Andy
took the Proxmox snapshot `pre-deploy-20261122` and rotated the test
passwords (step 1). Step 5 (kld_list, bridge0 `vlanfilter`, reboot) was
approved by Andy for Claude Code to do this one time. Raw outputs are in
[validation-2026-11-22/](validation-2026-11-22/) (file names follow the
runbook's step numbers).

Host: `hearst`, FreeBSD 15.1-RELEASE-p7, deployed main **dd7a8b6**.

| Step | What | Result |
|---|---|---|
| 1 | Logins of the three test users after rotation | 200 each (`01-logins.txt`) |
| 2 | Backups, deploy main (b61e578 → dd7a8b6) | **Pass**: both services verified at dd7a8b6, `agent.json` unchanged (`490bc087…`), `migrations_applied: 0`, health ok (`02-*`) |
| 3 | C1: VM NIC with a MAC held by a jail / a VM | **Pass**: 409 `vm_mac_in_use`, `held_by_kind` `container` / `vm`, names right; no task created (633 633 633); two `denied` audit entries (`03*`) |
| 4 | C4: nameservers on a NIC-less jail at create, update, clone | **Pass**: 422 `nameservers_require_nic` three times, no task for create and clone, control clone succeeds (`04*`) |
| 4b | SR-07: two jails with 192.0.2.77/24 on bridge0, started at once | **Pass**: one runs; the other's task failed with `container_address_in_use` ("the starting jail rb-sr07-b holds 192.0.2.77/24 on bridge0"), refused at `MarkStarting` while the first was starting; `started_once` 0 (`04b-*`) |
| 5 | `kld_list`, bridge0 VLAN filtering, reboot | Applied in the upgrade-doc order after checking management is on `vtnet0` and not a bridge0 member (`05-apply.txt`); back in under a minute |
| 5 (after) | Before any guest: tunable, RACCT, modules, bridge, services, health | **Pass**: `virtual_interrupt_delivery` 0, `kern.racct.enable` 1, vmm if_epair netgraph ng_socket ng_ether ng_bpf loaded, `VLANFILTER defuntagged=1`, vtnet1 untagged 1, both services at dd7a8b6, health ok (`05-after-reboot.txt`, `05-health.json`) |
| 6 | M7c item 10: the first guest start after the reboot is filtered | **Pass**: a jail and a VM started; `skf_epair0a` and `skf_tap0` between each guest port and the bridge, each with its program on the `jail` hook; both ports PVID 1; `port_filters` ok; no `port_filter.*` audit entry since the reboot (`06*`) |
| 7 | Clean up | All test guests deleted; no epair, tap, `skf_` node, or bhyve left; health ok (`07-*`) |

`rc.conf` on hearst, before and after step 5:

```diff
-ifconfig_bridge0="addm vtnet1 up"
+ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"
+kld_list="if_epair netgraph ng_socket ng_ether ng_bpf"
```

Notes:

- Task errors carry `code` and `message` only (no `details` object), so in
  step 4b the holder's name, address, and bridge are in the message.
  `api.py`'s `--wait` summary reads `error_code` and prints null; the task
  itself (`04b-2-a-task.json`) has the code.
- Step 6 used new guests (`rb-i10-*`): the step-3 guests had already been
  deleted when steps 2–4 were cleaned up.
- A first `stop` with `"force": true` was refused (400, not a field); the
  action is `force_stop`.

hearst afterwards: main dd7a8b6, `kld_list` and the bridge0 line
persistent, backup `/var/tmp/skep-backup-20261122T150343Z` kept until Andy
agrees the deploy stays.
