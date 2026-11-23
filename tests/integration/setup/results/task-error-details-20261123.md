# Task error details, and the zvol fix on langrishe — 2026-11-23

Unattended run; Andy away; hearst off. Branch `feat/task-error-details`, package
from c9ecac9 (`make stage` on merrick, `pkg create` on langrishe), installed with
`pkg add -f` and `skep-setup upgrade -yes` (health ok). The build contains main's
zvol device-node fix (#55, amendment z), so Job 4d ran on it too. Logs and the
drivers (`j3drive.py`, `j4d_repair.py`, `zapi.py`, which pins the API's key from
skepctl's pin file) are in [task-error-details-20261123/](task-error-details-20261123/).

Before any VM start: `hw.vmm.vmx.cap.virtual_interrupt_delivery` = 0.

The skepctl pin in `~/skep-lab-creds` for langrishe was from before the
2026-11-22 23:40 UTC clean install: the certificate (valid until
2027-12-24T23:40:48Z, 397 days) dates the current key to that install. The
presented key was compared with `sudo skep-api tls fingerprint` over ssh
(equal, `sha256:dafb11e9…`) and re-pinned with `skepctl login --trust-key`.

## Job 3: a failed task's error carries its details

Test VM `j3-zvol` (256 MiB, one 64 MiB thin disk on `zroot`, no NIC).

| Step | Result |
|---|---|
| `zfs set volmode=none` on its disk | node gone |
| `vm.start` | **failed** `disk_device_missing`; `/dev/vmm` empty; volmode still `none` (Skep did not change it) |
| `GET /tasks/{id}` `error.details` | `{"command": "zfs set volmode=dev zroot/skep/vm/00304dd2…/disk0", "dataset": "zroot/skep/vm/00304dd2…/disk0", "reason": "volmode_none", "volmode": "none"}` |
| `GET /tasks` (same task) | the same `details` |
| flagged keys | none present (`error` is not recorded on this path; never exposed anyway) |
| the printed `command` run by hand | node back (`crw-r-----`) |
| `DELETE /vms/{id}` | succeeded; no dataset left |

## Job 4d: the zvol fix, live on langrishe (earlier only on smith)

**The real vm.start that repairs a missing node.** Test VM `j4d-repair` (same
shape). Renaming the zvol away and back (the smith method) never lost the node
in 30 tries on `zroot`, so the disk's zvol was destroyed and created again by hand
with the same local properties (`volmode=dev`, `skep:vm_id`, `skep:disk_id`,
same `volsize`, sparse): the node was missing after 1 s on the first try.

| Step | Result |
|---|---|
| `vm.start` with the node missing, `volmode=dev` | **succeeded** |
| agent log | `hostops: zvol device node repaired` (rounds 1, 66 ms), then `bhyve: repaired a disk's device node before the start` |
| after | node present; `bhyve: skep-a306ca7c…` running |
| `force_stop`, `DELETE` | succeeded; no dataset left; `/dev/vmm` empty |

**The `volmode=none` refusal** on langrishe: the Job 3 run above (refused,
nothing changed, the printed command restores the node).

End state of langrishe: Skep c9ecac9 (this branch) running, health ok; no VMs,
no jails, no test datasets; vmm, nmdm and the kld_list modules loaded;
edk2-bhyve installed.
