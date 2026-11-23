# Setup readiness (#56): the firmware warning, live on smith — 2026-11-23

Unattended run; Andy away; hearst off. smith ran the `chore/m10-backlog` build
571f7b2, which contains this branch's readiness checks unchanged (#58 is
stacked on #56). No guests. Answers `-mgmt vtnet0 -pool tank`. Log in
[setup-readiness-firmware-20261123/](setup-readiness-firmware-20261123/).

| Step | Result |
|---|---|
| `pkg delete -y edk2-bhyve` (g202508_3) | removed; `/usr/local/share/edk2-bhyve` gone |
| `skep-setup check` | **warn** packages edk2-bhyve: "the bhyve UEFI firmware package edk2-bhyve is not installed (BHYVE_UEFI.fd, BHYVE_UEFI_VARS.fd missing in /usr/local/share/edk2-bhyve): VMs cannot be created without it; jails do not need it. Setup does not install it", `$ pkg install edk2-bhyve`; 1 warning; exit 1 (other pending items) |
| `skep-setup plan` | the same warning; exit 0 |
| `pkg install -y edk2-bhyve` | g202508_3 from FreeBSD-ports; firmware files back |
| `skep-setup check` | no firmware warning; 0 warnings |

End state: edk2-bhyve installed on smith, as before.
